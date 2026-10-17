package main

import (
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// Platform requirements end to end: a TLS registry, real cosign signatures
// with claims, the materializer binary. Needs cosign 3 at
// $KUBESWIFT_TEST_COSIGN; skipped without it.
func TestRequirements(t *testing.T) {
	cosign := os.Getenv("KUBESWIFT_TEST_COSIGN")
	if cosign == "" {
		t.Skip("KUBESWIFT_TEST_COSIGN (a cosign 3 binary) not set")
	}
	bin := binary(t)
	work := t.TempDir()
	// cosign is run as "cosign" from PATH, by the binary and by this test.
	pathDir := filepath.Join(work, "bin")
	if err := os.MkdirAll(pathDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cosign, filepath.Join(pathDir, "cosign")); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewTLSServer(registry.New())
	defer srv.Close()
	ca := filepath.Join(work, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+pathDir+":"+os.Getenv("PATH"), "SSL_CERT_FILE="+ca, "COSIGN_PASSWORD=", "HOME="+work)
	host := strings.TrimPrefix(srv.URL, "https://")

	run := func(name string, args ...string) {
		t.Helper()
		if name == "cosign" {
			name = cosign // exec resolves a bare name on this process's PATH
		}
		cmd := exec.Command(name, args...)
		cmd.Env, cmd.Dir = env, work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	for _, k := range []string{"a", "b", "tenant"} {
		run("cosign", "generate-key-pair", "--output-key-prefix", k)
	}
	pub := func(k string) string {
		b, err := os.ReadFile(filepath.Join(work, k+".pub"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	signingConfig := filepath.Join(work, "signing-config.json")
	cfg, err := os.ReadFile("../../internal/oci/signing-config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signingConfig, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	push := func(repo string) string {
		t.Helper()
		img, _ := random.Image(512, 1)
		ref, _ := name.ParseReference(host + "/" + repo + ":1")
		if err := remote.Write(ref, img, remote.WithTransport(srv.Client().Transport)); err != nil {
			t.Fatal(err)
		}
		d, _ := img.Digest()
		return host + "/" + repo + "@" + d.String()
	}
	sign := func(key, ref string, claims ...string) {
		t.Helper()
		args := []string{"sign", "--key", key + ".key", "--yes", "--signing-config", signingConfig}
		for _, c := range claims {
			args = append(args, "-a", c)
		}
		run("cosign", append(args, ref)...)
	}

	claims := []materialize.RequiredClaim{{Key: "example.com/kind", Values: []string{"cache"}}, {Key: "example.com/consumer", Values: []string{"team-a"}}}
	req := func(keys ...string) materialize.Requirement {
		r := materialize.Requirement{Policy: "caches", UID: "uid-1", Claims: claims}
		for _, k := range keys {
			r.Keys = append(r.Keys, materialize.RequirementKey{Authority: k, PublicKey: pub(k)})
		}
		return r
	}
	type outcome struct {
		code   int
		res    materialize.Result
		term   string
		cache  string
		mounts bool
	}
	var runEnv []string // nil: env
	materialize1 := func(ref string, reqs []materialize.Requirement, extra ...string) outcome {
		t.Helper()
		dir := t.TempDir()
		t.Cleanup(func() {
			_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi.IsDir() {
					_ = os.Chmod(p, 0o755)
				}
				return nil
			})
		})
		raw, _ := json.Marshal(reqs)
		result, ok := filepath.Join(dir, "result"), filepath.Join(dir, "ok")
		cmd := exec.Command(bin, append([]string{"--cache-dir", filepath.Join(dir, "cache"), "--mode", "oci",
			"--result-file", result, "--ok-file", ok, "--image", ref}, extra...)...)
		base := env
		if runEnv != nil {
			base = runEnv
		}
		cmd.Env = append(base, materialize.RequirementsEnv+"="+string(raw))
		out, err := cmd.CombinedOutput()
		o := outcome{cache: filepath.Join(dir, "cache")}
		if ee, isExit := err.(*exec.ExitError); isExit {
			o.code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("run: %v", err)
		}
		b, _ := os.ReadFile(result)
		o.term = string(b)
		_ = json.Unmarshal(b, &o.res)
		_, statErr := os.Stat(ok)
		o.mounts = statErr == nil
		t.Logf("%s", out)
		return o
	}
	record := func(o outcome, ref, key string) bool {
		t.Helper()
		pk, err := materialize.ParsePublicKey(pub(key))
		if err != nil {
			t.Fatal(err)
		}
		canon, _ := materialize.CanonicalRepository(ref)
		d := ref[strings.Index(ref, "@")+1:]
		_, err = os.Stat(materialize.RequirementMarkerPath(o.cache, d, materialize.RequirementFingerprint("uid-1", canon, d, pk.ID, claims)))
		return err == nil
	}

	good := push("caches/good")
	sign("a", good, "example.com/kind=cache", "example.com/consumer=team-a", "other=x")
	if o := materialize1(good, []materialize.Requirement{req("a")}); o.code != 0 || !o.mounts || !record(o, good, "a") {
		t.Fatalf("signed with the claims: %+v", o)
	}

	// Rotation: the policy trusts a and b; only b signed this one.
	rotated := push("caches/rotated")
	sign("b", rotated, "example.com/kind=cache", "example.com/consumer=team-a")
	if o := materialize1(rotated, []materialize.Requirement{req("a", "b")}); o.code != 0 || !record(o, rotated, "b") || record(o, rotated, "a") {
		t.Fatalf("rotated: %+v", o)
	}

	// Signed by the policy key, for another consumer: a claim mismatch.
	other := push("caches/other")
	sign("a", other, "example.com/kind=cache", "example.com/consumer=team-b")
	if o := materialize1(other, []materialize.Requirement{req("a")}); o.code == 0 || !strings.HasPrefix(o.term, "ClaimMismatch: policy caches") || record(o, other, "a") {
		t.Fatalf("required, other consumer: %+v", o)
	}
	if o := materialize1(other, []materialize.Requirement{req("a")}, "--optional"); o.code != 0 || o.mounts || o.res.Skipped == nil || o.res.Skipped.Reason != "ClaimMismatch" {
		t.Fatalf("optional, other consumer: %+v", o)
	}

	// A tenant's own key signed the right claims: not the platform's.
	tenant := push("caches/tenant")
	sign("tenant", tenant, "example.com/kind=cache", "example.com/consumer=team-a")
	if o := materialize1(tenant, []materialize.Requirement{req("a")}, "--optional"); o.code != 0 || o.mounts || o.res.Skipped == nil || o.res.Skipped.Reason != "VerificationFailed" {
		t.Fatalf("tenant-signed: %+v", o)
	}

	// The claims split across two platform signatures never combine.
	split := push("caches/split")
	sign("a", split, "example.com/kind=cache")
	sign("a", split, "example.com/consumer=team-a")
	if o := materialize1(split, []materialize.Requirement{req("a")}); o.code == 0 || !strings.HasPrefix(o.term, "ClaimMismatch") {
		t.Fatalf("split claims: %+v", o)
	}

	// Tenant key AND policy: both are checked and both recorded; the
	// tenant key failing fails the artifact even though the policy passes.
	both := push("caches/both")
	sign("a", both, "example.com/kind=cache", "example.com/consumer=team-a")
	sign("tenant", both)
	o := materialize1(both, []materialize.Requirement{req("a")}, "--verify-key", filepath.Join(work, "tenant.pub"))
	tk, _ := os.ReadFile(filepath.Join(work, "tenant.pub"))
	d := both[strings.Index(both, "@")+1:]
	if _, err := os.Stat(materialize.VerifiedMarkerPath(o.cache, d, materialize.KeyFingerprint(tk))); o.code != 0 || !record(o, both, "a") || err != nil {
		t.Fatalf("tenant key and policy: %+v (%v)", o, err)
	}
	if o := materialize1(good, []materialize.Requirement{req("a")}, "--verify-key", filepath.Join(work, "tenant.pub")); o.code == 0 {
		t.Fatalf("policy met but the tenant's own key did not sign: %+v", o)
	}

	// A node that already holds the record for a key the policy still
	// trusts needs no new check: with no cosign at all, a run under A+B over
	// a cache verified with A succeeds and records nothing new.
	shared := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(shared, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	if o := materialize1(good, []materialize.Requirement{req("a")}, "--cache-dir", shared); o.code != 0 {
		t.Fatalf("first run: %+v", o)
	}
	runEnv = []string{"PATH=/nonexistent", "SSL_CERT_FILE=" + ca, "HOME=" + work}
	if o := materialize1(good, []materialize.Requirement{req("a", "b")}, "--cache-dir", shared); o.code != 0 || !o.mounts {
		t.Fatalf("recorded, cosign absent: %+v", o)
	}
	// Under claims it never met, the record does not count: cosign is needed
	// (and absent), so the run fails.
	stricter := req("a")
	stricter.Claims = append(stricter.Claims, materialize.RequiredClaim{Key: "example.com/extra", Values: []string{"x"}})
	if o := materialize1(good, []materialize.Requirement{stricter}, "--cache-dir", shared); o.code == 0 {
		t.Fatalf("stricter claims reused a record: %+v", o)
	}
	runEnv = nil

	// Requirements never run against a plain-HTTP registry.
	if o := materialize1(good, []materialize.Requirement{req("a")}, "--insecure"); o.code == 0 {
		t.Fatalf("insecure: %+v", o)
	}
}
