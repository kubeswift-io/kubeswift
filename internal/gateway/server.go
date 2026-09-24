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
	// Origins, when set, polices browser (Origin-bearing) requests to the
	// Connect surface the same way as the raw-WS planes. Required for
	// auth-mode=insecure (see withCORS).
	Origins  *OriginPolicy
	Handlers []ConnectHandler
	// RawHandlers are non-Connect routes (e.g. the WebSocket console plane),
	// mounted on the same mux. They handle their own protocol upgrade.
	RawHandlers []ConnectHandler
	Log         logr.Logger
}

// NeedLeaderElection keeps the server running on every replica.
func (s *Server) NeedLeaderElection() bool { return false }

// Start serves until ctx is cancelled, then drains gracefully.
// MaxRequestBytes caps the decompressed size of any Connect request message.
// The gateway's messages are small control-plane payloads (guest specs,
// resource lists, a resource-apply manifest at most), so 4 MiB is generous
// while still bounding what an unauthenticated caller can make the server
// allocate: Connect reads and gunzips the whole message before the handler —
// and thus before auth — runs, so an uncapped handler lets one small gzip body
// inflate to gigabytes and OOM the gateway. Applied via connect.WithReadMaxBytes
// on every service handler in cmd/kubeswift-gateway.
const MaxRequestBytes = 4 << 20

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	for _, h := range s.Handlers {
		mux.Handle(h.Path, h.Handler)
	}
	for _, h := range s.RawHandlers {
		mux.Handle(h.Path, h.Handler)
	}
	mux.HandleFunc("/healthz", okHandler)
	mux.HandleFunc("/readyz", okHandler)

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           h2c.NewHandler(withCORS(mux, s.AllowedOrigin, s.Origins), &http2.Server{}),
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
//
// Not with auth-mode=insecure, where there is no token: "*" there let any page
// the operator visited drive every RPC, mutating ones included, on a gateway
// the browser could reach. A strict origin policy refuses a browser request
// from an origin it does not allow, and names only that origin in the CORS
// response.
func withCORS(h http.Handler, origin string, policy *OriginPolicy) http.Handler {
	if origin == "" {
		origin = "*"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allow := origin
		if policy != nil && policy.strict {
			if r.Header.Get("Origin") != "" && !policy.Allow(r) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			allow = policy.AllowedOrigin(r)
		}
		if allow != "" {
			w.Header().Set("Access-Control-Allow-Origin", allow)
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Grpc-Timeout, X-Grpc-Web, X-User-Agent, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Connect-Protocol-Version")
		w.Header().Set("Access-Control-Max-Age", "7200")
		if allow != "*" {
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
