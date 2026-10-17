package artifactpolicy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

func newKey(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

const dig = "sha256:" + "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"

func str(s string) *string { return &s }

type opt func(*sandboxv1alpha1.SwiftArtifactVerificationPolicy)

func policy(t *testing.T, name string, keys []string, opts ...opt) sandboxv1alpha1.SwiftArtifactVerificationPolicy {
	p := sandboxv1alpha1.SwiftArtifactVerificationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Generation: 1},
		Spec: sandboxv1alpha1.SwiftArtifactVerificationPolicySpec{
			NamespaceSelector: metav1.LabelSelector{MatchLabels: map[string]string{"trust": "platform"}},
			Repositories:      []string{"registry.example.com/caches/**"},
		},
	}
	for i, k := range keys {
		p.Spec.Authorities = append(p.Spec.Authorities, sandboxv1alpha1.ArtifactAuthority{Name: string(rune('a' + i)), Key: &sandboxv1alpha1.KeyAuthority{PublicKey: k}})
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

func compile(ps ...sandboxv1alpha1.SwiftArtifactVerificationPolicy) []*Compiled {
	var out []*Compiled
	for i := range ps {
		out = append(out, Compile(&ps[i]))
	}
	return out
}

var trusted = map[string]string{"trust": "platform"}

const (
	cacheRef = "registry.example.com/caches/app@" + dig
	otherRef = "registry.example.com/apps/app@" + dig
)

func TestDecide_Binding(t *testing.T) {
	k := newKey(t)
	ps := compile(policy(t, "caches", []string{k}))

	// No policy at all: exactly v0.17.
	if d := Decide(nil, "team-a", trusted, Artifact{Ref: cacheRef}); d.Governed() || d.Denied != "" {
		t.Errorf("no policies: %+v", d)
	}
	// A namespace the policy does not select: ungoverned, whatever the ref.
	if d := Decide(ps, "team-a", map[string]string{"trust": "none"}, Artifact{Ref: cacheRef}); d.Governed() || d.Denied != "" {
		t.Errorf("unselected namespace: %+v", d)
	}
	// Selected, governed: one requirement, the policy's keys.
	d := Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef})
	if !d.Governed() || d.Denied != "" || len(d.Requirements) != 1 || d.Requirements[0].UID != "uid-caches" || len(d.Requirements[0].Keys) != 1 {
		t.Fatalf("governed: %+v", d)
	}
	if d.Repository != "registry.example.com/caches/app" || d.Policies[0].Name != "caches" || d.Policies[0].Generation != 1 {
		t.Errorf("governed: %+v", d)
	}
	// A repository the policy does not govern: allowed by default ...
	if d := Decide(ps, "team-a", trusted, Artifact{Ref: otherRef}); d.Governed() || d.Denied != "" {
		t.Errorf("other repository, Allow: %+v", d)
	}
	// ... refused with otherRepositories: Deny.
	deny := compile(policy(t, "caches", []string{k}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.OtherRepositories = sandboxv1alpha1.OtherRepositoriesDeny
	}))
	if d := Decide(deny, "team-a", trusted, Artifact{Ref: otherRef}); d.Denied != DeniedRepositoryNotAllowed {
		t.Errorf("other repository, Deny: %+v", d)
	}
	// Deny in a namespace it does not select changes nothing there.
	if d := Decide(deny, "team-b", nil, Artifact{Ref: otherRef}); d.Denied != "" {
		t.Errorf("Deny outside its namespaces: %+v", d)
	}
	// Selecting by namespace name, which no namespace admin can change.
	byName := compile(policy(t, "caches", []string{k}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.NamespaceSelector = metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "team-a"}}
	}))
	if d := Decide(byName, "team-a", nil, Artifact{Ref: cacheRef}); !d.Governed() {
		t.Errorf("by namespace name: %+v", d)
	}
	if d := Decide(byName, "team-b", nil, Artifact{Ref: cacheRef}); d.Governed() {
		t.Errorf("by namespace name, other namespace: %+v", d)
	}
	// {} selects every namespace.
	all := compile(policy(t, "caches", []string{k}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.NamespaceSelector = metav1.LabelSelector{}
	}))
	if d := Decide(all, "anything", nil, Artifact{Ref: cacheRef}); !d.Governed() {
		t.Errorf("empty selector: %+v", d)
	}
}

