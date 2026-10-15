package lobsteraiserver

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// CallbackPath is the loopback path the backend redirects the browser to. It
// matches the desktop client's local callback server exactly
// (http://127.0.0.1:<port>/auth/callback?code=…&state=…).
const CallbackPath = "/auth/callback"

// OAuthResult carries the authorization code from the browser callback.
type OAuthResult struct {
	Code  string
	State string
	Error string
}

// OAuthServer handles the local loopback HTTP callback for the login flow.
//
// The backend redirects to a fixed loopback path carrying code+state. An
// ephemeral port is supported (Start returns the bound port) because the
// redirect URI is built from the listener address.
type OAuthServer struct {
	mu       sync.Mutex
	port     int
	listener net.Listener
	server   *http.Server
	running  bool

	resultChan chan *OAuthResult
	errorChan  chan error
}

// NewOAuthServer creates a loopback callback server. A port of 0 selects an
// ephemeral port.
func NewOAuthServer(port int) *OAuthServer {
	return &OAuthServer{
		port:       port,
		resultChan: make(chan *OAuthResult, 1),
		errorChan:  make(chan error, 1),
	}
}

// Start binds the loopback listener and returns the bound port.
func (s *OAuthServer) Start() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return s.port, nil
	}

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, errLn := net.Listen("tcp", addr)
	if errLn != nil {
		return 0, fmt.Errorf("lobsterai-server: failed to bind local callback server to %s: %w", addr, errLn)
	}

	s.listener = ln
	s.port = ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, s.handleCallback)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CallbackPath {
			s.handleCallback(w, r)
			return
		}
		http.NotFound(w, r)
	})

	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	s.running = true
	go func() {
		if errServe := s.server.Serve(ln); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.Debugf("lobsterai-server oauth callback server error: %v", errServe)
			select {
			case s.errorChan <- errServe:
			default:
			}
		}
	}()

	return s.port, nil
}

// Port returns the bound port (0 before Start).
func (s *OAuthServer) Port() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// RedirectURI returns the loopback redirect URI for the bound listener.
func (s *OAuthServer) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.Port(), CallbackPath)
}

// Stop gracefully stops the server.
func (s *OAuthServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}
	s.running = false
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

// WaitForCallback waits for the browser redirect or times out.
func (s *OAuthServer) WaitForCallback(timeout time.Duration) (*OAuthResult, error) {
	return s.WaitForCallbackWithContext(context.Background(), timeout)
}

// WaitForCallbackWithContext waits for the browser redirect, context
// cancellation, or timeout.
func (s *OAuthServer) WaitForCallbackWithContext(ctx context.Context, timeout time.Duration) (*OAuthResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-s.resultChan:
		return res, nil
	case err := <-s.errorChan:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, errors.New("lobsterai-server authentication timed out")
	}
}

func (s *OAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	errStr := strings.TrimSpace(query.Get("error"))
	errDesc := strings.TrimSpace(query.Get("error_description"))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if errStr != "" || code == "" {
		message := errStr
		if errDesc != "" {
			message = fmt.Sprintf("%s: %s", errStr, errDesc)
		}
		if message == "" {
			message = "missing authorization code"
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(fmt.Sprintf(loginFailureHTML, html.EscapeString(message))))
		select {
		case s.resultChan <- &OAuthResult{Error: message}:
		default:
		}
		return
	}

	_, _ = w.Write([]byte(loginSuccessHTML))
	select {
	case s.resultChan <- &OAuthResult{Code: code, State: state}:
	default:
	}
}

const loginSuccessHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>LobsterAI 登录</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f7f7f4;color:#14120b;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#fff;border:1px solid rgba(20,18,11,.08);border-radius:10px;padding:30px 34px;max-width:420px}
h1{color:#16a34a;font-size:20px;margin:0 0 10px;font-weight:600}p{color:#666;font-size:14px;margin:0}</style></head>
<body><div class="card"><h1>登录成功</h1><p>已完成授权，请返回终端继续。</p></div></body></html>`

const loginFailureHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>LobsterAI 登录</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#f7f7f4;color:#14120b;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#fff;border:1px solid rgba(20,18,11,.08);border-radius:10px;padding:30px 34px;max-width:420px}
h1{color:#dc2626;font-size:20px;margin:0 0 10px;font-weight:600}p{color:#666;font-size:14px;margin:0}</style></head>
<body><div class="card"><h1>登录失败</h1><p>%s</p></div></body></html>`
