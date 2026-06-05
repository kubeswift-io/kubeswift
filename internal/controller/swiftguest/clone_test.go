package swiftguest

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func cloneScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
	gvSnap := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSnap, &snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{})
	metav1.AddToGroupVersion(s, gvSnap)
	return s
}

func cloneGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-a", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			RunPolicy: swiftv1alpha1.RunPolicyRunning,
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{
				SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
			},
		},
	}
}

func sourceGuest() *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "rocky9"},
			GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
			RunPolicy:     swiftv1alpha1.RunPolicyStopped, // must NOT leak onto the clone
		},
	}
}

func localSnap(phase snapshotv1alpha1.SwiftSnapshotPhase) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "src"},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:  snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{HostPath: "/var/lib/kubeswift/snapshots/ns-snap"},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{Phase: phase, NodeName: "boba"},
	}
}

func newCloneReconciler(t *testing.T, objs ...client.Object) (*SwiftGuestReconciler, client.Client) {
	s := cloneScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &SwiftGuestReconciler{Client: c, Scheme: s}, c
}

func TestPrepareCloneFromSnapshot_TierB(t *testing.T) {
	g := cloneGuest()
	r, c := newCloneReconciler(t, g, sourceGuest(), localSnap(snapshotv1alpha1.SwiftSnapshotPhaseReady))
	eff, fail, requeue, err := r.prepareCloneFromSnapshot(context.Background(), g)
	if err != nil || fail != "" || requeue {
		t.Fatalf("expected success; fail=%q requeue=%v err=%v", fail, requeue, err)
	}
	// effective guest carries the SOURCE spec (imageRef) but the CLONE runPolicy.
	if eff.Spec.ImageRef == nil || eff.Spec.ImageRef.Name != "rocky9" {
		t.Errorf("effective spec must carry source imageRef; got %+v", eff.Spec.ImageRef)
	}
	if eff.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("effective runPolicy = %q, want the clone's Running (not the source's Stopped)", eff.Spec.RunPolicy)
	}
	// restore annotations stamped on the real guest, in-cluster + in-memory.
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "clone-a", Namespace: "ns"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[AnnotationActiveRestore] != "snap" ||
		got.Annotations[AnnotationRestoreMode] != RestoreModeClone ||
		got.Annotations[AnnotationRestoreNodeName] != "boba" ||
		got.Annotations[AnnotationRestoreSnapshotPath] != "/var/lib/kubeswift/snapshots/ns-snap" ||
		got.Annotations[AnnotationRestoreMACRewrites] == "" ||
		got.Annotations[AnnotationRestoreNullifyHostMAC] != "true" {
		t.Errorf("clone restore annotations not stamped: %+v", got.Annotations)
	}
	if g.Annotations[AnnotationActiveRestore] != "snap" {
		t.Errorf("in-memory guest annotations not updated: %+v", g.Annotations)
	}
}

func TestPrepareCloneFromSnapshot_NotReady(t *testing.T) {
	g := cloneGuest()
	r, _ := newCloneReconciler(t, g, sourceGuest(), localSnap(snapshotv1alpha1.SwiftSnapshotPhaseCapturing))
	_, fail, requeue, err := r.prepareCloneFromSnapshot(context.Background(), g)
	if err != nil || fail != "" || !requeue {
		t.Fatalf("not-Ready snapshot should requeue; fail=%q requeue=%v err=%v", fail, requeue, err)
	}
}

func s3CloneSnap() *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "src"},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendS3,
				S3:   &snapshotv1alpha1.S3Backend{Bucket: "bk", CredentialsSecretRef: &snapshotv1alpha1.SecretObjectReference{Name: "c"}},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase: snapshotv1alpha1.SwiftSnapshotPhaseReady,
			S3:    &snapshotv1alpha1.S3SnapshotStatus{Location: "s3://bk/ns/snap/"},
		},
	}
}

