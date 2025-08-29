# POC for terminal session service

- web server with endpoints
  - POST /session
    - Creates a Kubernetes pod running a shell.
    - Stores an in-memory mapping of sessionId → {namespace, podName, wsUrl, podSpec}.
    - Returns JSON containing the sessionId and the wsUrl to attach.
  - GET /sessions/{id}/attach (websocket url returned by the above endpoint
    - Upgrades the HTTP connection to a WebSocket.
    - Bridges the WebSocket to a kubectl exec session inside the pod (stdin/stdout/stderr + TTY).
    - Supports resize messages and heartbeat pings.
    - When the WebSocket closes, the exec stream ends and the pod/session is cleaned up.

## To do:
- Atlas integration
- Clean up logic
- Authn/z story?
