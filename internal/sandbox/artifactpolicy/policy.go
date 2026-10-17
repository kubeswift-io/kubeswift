// Package artifactpolicy decides which SwiftArtifactVerificationPolicies
// govern an artifact, and what they require of it. It is pure: the caller
// supplies the policies, the namespace and the artifact.
//
// The decision reads only the namespace (its name and labels) and the
// artifact's repository, reference form and claims. No workload field names
// a policy, so a tenant cannot pick, omit or swap one. Every governing
// policy must be met (AND). An invalid policy governs as if valid, and
// refuses: it never lets an artifact through unchecked.
package artifactpolicy

import (
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/materialize"
)

// Why an artifact is refused before anything is fetched (status message
// prefix under the PolicyDenied reason).
const (
	DeniedNoMatchingPolicy     = "NoMatchingPolicy"
	DeniedRepositoryNotAllowed = "RepositoryNotAllowed"
	DeniedTagReference         = "TagReference"
	DeniedPolicyInvalid        = "PolicyInvalid"
	DeniedMissingClaimValue    = "MissingClaimValue"
	DeniedClaimConflict        = "ClaimConflict"
)

// Compiled is a policy ready to evaluate.
type Compiled struct {
	Name       string
	UID        types.UID
	Generation int64

	selector      labels.Selector
	repositories  []string
	badGlobs      bool
	allowOther    bool
	requireDigest bool
	keys          []materialize.RequirementKey
	claims        []sandboxv1alpha1.ArtifactClaimRequirement

	// Authorities describes each key, for the policy's status.
	Authorities []sandboxv1alpha1.ArtifactAuthorityStatus
	// Err is why the policy cannot be enforced; it then refuses what it
	// governs.
	Err error
}

// Compile parses p. A policy that does not parse is still returned, with
// Err set.
func Compile(p *sandboxv1alpha1.SwiftArtifactVerificationPolicy) *Compiled {
	c := &Compiled{Name: p.Name, UID: p.UID, Generation: p.Generation,
		repositories:  p.Spec.Repositories,
		allowOther:    p.Spec.AllowsOtherRepositories(),
		requireDigest: p.Spec.RequiresDigest(),
		claims:        p.Spec.Claims,
	}
	sel, err := metav1.LabelSelectorAsSelector(&p.Spec.NamespaceSelector)
	if err != nil {
		// Fail closed: an unreadable selector selects every namespace.
		c.selector, c.Err = labels.Everything(), fmt.Errorf("namespaceSelector: %w", err)
	} else {
		c.selector = sel
	}
	for _, g := range p.Spec.Repositories {
		if err := validGlob(g); err != nil {
			c.badGlobs = true
			if c.Err == nil {
				c.Err = fmt.Errorf("repositories %q: %w", g, err)
			}
		}
	}
	if len(p.Spec.Repositories) == 0 {
		c.badGlobs = true
		if c.Err == nil {
			c.Err = fmt.Errorf("no repositories")
		}
	}
	for _, a := range p.Spec.Authorities {
		if a.Key == nil {
			if c.Err == nil {
				c.Err = fmt.Errorf("authority %s: no key", a.Name)
			}
			continue
		}
		k, err := materialize.ParsePublicKey(a.Key.PublicKey)
		if err != nil {
			if c.Err == nil {
				c.Err = fmt.Errorf("authority %s: %w", a.Name, err)
			}
			c.Authorities = append(c.Authorities, sandboxv1alpha1.ArtifactAuthorityStatus{Name: a.Name})
			continue
		}
		c.keys = append(c.keys, materialize.RequirementKey{Authority: a.Name, PublicKey: string(k.PEM)})
		c.Authorities = append(c.Authorities, sandboxv1alpha1.ArtifactAuthorityStatus{Name: a.Name, KeyID: k.ID, Algorithm: k.Algorithm})
	}
	if len(c.keys) == 0 && c.Err == nil {
		c.Err = fmt.Errorf("no usable authority")
	}
	for _, cl := range p.Spec.Claims {
		if err := validClaim(cl); err != nil && c.Err == nil {
			c.Err = fmt.Errorf("claim %s: %w", cl.Key, err)
		}
	}
	return c
}