func TestPrepareCloneFromSnapshot_TierC_RequiresTargetNode(t *testing.T) {
	g := cloneGuest() // no targetNode
	r, _ := newCloneReconciler(t, g, sourceGuest(), s3CloneSnap())
	r.SnapshotS3Image = "img"
	_, fail, _, err := r.prepareCloneFromSnapshot(context.Background(), g)
	if err != nil || !strings.Contains(fail, "requires spec.cloneFromSnapshot.targetNode") {
		t.Fatalf("Tier C without targetNode should fail; fail=%q err=%v", fail, err)
	}
}

func TestPrepareCloneFromSnapshot_TierC_DownloadsThenProceeds(t *testing.T) {
	g := cloneGuest()
	g.Spec.CloneFromSnapshot.TargetNode = "miles"
	r, c := newCloneReconciler(t, g, sourceGuest(), s3CloneSnap())
	r.SnapshotS3Image = "img"

	// First pass: creates the download Job + requeues (not yet complete).
	_, fail, requeue, err := r.prepareCloneFromSnapshot(context.Background(), g)
	if err != nil || fail != "" || !requeue {
		t.Fatalf("first pass should create the download Job + requeue; fail=%q requeue=%v err=%v", fail, requeue, err)
	}
	var job batchv1.Job
	wantJob := cloneDownloadJobName(s3CloneSnap(), "miles")
	if err := c.Get(context.Background(), client.ObjectKey{Name: wantJob, Namespace: "ns"}, &job); err != nil {
		t.Fatalf("download Job not created: %v", err)
	}
	if job.Spec.Template.Spec.NodeName != "miles" {
		t.Errorf("download Job must pin to targetNode miles; got %q", job.Spec.Template.Spec.NodeName)
	}
	// The shared Job is owned by the clone guest that created it.
	if oc := metav1.GetControllerOf(&job); oc == nil || oc.Kind != "SwiftGuest" || oc.Name != "clone-a" {
		t.Errorf("download Job must be owned by the creating SwiftGuest; got %+v", job.OwnerReferences)
	}

	// Mark the Job complete; second pass should proceed (effective + stamped).
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}
	eff, fail, requeue, err := r.prepareCloneFromSnapshot(context.Background(), g)
	if err != nil || fail != "" || requeue || eff == nil {
		t.Fatalf("after download completes, should proceed; fail=%q requeue=%v err=%v", fail, requeue, err)
	}
	if g.Annotations[AnnotationRestoreNodeName] != "miles" ||
		g.Annotations[AnnotationRestoreSnapshotPath] != "/var/lib/kubeswift/snapshots/ns-snap" {
		t.Errorf("Tier C restore annotations wrong: %+v", g.Annotations)
	}
}

func TestCloneDownloadJobName_PerNodeSnapshot(t *testing.T) {
	snap := s3CloneSnap()
	// Deterministic + DNS-1123 valid + guest-independent.
	if a, b := cloneDownloadJobName(snap, "miles"), cloneDownloadJobName(snap, "miles"); a != b {
		t.Errorf("name must be deterministic; got %q vs %q", a, b)
	}
	if !strings.HasPrefix(cloneDownloadJobName(snap, "miles"), "clone-dl-") {
		t.Errorf("name must carry the clone-dl- prefix; got %q", cloneDownloadJobName(snap, "miles"))
	}
	// Different node → different Job (each node's cache is a separate hostPath).
	if cloneDownloadJobName(snap, "miles") == cloneDownloadJobName(snap, "boba") {
		t.Error("different nodes must yield different download Job names")
	}
	// Different snapshot → different Job.
	other := s3CloneSnap()
	other.Name = "snap2"
	if cloneDownloadJobName(snap, "miles") == cloneDownloadJobName(other, "miles") {
		t.Error("different snapshots must yield different download Job names")
	}
}

