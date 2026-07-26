package swiftguest

import (
	"testing"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// The webhook is the primary gate but defaults to disabled (it needs
// cert-manager), so the controller must refuse an unconfined host path too --
// otherwise a default install has no enforcement at all.
func TestCheckHostPaths_EnforcedWithoutTheWebhook(t *testing.T) {
	hp := "/"
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		Filesystems: []swiftv1alpha1.Filesystem{{
			Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &hp},
		}},
	}}
	if err := checkHostPaths(g, []string{"/srv/vm"}); err == nil {
		t.Fatal("controller accepted hostPath / — the webhook is not the only gate")
	}
	ok := "/srv/vm/share"
	g.Spec.Filesystems[0].Source.HostPath = &ok
	if err := checkHostPaths(g, []string{"/srv/vm"}); err != nil {
		t.Errorf("rejected an allowed path: %v", err)
	}
	// Empty allowlist denies, matching the webhook's posture.
	if err := checkHostPaths(g, nil); err == nil {
		t.Error("empty allowlist should deny")
	}
}
