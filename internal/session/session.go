package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	ws "term-gateway/internal/websocket"

	"github.com/gorilla/websocket"
	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	podReadyTimeout   = 2 * time.Minute
	containerName     = "sandbox"
	maxMessageBytes   = 64 * 1024 // 64KB max message size
	readBufLimit      = 32 * 1024
	websocketReadWait = 60 * time.Second
	// WebsocketWriteWait = 10 * time.Second
	pingPeriod = 30 * time.Second
)

func boolPtr(b bool) *bool { return &b }

var schemeParameterCodec = scheme.ParameterCodec

func restParameterCodec() runtime.ParameterCodec {
	return schemeParameterCodec
}

func writeWSClose(conn *websocket.Conn, code int, reason string) {
	// Construct a WebSocket close frame
	msg := websocket.FormatCloseMessage(code, reason)
	_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
	_ = conn.Close()
}

func sandboxPodSpec() *v1.Pod {
	return &v1.Pod{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: "term-pty-",
			Labels:       map[string]string{"app": "term-gw"},
		},
		Spec: v1.PodSpec{
			// set a pre-created, least-privileged SA; or create per-session SA/RB on demand
			// ServiceAccountName: "sa-session",
			AutomountServiceAccountToken: func(b bool) *bool { return &b }(true),
			RestartPolicy:                v1.RestartPolicyNever,
			Containers: []v1.Container{{
				Name:            "sandbox",
				Image:           "bitnami/kubectl:latest", // use a minimal image with kubectl installed
				ImagePullPolicy: v1.PullIfNotPresent,
				Command:         []string{"/bin/sh", "-lc", "sleep 31536000"}, // long sleep; shell spawned by exec/attach
				Stdin:           true,
				StdinOnce:       false,
				TTY:             true,
				SecurityContext: &v1.SecurityContext{
					RunAsNonRoot:             boolPtr(true),
					AllowPrivilegeEscalation: boolPtr(false),
					Privileged:               boolPtr(false),
					ReadOnlyRootFilesystem:   boolPtr(true),
					Capabilities:             &v1.Capabilities{Drop: []v1.Capability{"ALL"}},
					SeccompProfile:           &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
				},
				VolumeMounts: []v1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
				Resources:    v1.ResourceRequirements{
					// tighten per your needs
				},
			}},
			Volumes: []v1.Volume{{
				Name: "tmp", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{Medium: v1.StorageMediumMemory}},
			}},
		},
	}
}

func (s *Server) ensureNamespaceExists(ctx context.Context, ns string) error {
	_, err := s.Core.Namespaces().Get(ctx, ns, meta.GetOptions{})
	if err == nil {
		return nil
	}

	_, err = s.Core.Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: meta.ObjectMeta{
			Name: ns,
		},
	}, meta.CreateOptions{})
	if err != nil {
		return err
	}

	return nil
}

func (s *Server) waitPodReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 1*time.Second, timeout, true, func(ctx context.Context) (done bool, err error) {
		p, err := s.Core.Pods(ns).Get(ctx, name, meta.GetOptions{})
		if err != nil {
			return false, nil
		}
		if p.Status.Phase == v1.PodRunning {
			// If any container is Ready, we’re good enough for a TTY
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name == containerName && cs.Ready {
					return true, nil
				}
			}
		}
		if p.Status.Phase == v1.PodFailed || p.Status.Phase == v1.PodSucceeded {
			return false, fmt.Errorf("pod finished with phase %s", p.Status.Phase)
		}
		return false, nil
	})
}

func (s *Server) CreateSession(ns string, req CreateReq) (CreateResp, error) {
	// Implementation for creating a session
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := s.ensureNamespaceExists(ctx, ns); err != nil {
		return CreateResp{}, err
	}

	// Create sandbox pod
	pod, err := s.Core.Pods(ns).Create(ctx, sandboxPodSpec(), meta.CreateOptions{})
	if err != nil {
		return CreateResp{}, err
	}

	// Wait until container is Ready (or at least PodRunning)
	if err := s.waitPodReady(ctx, ns, pod.Name, podReadyTimeout); err != nil {
		_ = s.Core.Pods(ns).Delete(context.Background(), pod.Name, meta.DeleteOptions{})
		return CreateResp{}, err
	}

	id := "123" // Generate a unique session ID (e.g., using UUID)
	s.Mu.Lock()
	s.Sessions[id] = &Session{
		ID:        id,
		Namespace: ns,
		PodName:   pod.Name,
		CreatedAt: time.Now(),
		Cancel:    cancel,
	}
	s.Mu.Unlock()

	ws := fmt.Sprintf("%s/sessions/%s/attach", strings.TrimRight(s.BaseURL, "/"), id)
	resp := CreateResp{SessionID: id, WSURL: ws, Pod: pod.Name, Namespace: ns}

	return resp, nil
}

func (s *Server) deleteSessionByID(id string) {
	s.Mu.Lock()
	ss, ok := s.Sessions[id]
	if ok {
		delete(s.Sessions, id)
	}
	s.Mu.Unlock()
	if ok {
		ss.Cancel()
		_ = s.Core.Pods(ss.Namespace).Delete(context.Background(), ss.PodName, meta.DeleteOptions{})
	}
}

func (s *Server) WsAttach(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Session ID is required", http.StatusBadRequest)
		return
	}

	s.Mu.RLock()
	ss, ok := s.Sessions[id]
	s.Mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	conn, err := s.Upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Failed to upgrade connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// Configure websocket
	conn.SetReadLimit(maxMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(websocketReadWait))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(websocketReadWait))
		return nil
	})

	// Heartbeat
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for range t.C {
			_ = conn.SetWriteDeadline(time.Now().Add(ws.WebsocketWriteWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}()

	// Kube exec → set up command and streams
	req := s.Kube.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(ss.PodName).
		Namespace(ss.Namespace).
		SubResource("exec")
	// Shell entrypoint; swap to bash if present
	cmd := []string{"/bin/sh", "-lc", "exec /bin/bash || exec /bin/sh"}
	req.VersionedParams(&v1.PodExecOptions{
		Container: containerName,
		Command:   cmd,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, restParameterCodec())

	exec, err := remotecommand.NewSPDYExecutor(s.RestCfg, http.MethodPost, req.URL())
	if err != nil {
		writeWSClose(conn, websocket.CloseInternalServerErr, "executor: "+err.Error())
		return
	}

	wsrw := ws.NewWSReadWriter(conn)
	defer wsrw.Close()

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Close session if websocket dies
	go func() {
		for {
			// Consume control frames to detect closure
			_, _, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Start exec stream
	err = exec.StreamWithContext(streamCtx, remotecommand.StreamOptions{
		Stdin:             wsrw,
		Stdout:            wsrw,
		Stderr:            wsrw, // merged; we’re TTY
		Tty:               true,
		TerminalSizeQueue: wsrw, // listens for resize JSON msgs
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[exec error] "+err.Error()+"\r\n"))
	}

	// Cleanup pod when stream ends
	go func() {
		s.deleteSessionByID(id)
	}()
}
