package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

const locSnapUID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

// locSnap is an oci snapshot that names no registry: a location supplies it.
func locSnap() *snapshotv1alpha1.SwiftSnapshot {
	s := ociSnap(nil)
	s.UID = locSnapUID
	s.Spec.Backend.OCI = nil
	return s
}

func clusterLocation(name string, isDefault bool, oci *storagev1alpha1.OCILocation, csi *storagev1alpha1.CSILocation) *storagev1alpha1.SwiftClusterStorageLocation {
	return &storagev1alpha1.SwiftClusterStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       storagev1alpha1.StorageLocationSpec{Default: isDefault, OCI: oci, CSI: csi},
	}
}

func namespaceLocation(ns, name string, isDefault bool, oci *storagev1alpha1.OCILocation, csi *storagev1alpha1.CSILocation) *storagev1alpha1.SwiftStorageLocation {
	return &storagev1alpha1.SwiftStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       storagev1alpha1.StorageLocationSpec{Default: isDefault, OCI: oci, CSI: csi},
	}
}

func registrySecret(ns, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}
}

// reconcileSnap runs one reconcile and returns the stored snapshot.
func reconcileSnap(t *testing.T, r *SwiftSnapshotReconciler, c client.Client, snap *snapshotv1alpha1.SwiftSnapshot) (snapshotv1alpha1.SwiftSnapshot, ctrl.Result) {
	t.Helper()
	key := types.NamespacedName{Namespace: snap.Namespace, Name: snap.Name}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	// The fake client accepts any condition; the apiserver does not.
	if errs := metav1validation.ValidateConditions(got.Status.Conditions, field.NewPath("status", "conditions")); len(errs) > 0 {
		t.Fatalf("the apiserver would refuse these conditions: %v", errs.ToAggregate())
	}
	return got, res
}

func readyReason(s *snapshotv1alpha1.SwiftSnapshot) (string, string) {
	if c := meta.FindStatusCondition(s.Status.Conditions, snapshotv1alpha1.SwiftSnapshotConditionReady); c != nil {
		return c.Reason, c.Message
	}
	return "", ""
}

