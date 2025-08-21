package session

import (
	"context"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type Session struct {
	ID        string
	Namespace string
	PodName   string
	CreatedAt time.Time
	Cancel    context.CancelFunc
}

type Server struct {
	Kube     *kubernetes.Clientset
	RestCfg  *rest.Config
	Core     typedcore.CoreV1Interface
	Sessions map[string]*Session
	Mu       sync.RWMutex
	Upgrader websocket.Upgrader
	BaseURL  string
}

type CreateReq struct {
	Namespace string `json:"namespace"`
	TTL       int    `json:"ttl"`
}

type CreateResp struct {
	SessionID string `json:"sessionId"`
	WSURL     string `json:"wsUrl"`
	Pod       string `json:"podName"`
	Namespace string `json:"namespace"`
}
