package materialize

import (
	"encoding/base64"
	"testing"
)

func dcj(host, user, pass string) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return []byte(`{"auths":{"` + host + `":{"auth":"` + auth + `"}}}`)
}

func TestAuthFromDockerConfigJSON(t *testing.T) {
	// A matching registry yields basic auth with the decoded user/pass.
	a, err := AuthFromDockerConfigJSON(dcj("ghcr.io", "wrkode", "tok123"), "ghcr.io/org/img:tag")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg, err := a.Authorization()
	if err != nil {
		t.Fatalf("authorization: %v", err)
	}
	if cfg.Username != "wrkode" || cfg.Password != "tok123" {
		t.Errorf("got %q/%q, want wrkode/tok123", cfg.Username, cfg.Password)
	}

	// A registry with no matching entry degrades to anonymous (not an error).
	a2, err := AuthFromDockerConfigJSON(dcj("ghcr.io", "u", "p"), "docker.io/library/alpine:3.20")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c2, _ := a2.Authorization()
	if c2.Username != "" || c2.Password != "" {
		t.Errorf("non-matching registry should be anonymous, got %q/%q", c2.Username, c2.Password)
	}

	// A scheme-prefixed config key still matches the bare registry host.
	a3, err := AuthFromDockerConfigJSON(dcj("https://ghcr.io", "x", "y"), "ghcr.io/o/i@sha256:"+repeat("a", 64))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	c3, _ := a3.Authorization()
	if c3.Username != "x" {
		t.Errorf("scheme-prefixed key should match; got %q", c3.Username)
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
