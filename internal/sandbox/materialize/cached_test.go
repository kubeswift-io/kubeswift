package materialize

import (
	"crypto/elliptic"
	"os"
	"path/filepath"
	"testing"
)

// An entry is used without the registry only when a warm checkout would use
// it: sealed, read from this repository, verified for this key, and meeting
// every platform requirement. Anything less sends the caller to the registry.
func TestCachedArtifact(t *testing.T) {
	pub := ecKey(t, elliptic.P256())
	pk, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	req := Requirement{Policy: "p", UID: "uid", Keys: []RequirementKey{{Authority: "a", PublicKey: pub}}}
	const keyFP = "f00d"
	touch := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	ready := func(t *testing.T, skip string) string {
		dir := t.TempDir()
		entry := CachePathFor(dir, dig, ModeTree)
		if err := os.MkdirAll(entry, 0o755); err != nil {
			t.Fatal(err)
		}
		touch(filepath.Join(entry, "file"))
		marks := map[string]string{
			"sealed":      SealedMarkerPath(entry),
			"origin":      OriginMarkerPath(dir, dig, repo),
			"verified":    VerifiedMarkerPath(dir, dig, keyFP),
			"requirement": RequirementMarkerPath(dir, dig, RequirementFingerprint("uid", repo, dig, pk.ID, nil)),
		}
		for k, p := range marks {
			if k != skip {
				touch(p)
			}
		}
		return dir
	}
	opts := func(dir, ref string) Options {
		return Options{ImageRef: ref, CacheDir: dir, Mode: ModeTree}
	}

	dir := ready(t, "")
	res, ok, err := CachedArtifact(opts(dir, repo+"@"+dig), keyFP, []Requirement{req})
	if err != nil || !ok || res.Digest != dig || !res.CacheHit || res.RootfsPath != CachePathFor(dir, dig, ModeTree) {
		t.Fatalf("ready entry: res=%+v ok=%t err=%v", res, ok, err)
	}
	if _, ok, _ := CachedArtifact(opts(dir, repo+"@"+dig), "", nil); !ok {
		t.Error("ready entry with no key or policy not used")
	}

	for _, tc := range []struct {
		name, skip, ref, key string
	}{
		{"unsealed", "sealed", repo + "@" + dig, keyFP},
		{"not read from this repository", "origin", repo + "@" + dig, keyFP},
		{"another repository", "", "registry.example.com/other/app@" + dig, keyFP},
		{"not verified with this key", "verified", repo + "@" + dig, keyFP},
		{"verified with another key", "", repo + "@" + dig, "beef"},
		{"platform requirement not met", "requirement", repo + "@" + dig, keyFP},
		{"a tag", "", repo + ":1", keyFP},
	} {
		dir := ready(t, tc.skip)
		if res, ok, err := CachedArtifact(opts(dir, tc.ref), tc.key, []Requirement{req}); ok || err != nil {
			t.Errorf("%s: used from the cache (res=%+v err=%v)", tc.name, res, err)
		}
	}
	if _, ok, _ := CachedArtifact(opts(t.TempDir(), repo+"@"+dig), "", nil); ok {
		t.Error("missing entry used")
	}
}