func validClaim(c sandboxv1alpha1.ArtifactClaimRequirement) error {
	if !sandboxv1alpha1.ValidClaimKey(c.Key) {
		return fmt.Errorf("key is not a valid claim key")
	}
	n := 0
	if c.Value != nil {
		n++
		if !sandboxv1alpha1.ValidClaimValue(*c.Value) {
			return fmt.Errorf("value is not a valid claim value")
		}
	}
	if len(c.Values) > 0 {
		n++
		for _, v := range c.Values {
			if !sandboxv1alpha1.ValidClaimValue(v) {
				return fmt.Errorf("value %q is not a valid claim value", v)
			}
		}
	}
	if c.FromNamespace {
		n++
	}
	if c.FromArtifact != nil {
		n++
		if !sandboxv1alpha1.ValidClaimKey(*c.FromArtifact) {
			return fmt.Errorf("fromArtifact is not a valid claim name")
		}
	}
	if n != 1 {
		return fmt.Errorf("set exactly one of value, values, fromNamespace, fromArtifact")
	}
	return nil
}

// validGlob accepts the CRD's glob grammar: lowercase segments of
// [a-z0-9._:-] and *, separated by /.
func validGlob(g string) error {
	if g == "" {
		return fmt.Errorf("empty")
	}
	for _, seg := range strings.Split(g, "/") {
		if seg == "" {
			return fmt.Errorf("empty segment")
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-*", r)) {
				return fmt.Errorf("character %q", r)
			}
		}
		if strings.Contains(seg, "**") && seg != "**" {
			return fmt.Errorf("** must be a whole segment")
		}
	}
	return nil
}

// MatchRepository reports whether repository matches glob: * within one
// segment, ** any number of segments (including none).
func MatchRepository(glob, repository string) bool {
	return matchSegments(strings.Split(glob, "/"), strings.Split(repository, "/"))
}

func matchSegments(g, r []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(r); i++ {
				if matchSegments(g[1:], r[i:]) {
					return true
				}
			}
			return false
		}
		if len(r) == 0 {
			return false
		}
		if ok, err := path.Match(g[0], r[0]); err != nil || !ok {
			return false
		}
		g, r = g[1:], r[1:]
	}
	return len(r) == 0
}

// selects reports whether the policy applies in the namespace.
func (c *Compiled) selects(nsLabels labels.Set) bool { return c.selector.Matches(nsLabels) }

// governs reports whether the policy governs repository (in a namespace it
// selects).
func (c *Compiled) governs(repository string) bool {
	// A policy whose repositories cannot be read governs every repository
	// in the namespaces it selects, and refuses them: fail closed.
	if c.badGlobs {
		return true
	}
	for _, g := range c.repositories {
		if MatchRepository(g, repository) {
			return true
		}
	}
	return false
}

// Artifact is what the decision reads from a workload's artifact.
type Artifact struct {
	Ref                         string
	Claims                      map[string]string
	RequirePlatformVerification bool
}

// Decision is what applies to one artifact in one namespace.
type Decision struct {
	// Repository is the artifact's canonical repository.
	Repository string
	// Requirements has one entry per governing policy, in name order; empty
	// when no policy governs the artifact.
	Requirements []materialize.Requirement
	// Policies names the governing policies at the generation applied.
	Policies []sandboxv1alpha1.ArtifactPolicyReference
	// Denied is why the artifact is refused outright (Denied* constants),
	// with Message; "" when it may be fetched and verified.
	Denied  string
	Message string
}

// Governed reports whether any policy governs the artifact.
func (d *Decision) Governed() bool { return len(d.Policies) > 0 }

