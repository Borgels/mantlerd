package localsocket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/Borgels/mantlerd/internal/runtime"
)

// Server listens on a Unix domain socket and handles CLI requests.
type Server struct {
	path     string
	manager  *runtime.Manager
	listener net.Listener
}

// NewServer creates (and starts) a new socket server. It removes any stale
// socket file before creating the listener.  On Linux as root the socket is
// chowned to root:mantler with mode 0660 so mantler-group members can connect.
func NewServer(manager *runtime.Manager) (*Server, error) {
	path := SocketPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}
	// Remove stale socket file if present.
	_ = os.Remove(path)

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// Set group-readable so mantler-group members can connect without root.
	if goruntime.GOOS == "linux" && os.Geteuid() == 0 {
		if err := os.Chmod(path, 0o660); err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("chmod socket: %w", err)
		}
		// Attempt group chown; non-fatal if mantler group doesn't exist yet.
		chownSocket(path)
	}

	return &Server{path: path, manager: manager, listener: l}, nil
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/model/pull", s.handleModelPull)
	mux.HandleFunc("/model/start", s.handleModelStart)
	mux.HandleFunc("/model/stop", s.handleModelStop)
	mux.HandleFunc("/runtime/install", s.handleRuntimeInstall)
	mux.HandleFunc("/runtime/restart", s.handleRuntimeRestart)

	srv := &http.Server{Handler: mux}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = os.Remove(s.path)
	}()

	if err := srv.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		log.Printf("localsocket: server error: %v", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{OK: true})
}

func (s *Server) handleModelPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req PullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Determine runtime if not specified.
	targetRuntime := req.Runtime
	if targetRuntime == "" {
		ready := s.manager.ReadyRuntimes()
		if len(ready) > 0 {
			targetRuntime = ready[0]
		} else {
			installed := s.manager.InstalledRuntimes()
			if len(installed) == 0 {
				jsonError(w, "no runtimes installed", http.StatusUnprocessableEntity)
				return
			}
			targetRuntime = installed[0]
		}
	}

	// Stream progress as newline-delimited JSON.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	emit := func(ev ProgressEvent) {
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "%s\n", b)
		if canFlush {
			flusher.Flush()
		}
	}

	reportProgress := func(p runtime.PullProgress) {
		emit(ProgressEvent{Status: p.Status, Percent: p.Percent, Total: p.Total})
	}

	if err := s.manager.PrepareModelWithRuntimeProgressCtx(r.Context(), req.ModelID, targetRuntime, nil, reportProgress); err != nil {
		emit(ProgressEvent{Error: err.Error()})
		return
	}
	emit(ProgressEvent{Done: true})
}

func (s *Server) handleModelStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.manager.StartModelWithRuntime(req.ModelID, req.Runtime, nil); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w)
}

func (s *Server) handleModelStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req StopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.manager.StopModelWithRuntime(req.ModelID, req.Runtime); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w)
}

func (s *Server) handleRuntimeInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RuntimeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.manager.EnsureRuntime(req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w)
}

func (s *Server) handleRuntimeRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RuntimeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	mgr := runtime.NewServiceManager()
	if err := mgr.Restart(req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w)
}

func jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{OK: true})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(Response{Error: msg})
}
