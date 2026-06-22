package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// ConnectHandler is a (path, handler) pair as returned by the generated
// New<Service>Handler constructors in kubeswiftv1connect.
type ConnectHandler struct {
	Path    string
	Handler http.Handler
}

// Server is the gateway's browser-facing HTTP surface: the Connect / gRPC-Web
// handlers plus health probes, served over h2c (cleartext HTTP/2) so that
// server-streaming RPCs work behind a TLS-terminating ingress. It is a
// manager.Runnable so it shares the manager's lifecycle and shuts down on the
// signal context.
type Server struct {
	Addr          string
	AllowedOrigin string
	Handlers      []ConnectHandler
	Log           logr.Logger
}

// NeedLeaderElection keeps the server running on every replica.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled, then drains gracefully.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	for _, h := range s.Handlers {
		mux.Handle(h.Path, h.Handler)
	}
	mux.HandleFunc("/healthz", okHandler)
	mux.HandleFunc("/readyz", okHandler)

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           h2c.NewHandler(withCORS(mux, s.AllowedOrigin), &http2.Server{}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("gateway listening", "addr", s.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// withCORS allows the configured browser origin to reach the Connect / gRPC-Web
// surface. Auth rides the Authorization header (a bearer token the gateway
// impersonates from — PR C2), never cookies, so credentials are not enabled and
// a wildcard origin is acceptable for a token-auth API.
func withCORS(h http.Handler, origin string) http.Handler {
	if origin == "" {
		origin = "*"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Protocol-Version")
		w.Header().Set("Access-Control-Max-Age", "7200")
		if origin != "*" {
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
