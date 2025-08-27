package websocket

import (
	"io"
	"sync"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"
)

type WsReadWriter struct {
	Conn   *websocket.Conn
	R      io.Reader
	WMu    *sync.Mutex
	Closed int32
	Resize chan remotecommand.TerminalSize
}
