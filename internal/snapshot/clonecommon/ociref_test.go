package clonecommon

import (
	"context"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func ociRefSnap(ns, name, repo, tag string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendOCI,
				OCI:  &snapshotv1alpha1.OCIBackend{Repository: repo, Tag: tag},
			},
		},
	}
}

func TestOCIReference(t *testing.T) {
	if got := OCIReference(ociRefSnap("team-a", "s1", "reg/vm", "")); got != "reg/vm:team-a-s1" {
		t.Errorf("default tag: %q", got)
	}
	if got := OCIReference(ociRefSnap("team-a", "s1", "reg/vm", "nightly")); got != "reg/vm:nightly" {
		t.Errorf("explicit tag: %q", got)
	}
	pushed := ociRefSnap("team-a", "s1", "reg/edited", "nightly")
	pushed.Status.OCI = &snapshotv1alpha1.OCISnapshotStatus{Reference: "reg/vm:nightly"}
	if got := OCIReference(pushed); got != "reg/vm:nightly" {
		t.Errorf("a pushed snapshot is where it pushed, not what its spec says now: %q", got)
	}
	if got := OCIReference(ociRefSnap("team-a", "s1", "", "")); got != "" {
		t.Errorf("no repository: %q", got)
	}
	local := ociRefSnap("team-a", "s1", "reg/vm", "")
	local.Spec.Backend.Type = snapshotv1alpha1.SnapshotBackendLocal
	if got := OCIReference(local); got != "" {
		t.Errorf("not oci: %q", got)
	}
}

// Only live snapshots in the same namespace that push to the same
// repository:tag hold it (#705).
func TestOCIReferenceHolders(t *testing.T) {
	s := runtime.NewScheme()
	gv := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gv, &snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{})
	metav1.AddToGroupVersion(s, gv)

	same := ociRefSnap("team-a", "same", "reg/vm", "nightly")
	otherTag := ociRefSnap("team-a", "other-tag", "reg/vm", "weekly")
	otherRepo := ociRefSnap("team-a", "other-repo", "reg/other", "nightly")
	otherNS := ociRefSnap("team-b", "other-ns", "reg/vm", "nightly")
	deleting := ociRefSnap("team-a", "deleting", "reg/vm", "nightly")
	deleting.Finalizers = []string{"kubeswift.io/test"}
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	failed := ociRefSnap("team-a", "failed", "reg/vm", "nightly")
	failed.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseFailed
	failedPushed := ociRefSnap("team-a", "failed-pushed", "reg/vm", "nightly")
	failedPushed.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseFailed
	failedPushed.Status.OCI = &snapshotv1alpha1.OCISnapshotStatus{Reference: "reg/vm:nightly"}
	self := ociRefSnap("team-a", "self", "reg/vm", "nightly")

	objs := []client.Object{same, otherTag, otherRepo, otherNS, deleting, failed, failedPushed, self}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).Build()

	holders, err := OCIReferenceHolders(context.Background(), c, self)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range holders {
		names = append(names, h.Name)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "failed-pushed,same" {
		t.Errorf("holders = %s, want failed-pushed,same", got)
	}
}

// Tags stay within the OCI limit of 128, and two long tags that share their
// first 119 characters still differ.
func TestTruncateTag(t *testing.T) {
	if got := TruncateTag("short"); got != "short" {
		t.Errorf("a short tag changed: %q", got)
	}
	a, b := strings.Repeat("a", 200)+"-1", strings.Repeat("a", 200)+"-2"
	ta, tb := TruncateTag(a), TruncateTag(b)
	if len(ta) != 128 || len(tb) != 128 || ta == tb {
		t.Errorf("truncated tags: %d %d equal=%v", len(ta), len(tb), ta == tb)
	}
	if TruncateTag(a) != ta {
		t.Error("truncation must be deterministic")
	}
}

func TestLocationOCITag(t *testing.T) {
	s := ociRefSnap("team-a", "nightly", "", "")
	s.UID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	if got := LocationOCITag(s); got != "nightly-0f1e2d3c" {
		t.Errorf("LocationOCITag = %q", got)
	}
	s.Name = strings.Repeat("n", 253)
	if got := LocationOCITag(s); len(got) > 128 {
		t.Errorf("a 253-character name gives a %d-character tag", len(got))
	}
	// The explicit default tag is truncated the same way.
	s.Spec.Backend.OCI.Repository = "reg/vm"
	if got := ExplicitOCITag(s); len(got) > 128 {
		t.Errorf("explicit default tag is %d characters", len(got))
	}
}

// status.location wins over the spec; a snapshot without one (taken before
// storage locations) reads its spec; one with neither has no registry.
func TestSnapshotOCI(t *testing.T) {
	s := ociRefSnap("team-a", "s1", "zot.svc:5000/vm", "")
	s.Spec.Backend.OCI.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "zot-creds"}
	if c, ok := SnapshotOCI(s); !ok || c.Repository != "zot.svc:5000/vm" || c.Tag != "team-a-s1" || c.CredentialsSecretName != "zot-creds" {
		t.Errorf("legacy snapshot: %+v ok=%v", c, ok)
	}
	s.Status.Location = &snapshotv1alpha1.SnapshotLocation{Source: "SwiftClusterStorageLocation/main", Repository: "registry.example.com/k/team-a/snapshots", Tag: "s1-12345678", CABundle: "pem"}
	if c, ok := SnapshotOCI(s); !ok || c.Repository != "registry.example.com/k/team-a/snapshots" || c.Tag != "s1-12345678" || c.CredentialsSecretName != "" || c.CABundle != "pem" {
		t.Errorf("recorded location: %+v ok=%v", c, ok)
	}
	if got := OCIReference(s); got != "registry.example.com/k/team-a/snapshots:s1-12345678" {
		t.Errorf("OCIReference = %q", got)
	}
	none := ociRefSnap("team-a", "s1", "", "")
	none.Spec.Backend.OCI = nil
	if _, ok := SnapshotOCI(none); ok || OCIReference(none) != "" {
		t.Error("a snapshot with no registry yet must report none")
	}
}