// Nothing a workload can set removes or weakens a governing policy.
func TestDecide_WorkloadCannotWeaken(t *testing.T) {
	k := newKey(t)
	ps := compile(policy(t, "caches", []string{k}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.Claims = []sandboxv1alpha1.ArtifactClaimRequirement{{Key: "example.com/kind", Value: str("cache")}}
	}))
	// Claims that try to replace the policy's value: refused, not obeyed.
	d := Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef, Claims: map[string]string{"example.com/kind": "anything"}})
	if d.Denied != DeniedClaimConflict {
		t.Errorf("conflicting claim: %+v", d)
	}
	// A claim equal to the policy's: same requirement.
	d = Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef, Claims: map[string]string{"example.com/kind": "cache"}})
	if d.Denied != "" || len(d.Requirements[0].Claims) != 1 {
		t.Errorf("agreeing claim: %+v", d)
	}
	// An extra claim adds a requirement.
	d = Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef, Claims: map[string]string{"example.com/extra": "y"}})
	if d.Denied != "" || len(d.Requirements[0].Claims) != 2 {
		t.Errorf("extra claim: %+v", d)
	}
	// Optional does not appear in the decision at all: the same requirement
	// applies (the caller skips instead of failing).
	// requirePlatformVerification and claims with no governing policy refuse.
	if d := Decide(ps, "team-a", trusted, Artifact{Ref: otherRef, RequirePlatformVerification: true}); d.Denied != DeniedNoMatchingPolicy {
		t.Errorf("requirePlatformVerification, ungoverned: %+v", d)
	}
	if d := Decide(ps, "team-a", trusted, Artifact{Ref: otherRef, Claims: map[string]string{"k": "v"}}); d.Denied != DeniedNoMatchingPolicy {
		t.Errorf("claims, ungoverned: %+v", d)
	}
	if d := Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef, RequirePlatformVerification: true}); d.Denied != "" || !d.Governed() {
		t.Errorf("requirePlatformVerification, governed: %+v", d)
	}
}

func TestDecide_DigestPinningAndClaims(t *testing.T) {
	k := newKey(t)
	withClaims := func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.Claims = []sandboxv1alpha1.ArtifactClaimRequirement{
			{Key: "example.com/kind", Value: str("cache")},
			{Key: "example.com/runtime", Values: []string{"rt-2", "rt-1"}},
			{Key: "example.com/consumer", FromNamespace: true},
			{Key: "example.com/source", FromArtifact: str("source")},
		}
	}
	ps := compile(policy(t, "caches", []string{k}, withClaims))
	if d := Decide(ps, "team-a", trusted, Artifact{Ref: "registry.example.com/caches/app:v1", Claims: map[string]string{"source": "s"}}); d.Denied != DeniedTagReference {
		t.Errorf("tag under Required: %+v", d)
	}
	optional := compile(policy(t, "caches", []string{k}, withClaims, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.DigestPinning = sandboxv1alpha1.DigestPinningOptional
	}))
	if d := Decide(optional, "team-a", trusted, Artifact{Ref: "registry.example.com/caches/app:v1", Claims: map[string]string{"source": "s"}}); d.Denied != "" {
		t.Errorf("tag under Optional: %+v", d)
	}
	if d := Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef}); d.Denied != DeniedMissingClaimValue {
		t.Errorf("fromArtifact not supplied: %+v", d)
	}
	d := Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef, Claims: map[string]string{"source": "sha256:abc"}})
	if d.Denied != "" {
		t.Fatalf("claims: %+v", d)
	}
	want := []materialize.RequiredClaim{
		{Key: "example.com/consumer", Values: []string{"team-a"}},
		{Key: "example.com/kind", Values: []string{"cache"}},
		{Key: "example.com/runtime", Values: []string{"rt-1", "rt-2"}},
		{Key: "example.com/source", Values: []string{"sha256:abc"}},
	}
	if got := d.Requirements[0].Claims; !slices.EqualFunc(got, want, func(a, b materialize.RequiredClaim) bool {
		return a.Key == b.Key && slices.Equal(a.Values, b.Values)
	}) {
		t.Errorf("claims %+v, want %+v", got, want)
	}
	// The "source" name feeds fromArtifact; it is not also an extra claim.
	for _, c := range d.Requirements[0].Claims {
		if c.Key == "source" {
			t.Error("a fromArtifact name became a claim of its own")
		}
	}
	// fromNamespace binds to the consumer: another namespace, another value.
	d2 := Decide(ps, "team-b", trusted, Artifact{Ref: cacheRef, Claims: map[string]string{"source": "sha256:abc"}})
	r1, _ := d.AcceptableRecords(dig)
	r2, _ := d2.AcceptableRecords(dig)
	if r1[0][0] == r2[0][0] {
		t.Error("two namespaces share a record under fromNamespace")
	}
}

