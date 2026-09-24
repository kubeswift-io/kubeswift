package gateway

import (
	"time"

	"k8s.io/klog/v2"
)

// maxAuditedCommand bounds how much of an exec command a session record keeps.
const maxAuditedCommand = 512

// auditSession records an interactive WebSocket session: its opening, always
// logged, and -- through the returned func -- its close. The raw WebSocket
// routes (console, sandbox exec, sandbox logs) do not pass through the Connect
// AuditInterceptor, so without this a console into a privileged launcher or a
// shell command run in a sandbox left no trace naming who did it (the rule
// audit.go states: an action taken on a user's behalf must outlive the
// request). kv adds route-specific fields, such as the command.
func auditSession(route string, id Identity, cluster, namespace, name string, kv ...any) func() {
	start := time.Now()
	fields := append([]any{
		"route", route,
		"user", sessionActor(id),
		"cluster", cluster,
		"namespace", namespace,
		"name", name,
	}, kv...)
	klog.InfoS("ws session opened", fields...)
	return func() {
		klog.InfoS("ws session closed", append(fields, "durationMs", time.Since(start).Milliseconds())...)
	}
}

// sessionActor names the end user, the same way the RPC audit does.
func sessionActor(id Identity) string {
	if id.User == "" {
		return "<gateway-credential>"
	}
	return id.User
}

// auditedCommand is cmd bounded for the audit record.
func auditedCommand(cmd string) string {
	if len(cmd) <= maxAuditedCommand {
		return cmd
	}
	return cmd[:maxAuditedCommand] + "...(truncated)"
}
