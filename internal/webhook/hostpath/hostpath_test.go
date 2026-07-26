package hostpath

import "testing"

func TestValidate_RejectsTraversalRegardlessOfAllowlist(t *testing.T) {
	// ".." is rejected even when the prefix check would otherwise pass — that is
	// the whole point of checking it separately. A prefix test is a string
	// comparison; "/var/lib/kubeswift/../../etc" satisfies HasPrefix.
	allowed := []string{"/var/lib/kubeswift"}
	for _, p := range []string{
		"/var/lib/kubeswift/../../etc",
		"/var/lib/kubeswift/..",
		"/../etc/kubernetes/pki",
		"/var/lib/kubeswift/a/../../../root",
	} {
		if err := Validate("f", p, allowed); err == nil {
			t.Errorf("accepted traversal path %q", p)
		}
	}
}

func TestValidate_EmptyAllowlistDeniesEverything(t *testing.T) {
	// The safe default: an install that has not opted in cannot mount ANY host
	// path, including otherwise-innocent ones.
	for _, p := range []string{"/", "/srv/vm", "/var/lib/kubeswift/x"} {
		if err := Validate("f", p, nil); err == nil {
			t.Errorf("empty allowlist accepted %q", p)
		}
	}
}

func TestValidate_TheOriginalExploit(t *testing.T) {
	// hostPath: "/" mounted RW into a tenant VM was the reported Critical.
	if err := Validate("spec.filesystems[0].source.hostPath", "/", []string{"/srv/vm"}); err == nil {
		t.Fatal("accepted hostPath / — the reported node-root escape")
	}
}

func TestValidate_PrefixIsSegmentAware(t *testing.T) {
	allowed := []string{"/srv/vm"}
	if err := Validate("f", "/srv/vmsecrets", allowed); err == nil {
		t.Error("accepted /srv/vmsecrets for prefix /srv/vm (naive HasPrefix bug)")
	}
	if err := Validate("f", "/srv/vm", allowed); err != nil {
		t.Errorf("rejected the prefix itself: %v", err)
	}
	if err := Validate("f", "/srv/vm/disks/a.raw", allowed); err != nil {
		t.Errorf("rejected a path under the prefix: %v", err)
	}
}

func TestValidate_RequiresAbsolute(t *testing.T) {
	if err := Validate("f", "srv/vm", []string{"/srv/vm"}); err == nil {
		t.Error("accepted a relative path")
	}
}

func TestValidateDir_ConfinesTheSocketDirectory(t *testing.T) {
	// The pod builder mounts filepath.Dir(socket), so THAT is what must be
	// confined — /etc/kubernetes/pki/x.sock would mount the PKI directory.
	allowed := []string{"/var/run/vhost"}
	if err := ValidateDir("spec.interfaces[0].socket", "/etc/kubernetes/pki/x.sock", allowed); err == nil {
		t.Error("accepted a socket whose directory is /etc/kubernetes/pki")
	}
	if err := ValidateDir("spec.interfaces[0].socket", "/var/run/vhost/net0.sock", allowed); err != nil {
		t.Errorf("rejected a legitimate vhost socket: %v", err)
	}
	if err := ValidateDir("f", "/var/run/vhost/../../etc/x.sock", allowed); err == nil {
		t.Error("accepted traversal in a socket path")
	}
}

func TestValidate_RootPrefixAllowsAll(t *testing.T) {
	// An operator who explicitly configures "/" gets what they asked for — the
	// allowlist is a policy knob, not a hard ceiling.
	if err := Validate("f", "/etc/kubernetes", []string{"/"}); err != nil {
		t.Errorf("explicit / prefix should permit any path: %v", err)
	}
}