func TestDecide_FailClosed(t *testing.T) {
	k := newKey(t)
	badKey := compile(policy(t, "caches", []string{"-----BEGIN PUBLIC KEY-----\nnope\n-----END PUBLIC KEY-----\n"}))
	if badKey[0].Err == nil {
		t.Fatal("a bad key compiled")
	}
	if d := Decide(badKey, "team-a", trusted, Artifact{Ref: cacheRef}); d.Denied != DeniedPolicyInvalid {
		t.Errorf("bad key: %+v", d)
	}
	// One bad key among good ones still makes the policy unusable: an
	// administrator sees it Not Ready instead of losing a key silently.
	mixed := compile(policy(t, "caches", []string{k, "garbage"}))
	if d := Decide(mixed, "team-a", trusted, Artifact{Ref: cacheRef}); d.Denied != DeniedPolicyInvalid {
		t.Errorf("one bad key: %+v", d)
	}
	badSelector := compile(policy(t, "caches", []string{k}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "x", Operator: "Bogus"}}}
	}))
	if d := Decide(badSelector, "elsewhere", nil, Artifact{Ref: cacheRef}); d.Denied != DeniedPolicyInvalid {
		t.Errorf("bad selector must select everywhere and refuse: %+v", d)
	}
	badGlob := compile(policy(t, "caches", []string{k}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.Repositories = []string{"Registry/X["}
	}))
	if d := Decide(badGlob, "team-a", trusted, Artifact{Ref: otherRef}); d.Denied != DeniedPolicyInvalid {
		t.Errorf("bad glob must govern everything and refuse: %+v", d)
	}
	// A valid and an invalid policy: the invalid one still refuses.
	both := compile(policy(t, "a-good", []string{k}), policy(t, "b-bad", []string{"garbage"}))
	if d := Decide(both, "team-a", trusted, Artifact{Ref: cacheRef}); d.Denied != DeniedPolicyInvalid || !strings.Contains(d.Message, "b-bad") {
		t.Errorf("good and bad: %+v", d)
	}
}

// Two governing policies: both apply, in name order.
func TestDecide_EveryGoverningPolicy(t *testing.T) {
	k1, k2 := newKey(t), newKey(t)
	ps := compile(policy(t, "z-second", []string{k2}), policy(t, "a-first", []string{k1}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.Repositories = []string{"registry.example.com/*/app"}
	}))
	d := Decide(ps, "team-a", trusted, Artifact{Ref: cacheRef})
	if len(d.Requirements) != 2 || d.Requirements[0].Policy != "a-first" || d.Requirements[1].Policy != "z-second" {
		t.Fatalf("%+v", d)
	}
	recs, err := d.AcceptableRecords(dig)
	if err != nil || len(recs) != 2 || len(recs[0]) != 1 || len(recs[1]) != 1 {
		t.Errorf("records %v, %v", recs, err)
	}
}