// The cluster default supplies the registry: the snapshot records it (with
// its own namespace path and a per-snapshot tag) before anything is captured,
// and a later change to the default does not move it.
func TestLocation_ClusterDefaultRecordedBeforeCapture(t *testing.T) {
	snap := locSnap()
	main := clusterLocation("main", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift"}, nil)
	r, c := newReconciler(t, snap, main, registrySecret("team-a", storagev1alpha1.DefaultCredentialsSecretName))

	got, res := reconcileSnap(t, r, c, snap)
	want := snapshotv1alpha1.SnapshotLocation{
		Source:                "SwiftClusterStorageLocation/main",
		Repository:            "registry.example.com/kubeswift/team-a/snapshots",
		Tag:                   "snap1-0f1e2d3c",
		CredentialsSecretName: storagev1alpha1.DefaultCredentialsSecretName,
	}
	if got.Status.Location == nil || *got.Status.Location != want {
		t.Fatalf("status.location = %+v, want %+v", got.Status.Location, want)
	}
	if reason, _ := readyReason(&got); got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending || reason != ReasonLocationResolved {
		t.Errorf("phase %q reason %q, want Pending/%s", got.Status.Phase, reason, ReasonLocationResolved)
	}
	if got.Status.CaptureStartedAt != nil || res.RequeueAfter == 0 {
		t.Errorf("the capture must start only on the next reconcile (captureStartedAt=%v, requeue=%v)", got.Status.CaptureStartedAt, res.RequeueAfter)
	}

	// The default changes: the recorded location does not.
	var loc storagev1alpha1.SwiftClusterStorageLocation
	if err := c.Get(context.Background(), types.NamespacedName{Name: "main"}, &loc); err != nil {
		t.Fatal(err)
	}
	loc.Spec.OCI.Repository = "elsewhere.example.com/kubeswift"
	if err := c.Update(context.Background(), &loc); err != nil {
		t.Fatal(err)
	}
	got, _ = reconcileSnap(t, r, c, snap)
	if got.Status.Location == nil || *got.Status.Location != want {
		t.Errorf("after the default changed, status.location = %+v, want it unchanged", got.Status.Location)
	}

	// The push is built from the record, not from the spec or the location.
	args := strings.Join(buildOCIPushJob(&got, "img", "node-1").Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--repository=registry.example.com/kubeswift/team-a/snapshots") || !strings.Contains(args, "--tag=snap1-0f1e2d3c") {
		t.Errorf("push args = %q", args)
	}
}

// A namespace default wins over the cluster default and is used as written
// (no namespace segment); a namespace default that configures only a
// VolumeSnapshotClass leaves oci snapshots to the cluster default.
func TestLocation_NamespaceDefault(t *testing.T) {
	cluster := clusterLocation("main", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift", Anonymous: true}, nil)

	snap := locSnap()
	team := namespaceLocation("team-a", "team", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/team-a", CredentialsSecretName: "team-creds"}, nil)
	r, c := newReconciler(t, snap, cluster, team, registrySecret("team-a", "team-creds"))
	got, _ := reconcileSnap(t, r, c, snap)
	if l := got.Status.Location; l == nil || l.Source != "SwiftStorageLocation/team" || l.Repository != "registry.example.com/team-a/snapshots" || l.CredentialsSecretName != "team-creds" {
		t.Errorf("namespace default: status.location = %+v", got.Status.Location)
	}

	snap = locSnap()
	csiOnly := namespaceLocation("team-a", "team", true, nil, &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "fast"})
	r, c = newReconciler(t, snap, cluster, csiOnly)
	got, _ = reconcileSnap(t, r, c, snap)
	if l := got.Status.Location; l == nil || l.Source != "SwiftClusterStorageLocation/main" || l.CredentialsSecretName != "" {
		t.Errorf("csi-only namespace default: status.location = %+v, want the anonymous cluster default", got.Status.Location)
	}
}

// Whatever cannot resolve leaves the snapshot Pending with a reason that says
// what is missing and where, nothing recorded and the guest untouched.
func TestLocation_Waits(t *testing.T) {
	withRef := func(kind, name string) *snapshotv1alpha1.SwiftSnapshot {
		s := locSnap()
		s.Spec.Backend.LocationRef = &storagev1alpha1.StorageLocationRef{Kind: kind, Name: name}
		return s
	}
	keyless := registrySecret("team-a", storagev1alpha1.DefaultCredentialsSecretName)
	keyless.Data = map[string][]byte{"token": []byte("x")}
	reg := &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift"}
	cases := map[string]struct {
		snap   *snapshotv1alpha1.SwiftSnapshot
		objs   []client.Object
		reason string
		says   []string
	}{
		"no location at all": {locSnap(), nil, storagelocation.ReasonNone, []string{"spec.backend.locationRef", "team-a"}},
		"the credentials Secret is missing": {locSnap(), []client.Object{clusterLocation("main", true, reg, nil)},
			storagelocation.ReasonCredentialsMissing, []string{"kubeswift-registry", "namespace team-a"}},
		// A location holds Secret names; the controller only ever reads them in
		// the snapshot's own namespace.
		"the Secret exists only in another namespace": {locSnap(), []client.Object{clusterLocation("main", true, reg, nil), registrySecret("other", storagev1alpha1.DefaultCredentialsSecretName)},
			storagelocation.ReasonCredentialsMissing, []string{"namespace team-a"}},
		"the Secret has no .dockerconfigjson": {locSnap(), []client.Object{clusterLocation("main", true, reg, nil), keyless},
			storagelocation.ReasonCredentialsMissing, []string{"no .dockerconfigjson key"}},
		"the signing key Secret is missing": {locSnap(), []client.Object{
			clusterLocation("main", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift", Anonymous: true, SigningKeySecretName: "cosign"}, nil)},
			storagelocation.ReasonSigningKeyMissing, []string{"cosign", "namespace team-a"}},
		"a referenced location does not exist": {withRef("", "team"), nil, storagelocation.ReasonNotFound, []string{"SwiftStorageLocation team", "namespace team-a"}},
		"a referenced location exists only in another namespace": {withRef("", "team"),
			[]client.Object{namespaceLocation("other", "team", false, &storagev1alpha1.OCILocation{Repository: "registry.example.com/other", Anonymous: true}, nil)},
			storagelocation.ReasonNotFound, []string{"SwiftStorageLocation team"}},
		"a referenced location has no registry": {withRef(storagev1alpha1.KindSwiftClusterStorageLocation, "snapclass"),
			[]client.Object{clusterLocation("snapclass", false, nil, &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "fast"})},
			storagelocation.ReasonInvalid, []string{"configures no oci registry"}},
		"two cluster defaults": {locSnap(), []client.Object{clusterLocation("a", true, reg, nil), clusterLocation("b", true, reg, nil)},
			storagelocation.ReasonAmbiguous, []string{"a, b"}},
		"two namespace defaults": {locSnap(), []client.Object{
			namespaceLocation("team-a", "x", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/x", Anonymous: true}, nil),
			namespaceLocation("team-a", "y", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/y", Anonymous: true}, nil)},
			storagelocation.ReasonAmbiguous, []string{"x, y", "namespace team-a"}},
		"the default is malformed (webhook off)": {locSnap(), []client.Object{
			clusterLocation("main", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift:v1", Anonymous: true}, nil)},
			storagelocation.ReasonInvalid, []string{"tag or digest"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, c := newReconciler(t, append([]client.Object{tc.snap}, tc.objs...)...)
			got, res := reconcileSnap(t, r, c, tc.snap)
			reason, msg := readyReason(&got)
			if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending || reason != tc.reason {
				t.Fatalf("phase %q reason %q (%s), want Pending/%s", got.Status.Phase, reason, msg, tc.reason)
			}
			for _, s := range tc.says {
				if !strings.Contains(msg, s) {
					t.Errorf("message %q should mention %q", msg, s)
				}
			}
			if got.Status.Location != nil || got.Status.CaptureStartedAt != nil {
				t.Errorf("a waiting snapshot recorded a location (%+v) or started capturing", got.Status.Location)
			}
			if res.RequeueAfter == 0 {
				t.Error("a waiting snapshot must check again")
			}
		})
	}
}

// An explicit backend.oci is used exactly as written, and recorded too, so
// every snapshot restores from status.
func TestLocation_ExplicitBackendRecorded(t *testing.T) {
	snap := ociSnap(func(o *snapshotv1alpha1.OCIBackend) {
		o.Insecure = true
		o.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "zot-creds"}
	})
	// A default exists, and is ignored.
	r, c := newReconciler(t, snap, clusterLocation("main", true, &storagev1alpha1.OCILocation{Repository: "registry.example.com/kubeswift"}, nil))
	got, _ := reconcileSnap(t, r, c, snap)
	want := snapshotv1alpha1.SnapshotLocation{
		Source:                snapshotv1alpha1.SnapshotLocationExplicit,
		Repository:            "zot.svc:5000/vm-snapshots",
		Tag:                   "team-a-snap1",
		Insecure:              true,
		CredentialsSecretName: "zot-creds",
	}
	if got.Status.Location == nil || *got.Status.Location != want {
		t.Errorf("status.location = %+v, want %+v", got.Status.Location, want)
	}
}

// A csi snapshot records its VolumeSnapshotClass: its own, else a location's,
// else the cluster default class; with none at all it fails at once instead
// of leaving the snapshotter to fail later.
func TestLocation_CSIClass(t *testing.T) {
	csiSnap := func(class string) *snapshotv1alpha1.SwiftSnapshot {
		s := makeSwiftSnapshot("snap1", "team-a", "g1", class)
		s.UID = locSnapUID
		return s
	}
	defaultClass := &volumesnapshotv1.VolumeSnapshotClass{
		ObjectMeta: metav1.ObjectMeta{Name: "longhorn", Annotations: map[string]string{isDefaultClassAnnotation: "true"}},
		Driver:     "driver.longhorn.io", DeletionPolicy: volumesnapshotv1.VolumeSnapshotContentDelete,
	}
	cases := map[string]struct {
		snap       *snapshotv1alpha1.SwiftSnapshot
		objs       []client.Object
		wantSource string
		wantClass  string
		failReason string
	}{
		"its own class": {csiSnap("fast"), []client.Object{clusterLocation("main", true, nil, &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "slow"})},
			snapshotv1alpha1.SnapshotLocationExplicit, "fast", ""},
		"the cluster default location's class": {csiSnap(""), []client.Object{clusterLocation("main", true, nil, &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "slow"})},
			"SwiftClusterStorageLocation/main", "slow", ""},
		"the namespace default location's class": {csiSnap(""), []client.Object{
			clusterLocation("main", true, nil, &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "slow"}),
			namespaceLocation("team-a", "team", true, nil, &storagev1alpha1.CSILocation{VolumeSnapshotClassName: "team-class"})},
			"SwiftStorageLocation/team", "team-class", ""},
		"the cluster default class": {csiSnap(""), []client.Object{defaultClass}, snapshotv1alpha1.SnapshotLocationDefaultClass, "", ""},
		"no class anywhere":         {csiSnap(""), nil, "", "", ReasonNoVolumeSnapshotClass},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, c := newReconciler(t, append([]client.Object{tc.snap}, tc.objs...)...)
			r.VolumeSnapshotEnabled = true
			got, _ := reconcileSnap(t, r, c, tc.snap)
			if tc.failReason != "" {
				reason, msg := readyReason(&got)
				if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed || reason != tc.failReason || !strings.Contains(msg, "volumeSnapshotClassName") {
					t.Fatalf("phase %q reason %q (%s), want Failed/%s", got.Status.Phase, reason, msg, tc.failReason)
				}
				return
			}
			l := got.Status.Location
			if l == nil || l.Source != tc.wantSource || l.VolumeSnapshotClassName != tc.wantClass {
				t.Fatalf("status.location = %+v, want source %q class %q", l, tc.wantSource, tc.wantClass)
			}
			if reason, _ := readyReason(&got); reason == ReasonLocationResolved {
				t.Error("a csi snapshot goes on in the same reconcile")
			}
			if csiClassName(&got) != tc.wantClass {
				t.Errorf("csiClassName = %q, want %q", csiClassName(&got), tc.wantClass)
			}
		})
	}
}

// A location's CA bundle reaches every Job that talks to the registry: the
// push and the disk chunking with the bundle recorded at resolution, the
// deletion with the one it is given (TransferCA).
func TestLocation_CABundleReachesTheJobs(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"
	snap := locSnap()
	snap.Status.Location = &snapshotv1alpha1.SnapshotLocation{
		Source: "SwiftClusterStorageLocation/main", Repository: "registry.example.com/k/team-a/snapshots", Tag: "snap1-0f1e2d3c", CABundle: ca,
	}
	hasCA := func(env []corev1.EnvVar) bool {
		for _, e := range env {
			if e.Name == clonecommon.RegistryCAEnv && e.Value == ca {
				return true
			}
		}
		return false
	}
	if !hasCA(buildOCIPushJob(snap, "img", "node-1").Spec.Template.Spec.Containers[0].Env) {
		t.Error("push Job")
	}
	if !hasCA(buildChunkJob(snap, "img", "node-1", "j", "t-disk", "pvc", false).Spec.Template.Spec.Containers[0].Env) {
		t.Error("disk chunk Job")
	}
	del := buildOCIDeleteJob(snap, "img", []ociArtifact{{repository: "r", tag: "t", digest: "sha256:x"}}, ca)
	if !hasCA(del.Spec.Template.Spec.Containers[0].Env) {
		t.Error("delete Job")
	}
	snap.Status.Location.CABundle = ""
	if hasCA(buildOCIPushJob(snap, "img", "node-1").Spec.Template.Spec.Containers[0].Env) {
		t.Error("no recorded bundle, no variable")
	}
}
