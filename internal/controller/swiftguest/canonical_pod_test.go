package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func guestWithPodRef(name, podRefName string) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	if podRefName != "" {
		g.Status.PodRef = &corev1.ObjectReference{Name: podRefName, Namespace: "default"}
	}
	return g
}

func TestCanonicalPodName(t *testing.T) {
	cases := []struct {
		name       string
		guestName  string
		podRefName string
		want       string
	}{
		{"no podRef falls back to guest name", "vm", "", "vm"},
		{"podRef equals guest name", "vm", "vm", "vm"},
		{"podRef is migration-renamed pod", "vm", "vm-mig-511016", "vm-mig-511016"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalPodName(guestWithPodRef(tc.guestName, tc.podRefName))
			if got != tc.want {
				t.Errorf("canonicalPodName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStaleMigrationPodRef guards the TFU #18 secondary-trap decision:
// the create path clears status.PodRef only when it points at a pod
// other than guest.Name (a live-migration <guest>-mig-<uid> leftover).
// A nil PodRef or a PodRef already equal to guest.Name must NOT be
// treated as stale (clearing it would be a no-op but the intent is to
// fire exclusively on the renamed-pod case).
func TestStaleMigrationPodRef(t *testing.T) {
	cases := []struct {
		name       string
		guestName  string
		podRefName string
		want       bool
	}{
		{"nil podRef is not stale", "vm", "", false},
		{"podRef equals guest name is not stale", "vm", "vm", false},
		{"podRef is a migration-renamed pod is stale", "vm", "vm-mig-511016", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := staleMigrationPodRef(guestWithPodRef(tc.guestName, tc.podRefName))
			if got != tc.want {
				t.Errorf("staleMigrationPodRef = %v, want %v", got, tc.want)
			}
		})
	}
}