// Decide evaluates policies (all of them; order does not matter) for a in
// namespace ns.
func Decide(policies []*Compiled, ns string, nsLabels map[string]string, a Artifact) Decision {
	repo, err := materialize.CanonicalRepository(a.Ref)
	if err != nil {
		return Decision{Denied: DeniedPolicyInvalid, Message: fmt.Sprintf("reference %q: %v", a.Ref, err)}
	}
	set := labels.Set(nsLabels)
	if set == nil {
		set = labels.Set{}
	}
	if _, ok := set["kubernetes.io/metadata.name"]; !ok {
		set = labels.Merge(set, labels.Set{"kubernetes.io/metadata.name": ns})
	}
	sorted := slices.Clone(policies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	d := Decision{Repository: repo}
	denyOther := ""
	var governing []*Compiled
	for _, p := range sorted {
		if !p.selects(set) {
			continue
		}
		if p.governs(repo) {
			governing = append(governing, p)
			d.Policies = append(d.Policies, sandboxv1alpha1.ArtifactPolicyReference{Name: p.Name, Generation: p.Generation})
		} else if !p.allowOther && denyOther == "" {
			denyOther = p.Name
		}
	}
	deny := func(why, msg string) Decision {
		d.Requirements, d.Denied, d.Message = nil, why, msg
		return d
	}
	if len(governing) == 0 {
		switch {
		case denyOther != "":
			return deny(DeniedRepositoryNotAllowed, fmt.Sprintf("policy %s allows only the repositories it governs in this namespace; no policy governs %s", denyOther, repo))
		case a.RequirePlatformVerification:
			return deny(DeniedNoMatchingPolicy, fmt.Sprintf("requirePlatformVerification is set, and no policy governs %s in this namespace", repo))
		case len(a.Claims) > 0:
			return deny(DeniedNoMatchingPolicy, fmt.Sprintf("claims are set, and no policy governs %s in this namespace to check them", repo))
		}
		return d
	}
	isDigest := false
	if r, err := name.ParseReference(a.Ref); err == nil {
		_, isDigest = r.(name.Digest)
	}
	// Claim names the policies read as fromArtifact: every other entry of the
	// artifact's claims is an extra claim of its own.
	fromArtifact := map[string]bool{}
	for _, p := range governing {
		for _, c := range p.claims {
			if c.FromArtifact != nil {
				fromArtifact[*c.FromArtifact] = true
			}
		}
	}
	for _, p := range governing {
		if p.Err != nil {
			return deny(DeniedPolicyInvalid, fmt.Sprintf("policy %s cannot be enforced: %v", p.Name, p.Err))
		}
		if p.requireDigest && !isDigest {
			return deny(DeniedTagReference, fmt.Sprintf("policy %s requires a digest reference (repo@sha256:...), got %s", p.Name, a.Ref))
		}
		req := materialize.Requirement{Policy: p.Name, UID: string(p.UID), Generation: p.Generation, Keys: p.keys}
		values := map[string][]string{}
		for _, c := range p.claims {
			var v []string
			switch {
			case c.Value != nil:
				v = []string{*c.Value}
			case len(c.Values) > 0:
				v = c.Values
			case c.FromNamespace:
				v = []string{ns}
			case c.FromArtifact != nil:
				got, ok := a.Claims[*c.FromArtifact]
				if !ok {
					return deny(DeniedMissingClaimValue, fmt.Sprintf("policy %s needs the artifact's claims[%s] (claim %s)", p.Name, *c.FromArtifact, c.Key))
				}
				v = []string{got}
			}
			values[c.Key] = v
		}
		// The artifact's own claims add to the policy's: the same signed
		// payload must carry them too.
		for k, v := range a.Claims {
			if fromArtifact[k] {
				continue
			}
			if have, ok := values[k]; ok {
				if !slices.Contains(have, v) {
					return deny(DeniedClaimConflict, fmt.Sprintf("the artifact's claim %s=%s is not one policy %s accepts", k, v, p.Name))
				}
			}
			values[k] = []string{v}
		}
		for k, v := range values {
			req.Claims = append(req.Claims, materialize.RequiredClaim{Key: k, Values: v})
		}
		req.Claims = materialize.CanonicalClaims(req.Claims)
		d.Requirements = append(d.Requirements, req)
	}
	return d
}

// AcceptableRecords lists, for the decision at digest, each requirement's
// acceptable record fingerprints (one per key it trusts): a node cache entry
// meets the decision when every group has at least one record.
func (d *Decision) AcceptableRecords(digest string) ([][]string, error) {
	var out [][]string
	for _, req := range d.Requirements {
		fps, err := req.AcceptableFingerprints(d.Repository, digest)
		if err != nil {
			return nil, err
		}
		out = append(out, fps)
	}
	return out, nil
}

// Cache compiles each policy once per generation.
type Cache struct {
	mu sync.Mutex
	m  map[types.UID]*Compiled
}

// Compiled returns policies compiled, reusing what was compiled for the
// same UID and generation, and forgetting policies no longer listed.
func (c *Cache) Compiled(policies []sandboxv1alpha1.SwiftArtifactVerificationPolicy) []*Compiled {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[types.UID]*Compiled, len(policies))
	out := make([]*Compiled, 0, len(policies))
	for i := range policies {
		p := &policies[i]
		cp, ok := c.m[p.UID]
		if !ok || cp.Generation != p.Generation || cp.Name != p.Name {
			cp = Compile(p)
		}
		next[p.UID] = cp
		out = append(out, cp)
	}
	c.m = next
	return out
}
