package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"term-gateway/internal/session"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	readBufLimit = 32 * 1024 // 32KB read buffer limit
)

func env(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func encode[T any](w http.ResponseWriter, r *http.Request, status int, v T) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

func decode[T any](r *http.Request) (T, error) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		return v, fmt.Errorf("decode json: %w", err)
	}
	return v, nil
}

func mustKube() (*rest.Config, *kubernetes.Clientset, error) {
	// Try in-cluster, fallback to $KUBECONFIG / ~/.kube/config
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, _ := os.UserHomeDir()
			kubeconfig = home + "/.kube/config"
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, nil, err
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cfg, cs, nil
}

func handleCreateSession(s *session.Server) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var req session.CreateReq
			var err error

			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			req, err = decode[session.CreateReq](r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer r.Body.Close()

			res, err := s.CreateSession(req.Namespace, req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			if err := encode(w, r, http.StatusCreated, res); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		},
	)
}

func addRoutes(
	mux *http.ServeMux,
	s *session.Server,
) {
	// Here you would typically set up your routes, e.g.:
	// http.HandleFunc("/example", s.exampleHandler)
	mux.Handle("/sessions", handleCreateSession(s))
	// mux.HandleFunc("/sessions/{id}", s.getSession).Methods("GET")
	// mux.HandleFunc("/sessions/{id}", s.deleteSession).Methods("DELETE")
	mux.HandleFunc("/sessions/{id}/attach", s.WsAttach)
}

// NewServer creates a new HTTP server with the specified routes and middleware.
func NewServerMux(s *session.Server) http.Handler {
	mux := http.NewServeMux()

	// Add your routes to the mux
	addRoutes(mux, s)

	// You can add middleware here, such as logging or recovery middleware.
	// For example, you might use a logging middleware like this:
	// mux.Handle("/", loggingMiddleware(http.HandlerFunc(yourHandler)))
	var handler http.Handler = mux
	handler = http.TimeoutHandler(handler, 30*time.Second, "Request timed out")

	return handler
}

func run(ctx context.Context, w io.Writer, args []string) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	cfg, cs, err := mustKube()
	if err != nil {
		return fmt.Errorf("failed to create kube client: %w", err)
	}

	s := &session.Server{
		Kube:     cs,
		RestCfg:  cfg,
		Core:     cs.CoreV1(),
		Sessions: make(map[string]*session.Session),
		Mu:       sync.RWMutex{},
		Upgrader: websocket.Upgrader{
			ReadBufferSize:  readBufLimit,
			WriteBufferSize: readBufLimit,
		},
		BaseURL: env("PUBLIC_BASE_URL", "http://localhost:8080"),
	}

	srvMux := NewServerMux(s)

	httpServer := &http.Server{
		Addr:    net.JoinHostPort("", "8080"),
		Handler: srvMux,
	}

	go func() {
		log.Printf("listening on %s\n", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "error listening and serving: %s\n", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		shutdownCtx := context.Background()
		shutdownCtx, cancel := context.WithTimeout(shutdownCtx, 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "error shutting down http server: %s\n", err)
		}
	}()
	wg.Wait()
	return nil
}

func main() {
	ctx := context.Background()
	if err := run(ctx, os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
