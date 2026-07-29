package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// AuditInterceptor logs every RPC the gateway serves.
//
// Why this exists: two SwiftGuests were deleted from a dev cluster and the
// cause could not be determined, because the gateway had served traffic for 42
// hours and written three lines -- all from start-up. Every mutation it had
// ever proxied was unrecorded. A SwiftGuest carries no finalizer and no owner
// reference, so a delete leaves nothing behind to inspect: the object, its
// launcher pod and its events all go at once. This interceptor is the only
// place such a call can be observed at all.
//
// The rule it enforces: a mutation the gateway performs on a user's behalf MUST
// leave a trace that outlives the request, naming who asked and what was
// touched. Reads stay at V(1) because list/watch/telemetry poll continuously,
// and their absence is not what makes an incident unsolvable.
//
// Classification is deliberately default-mutating: a procedure counts as a read
// only if its name starts with a known read verb. An RPC added later that
// nobody thinks to classify is logged loudly rather than skipped silently -- the
// cost of forgetting is noise, not another blind spot.
type AuditInterceptor struct {
	log  logr.Logger
	auth Authenticator
}

// NewAuditInterceptor returns an interceptor that records RPCs. auth may be nil,
// in which case the caller is logged as unknown rather than the interceptor
// being skipped: a mutation with an unidentified actor is still worth recording.
func NewAuditInterceptor(log logr.Logger, auth Authenticator) *AuditInterceptor {
	return &AuditInterceptor{log: log, auth: auth}
}

// readVerbs are the procedure-name prefixes that do not change state.
var readVerbs = []string{"List", "Get", "Watch", "CanI", "Stream", "Describe"}

// splitProcedure reduces "/kubeswift.v1.GuestService/DeleteGuest" to
// ("GuestService", "DeleteGuest").
func splitProcedure(procedure string) (service, method string) {
	parts := strings.Split(strings.TrimPrefix(procedure, "/"), "/")
	if len(parts) != 2 {
		return "", procedure
	}
	svc := parts[0]
	if i := strings.LastIndex(svc, "."); i >= 0 {
		svc = svc[i+1:]
	}
	return svc, parts[1]
}

// isMutating reports whether a method changes state. Unknown verbs count as
// mutating on purpose -- see the type comment.
func isMutating(method string) bool {
	for _, v := range readVerbs {
		if strings.HasPrefix(method, v) {
			return false
		}
	}
	return true
}

// requestTarget extracts a human-meaningful subject from a request message
// without the interceptor having to know every request type. It follows a
// nested `ref` (the common ObjectRef shape) and otherwise reads flat
// cluster/namespace/name fields. Anything unrecognised yields "", which is
// omitted from the log rather than guessed at.
func requestTarget(msg any) (cluster, namespace, name string) {
	pm, ok := msg.(proto.Message)
	if !ok {
		return "", "", ""
	}
	m := pm.ProtoReflect()
	fields := m.Descriptor().Fields()

	if rf := fields.ByName("ref"); rf != nil && rf.Kind() == protoreflect.MessageKind && m.Has(rf) {
		return requestTarget(m.Get(rf).Message().Interface())
	}

	str := func(n string) string {
		f := fields.ByName(protoreflect.Name(n))
		if f == nil || f.Kind() != protoreflect.StringKind {
			return ""
		}
		return m.Get(f).String()
	}
	return str("cluster"), str("namespace"), str("name")
}

// WrapUnary records unary RPCs. Every mutation lands here.
func (a *AuditInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)

		svc, method := splitProcedure(req.Spec().Procedure)
		cluster, namespace, name := requestTarget(req.Any())

		kv := []any{
			"service", svc,
			"method", method,
			"user", a.actor(ctx, req.Header()),
			"durationMs", time.Since(start).Milliseconds(),
			"outcome", outcomeOf(err),
		}
		kv = appendIfSet(kv, "cluster", cluster)
		kv = appendIfSet(kv, "namespace", namespace)
		kv = appendIfSet(kv, "name", name)
		kv = appendIfSet(kv, "err", errText(err))

		if isMutating(method) {
			// Always on. This is the line that has to exist a day later.
			a.log.Info("rpc mutation", kv...)
		} else {
			a.log.V(1).Info("rpc", kv...)
		}
		return resp, err
	}
}

// WrapStreamingHandler records the open and close of a server stream. Individual
// events are not logged: a watch can run for hours and emit thousands of
// updates, none of which is a mutation.
func (a *AuditInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		svc, method := splitProcedure(conn.Spec().Procedure)
		a.log.V(1).Info("rpc stream closed",
			"service", svc,
			"method", method,
			"user", a.actor(ctx, conn.RequestHeader()),
			"durationMs", time.Since(start).Milliseconds(),
			"outcome", outcomeOf(err))
		return err
	}
}

// WrapStreamingClient satisfies the interceptor interface. The gateway is a
// server and never originates Connect calls, so this is a pass-through.
func (a *AuditInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// actor names the end user on whose behalf the call ran. It re-verifies the
// token rather than reaching into handler state; that costs one extra signature
// check on a cached JWKS, which is a fair price for the audit line being
// trustworthy on its own.
func (a *AuditInterceptor) actor(ctx context.Context, h http.Header) string {
	if a.auth == nil || h == nil {
		return "<unknown>"
	}
	id, err := a.auth.Authenticate(ctx, h)
	if err != nil {
		return "<unauthenticated>"
	}
	if id.User == "" {
		// auth-mode=insecure: member queries run as the gateway's own
		// credential, so there is no end user to name. Say that explicitly
		// rather than logging "" , which reads like a bug in the logger.
		return "<gateway-credential>"
	}
	return id.User
}

func appendIfSet(kv []any, key, val string) []any {
	if val == "" {
		return kv
	}
	return append(kv, key, val)
}

func outcomeOf(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
