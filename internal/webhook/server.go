package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

// Server runs the webhook receiver as a controller-runtime Runnable. It
// does not need leader election: every replica can accept deliveries
// because the intake is idempotent.
type Server struct {
	Addr    string
	Path    string
	Handler http.Handler
	Log     logr.Logger
	// Listener, if set, is used instead of listening on Addr (tests).
	Listener net.Listener
	// ready is closed once the listener is bound.
	ready chan struct{}
	addr  net.Addr
}

// NeedLeaderElection reports false: all replicas serve webhooks.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Start(ctx context.Context) error {
	ln := s.Listener
	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", s.Addr)
		if err != nil {
			return err
		}
	}
	s.addr = ln.Addr()
	if s.ready != nil {
		close(s.ready)
	}
	mux := http.NewServeMux()
	mux.Handle(s.Path, s.Handler)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.Log.Info("webhook receiver listening", "addr", ln.Addr().String(), "path", s.Path)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// NewServer returns a server; Ready is closed when it is listening.
func NewServer(addr, path string, h http.Handler, log logr.Logger) *Server {
	return &Server{Addr: addr, Path: path, Handler: h, Log: log, ready: make(chan struct{})}
}

// Ready is closed once the server has bound its listener.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// BoundAddr is the address actually listened on (valid after Ready).
func (s *Server) BoundAddr() net.Addr { return s.addr }