// A -> A+B -> B: adding a key keeps A's record acceptable; removing A
// leaves only B's.
func TestAcceptableRecords_Rotation(t *testing.T) {
	a, b := newKey(t), newKey(t)
	recs := func(keys ...string) []string {
		d := Decide(compile(policy(t, "caches", keys)), "team-a", trusted, Artifact{Ref: cacheRef})
		r, err := d.AcceptableRecords(dig)
		if err != nil {
			t.Fatal(err)
		}
		return r[0]
	}
	onlyA, both, onlyB := recs(a), recs(a, b), recs(b)
	if !slices.Contains(both, onlyA[0]) || !slices.Contains(both, onlyB[0]) || len(both) != 2 {
		t.Errorf("A+B %v must accept A %v and B %v", both, onlyA, onlyB)
	}
	if slices.Contains(onlyB, onlyA[0]) {
		t.Error("after removing A, A's record is still accepted")
	}
	// Claims change the record; the selector and repositories do not.
	base := recs(a)[0]
	d := Decide(compile(policy(t, "caches", []string{a}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.NamespaceSelector = metav1.LabelSelector{}
		p.Spec.Repositories = append(p.Spec.Repositories, "other.example.com/**")
		p.Generation = 7
	})), "team-a", trusted, Artifact{Ref: cacheRef})
	if r, _ := d.AcceptableRecords(dig); r[0][0] != base {
		t.Error("a selector or repository change invalidated records")
	}
	d = Decide(compile(policy(t, "caches", []string{a}, func(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) {
		p.Spec.Claims = []sandboxv1alpha1.ArtifactClaimRequirement{{Key: "k", Value: str("v")}}
	})), "team-a", trusted, Artifact{Ref: cacheRef})
	if r, _ := d.AcceptableRecords(dig); r[0][0] == base {
		t.Error("a claim change kept the old record")
	}
	// Another policy (another UID) with the same content: its own record.
	p2 := policy(t, "caches", []string{a})
	p2.UID = "uid-recreated"
	d = Decide(compile(p2), "team-a", trusted, Artifact{Ref: cacheRef})
	if r, _ := d.AcceptableRecords(dig); r[0][0] == base {
		t.Error("a recreated policy reused the old policy's records")
	}
}

func TestMatchRepository(t *testing.T) {
	for _, tc := range []struct {
		glob, repo string
		want       bool
	}{
		{"registry.example.com/caches/**", "registry.example.com/caches/app", true},
		{"registry.example.com/caches/**", "registry.example.com/caches/a/b/c", true},
		{"registry.example.com/caches/**", "registry.example.com/caches", true},
		{"registry.example.com/caches/**", "registry.example.com/cachesx/app", false},
		{"registry.example.com/caches/*", "registry.example.com/caches/a/b", false},
		{"registry.example.com/*/app", "registry.example.com/team/app", true},
		{"registry.example.com/**", "evil.example.com/registry.example.com/x", false},
		{"*.example.com/x", "registry.example.com/x", true},
		{"registry.example.com:5000/x", "registry.example.com/x", false},
		{"index.docker.io/library/*", "index.docker.io/library/busybox", true},
		{"**", "anything/at/all", true},
	} {
		if got := MatchRepository(tc.glob, tc.repo); got != tc.want {
			t.Errorf("%s ~ %s = %v, want %v", tc.glob, tc.repo, got, tc.want)
		}
	}
}

func TestCache(t *testing.T) {
	var c Cache
	p := policy(t, "caches", []string{newKey(t)})
	first := c.Compiled([]sandboxv1alpha1.SwiftArtifactVerificationPolicy{p})
	if again := c.Compiled([]sandboxv1alpha1.SwiftArtifactVerificationPolicy{p}); again[0] != first[0] {
		t.Error("same generation compiled twice")
	}
	p.Generation = 2
	p.Spec.Authorities[0].Key.PublicKey = "garbage"
	if next := c.Compiled([]sandboxv1alpha1.SwiftArtifactVerificationPolicy{p}); next[0] == first[0] || next[0].Err == nil {
		t.Error("a new generation was not recompiled")
	}
	if none := c.Compiled(nil); len(none) != 0 || len(c.m) != 0 {
		t.Error("a deleted policy was kept")
	}
}
