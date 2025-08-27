package websocket

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	WebsocketWriteWait = 10 * time.Second
)

func NewWSReadWriter(c *websocket.Conn, wmu *sync.Mutex) *WsReadWriter {
	return &WsReadWriter{Conn: c, Resize: make(chan remotecommand.TerminalSize, 8), WMu: wmu}
}

func (w *WsReadWriter) Read(p []byte) (int, error) {
	for {
		// w.RMu.Lock()
		if w.R != nil {
			n, err := w.R.Read(p)
			if err == io.EOF {
				w.R = nil
				// w.RMu.Unlock()
				continue
			}
			// w.RMu.Unlock()
			return n, err
		}
		// w.RMu.Unlock()

		mt, msg, err := w.Conn.ReadMessage()
		if err != nil {
			return 0, err
		}
		switch mt {
		case websocket.TextMessage:
			// 1) Check for resize directives FIRST
			text := string(msg)
			if strings.HasPrefix(text, "RESIZE ") {
				var cols, rows int
				fmt.Sscanf(text, "RESIZE %dx%d", &cols, &rows)
				if cols > 0 && rows > 0 {
					// non-blocking send to avoid deadlock if reader is slow
					select {
					case w.Resize <- remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}:
					default:
					}
					continue
				}
			}
			// JSON resize: {"type":"resize","cols":120,"rows":30}
			if bytes.Contains(msg, []byte(`"type":"resize"`)) {
				var obj struct {
					Type string `json:"type"`
					Cols uint16 `json:"cols"`
					Rows uint16 `json:"rows"`
				}
				if json.Unmarshal(msg, &obj) == nil && obj.Cols > 0 && obj.Rows > 0 {
					select {
					case w.Resize <- remotecommand.TerminalSize{Width: obj.Cols, Height: obj.Rows}:
					default:
					}
					continue
				}
			}

			// w.RMu.Lock()
			w.R = strings.NewReader(string(msg))
			// w.RMu.Unlock()

		case websocket.BinaryMessage:
			// w.RMu.Lock()
			w.R = bytes.NewReader(msg)
			// w.RMu.Unlock()
		case websocket.CloseMessage:
			return 0, io.EOF
		case websocket.PongMessage:
			// ignore
		default:
			// Simple protocol: handle resize messages prefixed with "RESIZE WxH", or JSON: {"type":"resize","cols":120,"rows":30}
			text := string(msg)
			if strings.HasPrefix(text, "RESIZE ") {
				var cols, rows int
				fmt.Sscanf(text, "RESIZE %dx%d", &cols, &rows)
				w.Resize <- remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}
			} else if strings.Contains(text, "\"type\":\"resize\"") {
				var obj struct {
					Type       string
					Cols, Rows uint16
				}
				_ = json.Unmarshal(msg, &obj)
				if obj.Cols > 0 && obj.Rows > 0 {
					w.Resize <- remotecommand.TerminalSize{Width: obj.Cols, Height: obj.Rows}
				}
			}
		}
	}
}

func (w *WsReadWriter) Write(p []byte) (int, error) {
	w.WMu.Lock()
	defer w.WMu.Unlock()

	_ = w.Conn.SetWriteDeadline(time.Now().Add(WebsocketWriteWait))
	if err := w.Conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *WsReadWriter) Next() *remotecommand.TerminalSize {
	// select {
	// case sz := <-w.Resize:
	// 	return &sz
	// default:
	// 	return nil
	// }
	sz, ok := <-w.Resize
	if !ok {
		return nil
	}
	return &sz
}

func (w *WsReadWriter) Close() error {
	close(w.Resize)
	return w.Conn.Close()
}