// TestEnsureCloneDownloadJob_DedupPerNodeSnapshot proves two clones on the same
// node from the same snapshot converge on ONE shared download Job — the fix that
// lifts the "replicas <= schedulable nodes" constraint.
func TestEnsureCloneDownloadJob_DedupPerNodeSnapshot(t *testing.T) {
	snap := s3CloneSnap()
	gA := cloneGuest() // "clone-a"
	gB := cloneGuest() // a second, distinct clone
	gB.Name = "clone-b"
	r, c := newCloneReconciler(t, gA, gB, snap)
	r.SnapshotS3Image = "img"

	// Two distinct clone guests, both targeting node "miles".
	for _, g := range []*swiftv1alpha1.SwiftGuest{gA, gB} {
		done, fail, err := r.ensureCloneDownloadJob(context.Background(), g, snap, "miles")
		if err != nil || fail != "" || done {
			t.Fatalf("%s: expected in-progress (not done, no fail); done=%v fail=%q err=%v", g.Name, done, fail, err)
		}
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly ONE shared download Job for (miles, snap); got %d: %v", len(jobs.Items), jobNames(jobs))
	}
	job := jobs.Items[0]
	if job.Name != cloneDownloadJobName(snap, "miles") {
		t.Errorf("shared Job name = %q, want %q", job.Name, cloneDownloadJobName(snap, "miles"))
	}
	// Owned by the race-winner (clone-a, created first here); the sibling reads it.
	if oc := metav1.GetControllerOf(&job); oc == nil || oc.Kind != "SwiftGuest" || oc.Name != "clone-a" {
		t.Errorf("shared Job must be owned by the creating SwiftGuest; got %+v", job.OwnerReferences)
	}

	// A clone on a DIFFERENT node gets its own Job (separate node-local cache).
	if _, _, err := r.ensureCloneDownloadJob(context.Background(), gA, snap, "boba"); err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &jobs, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 {
		t.Fatalf("a clone on a second node must add a second Job; got %d: %v", len(jobs.Items), jobNames(jobs))
	}
}

func jobNames(l batchv1.JobList) []string {
	out := make([]string, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, l.Items[i].Name)
	}
	return out
}

func TestPrepareCloneFromSnapshot_SourceGone(t *testing.T) {
	g := cloneGuest()
	r, _ := newCloneReconciler(t, g, localSnap(snapshotv1alpha1.SwiftSnapshotPhaseReady)) // no source guest
	_, fail, _, err := r.prepareCloneFromSnapshot(context.Background(), g)
	if err != nil || !strings.Contains(fail, "no longer exists") {
		t.Fatalf("missing source should fail; fail=%q err=%v", fail, err)
	}
}

func TestCloneRegenIncludesNonMAC(t *testing.T) {
	cases := []struct {
		name string
		in   []swiftv1alpha1.CloneIdentityItem
		want bool
	}{
		{"empty defaults to all", nil, true},
		{"only macAddresses", []swiftv1alpha1.CloneIdentityItem{swiftv1alpha1.CloneRegenMACAddresses}, false},
		{"includes hostname", []swiftv1alpha1.CloneIdentityItem{swiftv1alpha1.CloneRegenMACAddresses, swiftv1alpha1.CloneRegenHostname}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cloneRegenIncludesNonMAC(&swiftv1alpha1.CloneFromSnapshotSource{Regenerate: tc.in})
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResumeCloneIfNeeded(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "clone-a", Namespace: "ns"}}
	r := poolCloneReconciler(t, pod)
	g := cloneGuest()

	// First call: stamps the resume action onto the pod.
	if err := r.resumeCloneIfNeeded(context.Background(), pod, g); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[cloneActionKey] != cloneVerbResume || pod.Annotations[cloneActionIDKey] != "clone-a-clone-resume" {
		t.Fatalf("resume action not stamped: %+v", pod.Annotations)
	}
	// Second call (action-id already set): no-op (idempotent).
	before := pod.Annotations[cloneActionIDKey]
	if err := r.resumeCloneIfNeeded(context.Background(), pod, g); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[cloneActionIDKey] != before {
		t.Errorf("resume should be idempotent once the action-id is set")
	}
}

func poolCloneReconciler(t *testing.T, objs ...client.Object) *SwiftGuestReconciler {
	t.Helper()
	s := cloneScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &SwiftGuestReconciler{Client: c, Scheme: s}
}
