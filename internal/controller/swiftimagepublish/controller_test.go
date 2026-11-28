package swiftimagepublish

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

const ns = "team-a"

// testPublicKey is a PEM public key: a location's verifyKey must parse.
var testPublicKey = func() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}()

func snapFixtures(block bool) []client.Object {
	mode := corev1.PersistentVolumeFilesystem
	if block {
		mode = corev1.PersistentVolumeBlock
	}
	return []client.Object{
		&snapshotv1alpha1.SwiftSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "snap", UID: "snap-uid"},
			Spec: snapshotv1alpha1.SwiftSnapshotSpec{
				GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "web"},
				Backend:  snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot},
			},
			Status: snapshotv1alpha1.SwiftSnapshotStatus{
				Phase: snapshotv1alpha1.SwiftSnapshotPhaseReady,
				Disks: []snapshotv1alpha1.SnapshotDiskRef{{Role: "root", SizeBytes: 10 << 30, Handle: ns + "/vs-root"}},
			},
		},
		&volumesnapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "vs-root"},
			Status: &volumesnapshotv1.VolumeSnapshotStatus{
				ReadyToUse:                     ptr.To(true),
				RestoreSize:                    resource.NewQuantity(10<<30, resource.BinarySI),
				BoundVolumeSnapshotContentName: ptr.To("content-1"),
			},
		},
		&volumesnapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: "content-1"},
			Spec:       volumesnapshotv1.VolumeSnapshotContentSpec{SourceVolumeMode: &mode},
		},
	}
}

func guestFixtures() []client.Object {
	return []client.Object{
		&swiftv1alpha1.SwiftGuest{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
			Spec:       swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "win"}},
		},
		&imagev1alpha1.SwiftImage{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "win"},
			Spec:       imagev1alpha1.SwiftImageSpec{OSType: imagev1alpha1.OSTypeWindows},
		},
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "swiftguest-root-web"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("fast")},
		},
	}
}

func clusterLocation() client.Object {
	return &storagev1alpha1.SwiftClusterStorageLocation{
		ObjectMeta: metav1.ObjectMeta{Name: "registry"},
		Spec: storagev1alpha1.StorageLocationSpec{
			Default: true,
			OCI: &storagev1alpha1.OCILocation{
				Repository:           "registry.example.com/kubeswift",
				SigningKeySecretName: "signing",
				VerifyKey:            testPublicKey,
			},
		},
	}
}

func secrets() []client.Object {
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: storagev1alpha1.DefaultCredentialsSecretName},
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte("{}")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "signing"},
			Data: map[string][]byte{"cosign.key": []byte("k")}},
	}
}

func newPublish(mut func(*imagev1alpha1.SwiftImagePublish)) *imagev1alpha1.SwiftImagePublish {
	p := &imagev1alpha1.SwiftImagePublish{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-2026", UID: "pub-uid"},
		Spec: imagev1alpha1.SwiftImagePublishSpec{
			Source:      imagev1alpha1.ImagePublishSource{SnapshotRef: &corev1.LocalObjectReference{Name: "snap"}},
			Destination: imagev1alpha1.ImagePublishDestination{Image: "web", Tag: "2026.10"},
		},
	}
	if mut != nil {
		mut(p)
	}
	return p
}

func setup(t *testing.T, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&imagev1alpha1.SwiftImagePublish{}, &batchv1.Job{}).Build()
	return &Reconciler{Client: c, Scheme: scheme.Scheme, SnapshotORASImage: "snapshot-oras:test"}, c
}

func reconcileOnce(t *testing.T, r *Reconciler, name string) *imagev1alpha1.SwiftImagePublish {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var p imagev1alpha1.SwiftImagePublish
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func reason(p *imagev1alpha1.SwiftImagePublish) string {
	if c := meta.FindStatusCondition(p.Status.Conditions, ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func finishJob(t *testing.T, c client.Client, pub *imagev1alpha1.SwiftImagePublish, failed bool, msg string) {
	t.Helper()
	ctx := context.Background()
	var job batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName(pub)}, &job); err != nil {
		t.Fatal(err)
	}
	typ, phase, code := batchv1.JobComplete, corev1.PodSucceeded, int32(0)
	if failed {
		typ, phase, code = batchv1.JobFailed, corev1.PodFailed, 1
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: typ, Status: corev1.ConditionTrue, Message: "done"})
	if err := c.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: job.Name + "-x", Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{Name: "publish",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}}}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func TestPublish_CSISnapshotToClusterLocation(t *testing.T) {
	objs := append(snapFixtures(true), guestFixtures()...)
	objs = append(objs, secrets()...)
	objs = append(objs, clusterLocation(), newPublish(func(p *imagev1alpha1.SwiftImagePublish) {
		p.Spec.CreateImage = &imagev1alpha1.ImagePublishCreateImage{Name: "web-2026"}
	}))
	r, c := setup(t, objs...)
	ctx := context.Background()

	p := reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishPublishing {
		t.Fatalf("phase %s (%s)", p.Status.Phase, reason(p))
	}
	d := p.Status.Destination
	if d.Location != "SwiftClusterStorageLocation/registry" || d.Repository != "registry.example.com/kubeswift/team-a/images/web" ||
		d.CredentialsSecretName != storagev1alpha1.DefaultCredentialsSecretName || d.SigningKeySecretName != "signing" {
		t.Fatalf("destination %+v", d)
	}
	if p.Status.Reference != "registry.example.com/kubeswift/team-a/images/web:2026.10" {
		t.Fatalf("reference %s", p.Status.Reference)
	}
	s := p.Status.Source
	if !s.Block || s.OSType != imagev1alpha1.OSTypeWindows || s.SizeBytes != 10<<30 || s.VolumeSnapshot != "vs-root" {
		t.Fatalf("source %+v", s)
	}

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: diskPVCName(p)}, &pvc); err != nil {
		t.Fatal(err)
	}
	if *pvc.Spec.StorageClassName != "fast" || *pvc.Spec.VolumeMode != corev1.PersistentVolumeBlock || pvc.Spec.DataSource.Name != "vs-root" {
		t.Fatalf("disk pvc %+v", pvc.Spec)
	}

	var job batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName(p)}, &job); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{
		"--mode=upload-image", "--file=" + diskDevice, "--repository=registry.example.com/kubeswift/team-a/images/web",
		"--tag=2026.10", "--chunk-size-mib=128", "--os-type=windows", "--immutable-tag", "--sign-key=",
		"--annotation=io.kubeswift.source.kind=SwiftSnapshot", "--annotation=io.kubeswift.source.uid=snap-uid",
		"--annotation=io.kubeswift.publish.namespace=team-a",
		"--annotation=org.opencontainers.image.created=",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if fp := job.Spec.PodFailurePolicy; fp == nil || fp.Rules[0].OnExitCodes.Values[0] != 3 {
		t.Errorf("no FailJob rule for TagInUse: %+v", fp)
	}
	ps := job.Spec.Template.Spec
	sc := ps.Containers[0].SecurityContext
	if !*sc.RunAsNonRoot || *sc.RunAsUser == 0 || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation ||
		len(sc.Capabilities.Drop) != 1 || *ps.AutomountServiceAccountToken || job.Spec.ActiveDeadlineSeconds == nil {
		t.Errorf("job not hardened: %+v %+v", sc, ps)
	}
	for _, v := range ps.Volumes {
		if v.HostPath != nil {
			t.Errorf("hostPath volume %s", v.Name)
		}
		if v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("disk not read-only")
		}
	}

	rep, _ := json.Marshal(clonecommon.TransferReport{ManifestDigest: "sha256:abc", TotalBytes: 10<<30 + 1, TransferredBytes: 3 << 30, Signed: true})
	finishJob(t, c, p, false, string(rep))
	p = reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishReady || p.Status.Digest != "sha256:abc" || !p.Status.Signed || p.Status.Image != "web-2026" {
		t.Fatalf("status %+v (%s)", p.Status, reason(p))
	}
	var img imagev1alpha1.SwiftImage
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "web-2026"}, &img); err != nil {
		t.Fatal(err)
	}
	o := img.Spec.Source.OCI
	if o.Digest != "sha256:abc" || o.Tag != "" || o.Repository != d.Repository ||
		o.LocationRef == nil || o.LocationRef.Kind != storagev1alpha1.KindSwiftClusterStorageLocation || o.LocationRef.Name != "registry" {
		t.Fatalf("image source %+v", o)
	}
	if img.Spec.RootDisk.Size.Value() != 11<<30 || img.Spec.OSType != imagev1alpha1.OSTypeWindows || len(img.OwnerReferences) != 0 {
		t.Fatalf("image spec %+v owners %v", img.Spec, img.OwnerReferences)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&pvc), &pvc); !apierrors.IsNotFound(err) {
		t.Fatalf("disk pvc not deleted: %v", err)
	}
}

func TestPublish_Waits(t *testing.T) {
	cases := []struct {
		name   string
		objs   []client.Object
		mut    func(*imagev1alpha1.SwiftImagePublish)
		reason string
	}{
		{"no location", append(snapFixtures(false), guestFixtures()...), nil, "NoStorageLocation"},
		{"credentials missing", append(append(snapFixtures(false), guestFixtures()...), clusterLocation()), nil, "RegistryCredentialsMissing"},
		{"snapshot missing", append(secrets(), clusterLocation()), nil, ReasonSourceNotFound},
		{"guest gone, no class", append(append(snapFixtures(false), secrets()...), clusterLocation()), nil, ReasonStorageClassRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := setup(t, append(tc.objs, newPublish(tc.mut))...)
			p := reconcileOnce(t, r, "web-2026")
			if p.Status.Phase != imagev1alpha1.SwiftImagePublishPending || reason(p) != tc.reason {
				t.Fatalf("phase %s reason %s, want Pending %s", p.Status.Phase, reason(p), tc.reason)
			}
			var jobs batchv1.JobList
			_ = c.List(context.Background(), &jobs)
			if len(jobs.Items) != 0 {
				t.Fatal("a Job was created while waiting")
			}
		})
	}
}

func TestPublish_GuestGoneWithStorageClass(t *testing.T) {
	objs := append(snapFixtures(false), secrets()...)
	objs = append(objs, clusterLocation(), newPublish(func(p *imagev1alpha1.SwiftImagePublish) { p.Spec.StorageClassName = "slow" }))
	r, c := setup(t, objs...)
	p := reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishPublishing || p.Status.Source.OSType != imagev1alpha1.OSTypeLinux || p.Status.Source.Block {
		t.Fatalf("status %+v (%s)", p.Status, reason(p))
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: diskPVCName(p)}, &pvc); err != nil || *pvc.Spec.StorageClassName != "slow" {
		t.Fatalf("pvc class: %v %v", err, pvc.Spec.StorageClassName)
	}
}

func TestPublish_Refusals(t *testing.T) {
	notCSI := snapFixtures(false)
	notCSI[0].(*snapshotv1alpha1.SwiftSnapshot).Spec.Backend.Type = snapshotv1alpha1.SnapshotBackendLocal
	kernel := guestFixtures()
	kernel[0].(*swiftv1alpha1.SwiftGuest).Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	for name, objs := range map[string][]client.Object{
		"not csi":     notCSI,
		"kernel boot": append(snapFixtures(false), kernel...),
	} {
		t.Run(name, func(t *testing.T) {
			objs = append(objs, secrets()...)
			r, _ := setup(t, append(objs, clusterLocation(), newPublish(nil))...)
			p := reconcileOnce(t, r, "web-2026")
			if p.Status.Phase != imagev1alpha1.SwiftImagePublishFailed || reason(p) != ReasonSourceUnsupported {
				t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
			}
		})
	}
}

func TestPublish_ExplicitDestination(t *testing.T) {
	objs := append(snapFixtures(false), guestFixtures()...)
	objs = append(objs, newPublish(func(p *imagev1alpha1.SwiftImagePublish) {
		p.Spec.Destination = imagev1alpha1.ImagePublishDestination{
			Repository: "reg.local:5000/golden/web", Tag: "v1", Insecure: true, OverwriteTag: true,
			VerifyKeySecretRef: &imagev1alpha1.SecretObjectReference{Name: "pub"},
		}
		p.Spec.CreateImage = &imagev1alpha1.ImagePublishCreateImage{Name: "web"}
	}))
	r, c := setup(t, objs...)
	p := reconcileOnce(t, r, "web-2026")
	if p.Status.Destination.Location != LocationExplicit || p.Status.Destination.Repository != "reg.local:5000/golden/web" {
		t.Fatalf("destination %+v (%s)", p.Status.Destination, reason(p))
	}
	var job batchv1.Job
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: jobName(p)}, &job)
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if strings.Contains(args, "--immutable-tag") || strings.Contains(args, "--sign-key") || !strings.Contains(args, "--insecure") {
		t.Fatalf("args %s", args)
	}
	rep, _ := json.Marshal(clonecommon.TransferReport{ManifestDigest: "sha256:d", TotalBytes: 1 << 30})
	finishJob(t, c, p, false, string(rep))
	p = reconcileOnce(t, r, "web-2026")
	var img imagev1alpha1.SwiftImage
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web"}, &img); err != nil {
		t.Fatal(err)
	}
	o := img.Spec.Source.OCI
	if o.LocationRef != nil || !o.Insecure || o.VerifyKeySecretRef == nil || o.VerifyKeySecretRef.Name != "pub" {
		t.Fatalf("explicit image source %+v", o)
	}
}

func TestPublish_TagInUseAndImageExists(t *testing.T) {
	objs := append(snapFixtures(false), guestFixtures()...)
	objs = append(objs, secrets()...)
	r, c := setup(t, append(objs, clusterLocation(), newPublish(nil))...)
	p := reconcileOnce(t, r, "web-2026")
	finishJob(t, c, p, true, "snapshot-oras: tag in use: 2026.10 names sha256:old, not sha256:new")
	p = reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishFailed || reason(p) != ReasonTagInUse {
		t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
	}

	foreign := &imagev1alpha1.SwiftImage{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-2026"}}
	r, c = setup(t, append(objs, clusterLocation(), foreign, newPublish(func(p *imagev1alpha1.SwiftImagePublish) {
		p.Spec.CreateImage = &imagev1alpha1.ImagePublishCreateImage{Name: "web-2026"}
	}))...)
	p = reconcileOnce(t, r, "web-2026")
	rep, _ := json.Marshal(clonecommon.TransferReport{ManifestDigest: "sha256:abc", TotalBytes: 1 << 30, Signed: true})
	finishJob(t, c, p, false, string(rep))
	p = reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishFailed || reason(p) != ReasonImageExists || p.Status.Digest != "sha256:abc" {
		t.Fatalf("phase %s reason %s digest %s", p.Status.Phase, reason(p), p.Status.Digest)
	}
}

func TestPublish_UnsignedWithSigningKeyFails(t *testing.T) {
	objs := append(snapFixtures(false), guestFixtures()...)
	objs = append(objs, secrets()...)
	r, c := setup(t, append(objs, clusterLocation(), newPublish(nil))...)
	p := reconcileOnce(t, r, "web-2026")
	rep, _ := json.Marshal(clonecommon.TransferReport{ManifestDigest: "sha256:abc", TotalBytes: 1 << 30})
	finishJob(t, c, p, false, string(rep))
	p = reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishFailed {
		t.Fatalf("an unsigned image from a signing destination was accepted: %s", reason(p))
	}
}

func TestPublish_Queued(t *testing.T) {
	busy := func(name string) *imagev1alpha1.SwiftImagePublish {
		return &imagev1alpha1.SwiftImagePublish{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: name, UID: types.UID(name)},
			Status:     imagev1alpha1.SwiftImagePublishStatus{Phase: imagev1alpha1.SwiftImagePublishPublishing},
		}
	}
	objs := append(snapFixtures(false), guestFixtures()...)
	objs = append(objs, secrets()...)
	r, _ := setup(t, append(objs, clusterLocation(), busy("a"), busy("b"), newPublish(nil))...)
	p := reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishPending || reason(p) != ReasonQueued {
		t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
	}
	r.MaxConcurrent = 3
	p = reconcileOnce(t, r, "web-2026")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishPublishing {
		t.Fatalf("not started with a free slot: %s", reason(p))
	}
}

func guestSourceFixtures(phase swiftv1alpha1.SwiftGuestPhase) []client.Object {
	objs := guestFixtures()
	g := objs[0].(*swiftv1alpha1.SwiftGuest)
	g.Status.Phase = phase
	g.Spec.GuestClassRef = corev1.LocalObjectReference{Name: "plain"}
	pvc := objs[2].(*corev1.PersistentVolumeClaim)
	pvc.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}
	return append(objs, &swiftv1alpha1.SwiftGuestClass{ObjectMeta: metav1.ObjectMeta{Name: "plain"}})
}

func guestPublish(name string) *imagev1alpha1.SwiftImagePublish {
	return newPublish(func(p *imagev1alpha1.SwiftImagePublish) {
		p.Name = name
		p.UID = types.UID(name + "-uid")
		p.Spec.Source = imagev1alpha1.ImagePublishSource{GuestRef: &corev1.LocalObjectReference{Name: "web"}}
		p.Spec.Destination.Tag = name
	})
}

func holdOf(t *testing.T, c client.Client) string {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "web"}, &g); err != nil {
		t.Fatal(err)
	}
	return g.Annotations[imagev1alpha1.AnnotationPublishHold]
}

func TestPublish_GuestWaitsUntilStopped(t *testing.T) {
	objs := append(guestSourceFixtures(swiftv1alpha1.SwiftGuestPhaseRunning), secrets()...)
	r, c := setup(t, append(objs, clusterLocation(), guestPublish("g1"))...)
	reconcileOnce(t, r, "g1") // finalizer
	p := reconcileOnce(t, r, "g1")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishPending || reason(p) != ReasonGuestNotStopped {
		t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
	}
	if h := holdOf(t, c); h != "" {
		t.Fatalf("a running guest was held: %q", h)
	}
}

func TestPublish_GuestHeldReadAndReleased(t *testing.T) {
	objs := append(guestSourceFixtures(swiftv1alpha1.SwiftGuestPhaseStopped), secrets()...)
	r, c := setup(t, append(objs, clusterLocation(), guestPublish("g1"))...)
	ctx := context.Background()
	reconcileOnce(t, r, "g1") // finalizer
	p := reconcileOnce(t, r, "g1")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishPublishing {
		t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
	}
	if h := holdOf(t, c); h != "g1" {
		t.Fatalf("hold = %q", h)
	}
	if !hasFinalizer(p) {
		t.Fatal("no finalizer while holding a guest")
	}
	s := p.Status.Source
	if s.Kind != "SwiftGuest" || s.PVCName != "swiftguest-root-web" || s.SizeBytes != 20<<30 || s.OSType != imagev1alpha1.OSTypeWindows {
		t.Fatalf("source %+v", s)
	}
	var job batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName(p)}, &job); err != nil {
		t.Fatal(err)
	}
	if claim := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim; claim.ClaimName != "swiftguest-root-web" || !claim.ReadOnly {
		t.Fatalf("job reads %+v", claim)
	}
	var tmp corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: diskPVCName(p)}, &tmp); !apierrors.IsNotFound(err) {
		t.Fatalf("a temporary PVC was made for a guest source: %v", err)
	}

	// A second request for the same guest waits for the first.
	if err := c.Create(ctx, guestPublish("g2")); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "g2") // finalizer
	if p2 := reconcileOnce(t, r, "g2"); reason(p2) != ReasonGuestHeld {
		t.Fatalf("second request: %s %s", p2.Status.Phase, reason(p2))
	}

	rep, _ := json.Marshal(clonecommon.TransferReport{ManifestDigest: "sha256:g", TotalBytes: 20 << 30, Signed: true})
	finishJob(t, c, p, false, string(rep))
	p = reconcileOnce(t, r, "g1")
	if p.Status.Phase != imagev1alpha1.SwiftImagePublishReady {
		t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
	}
	if h := holdOf(t, c); h != "" {
		t.Fatalf("hold not released: %q", h)
	}
	if hasFinalizer(p) {
		t.Fatal("finalizer not removed")
	}
	if p2 := reconcileOnce(t, r, "g2"); p2.Status.Phase != imagev1alpha1.SwiftImagePublishPublishing || holdOf(t, c) != "g2" {
		t.Fatalf("second request did not take over: %s %s", p2.Status.Phase, reason(p2))
	}
}

func TestPublish_DeletedWhileReading(t *testing.T) {
	objs := append(guestSourceFixtures(swiftv1alpha1.SwiftGuestPhaseStopped), secrets()...)
	r, c := setup(t, append(objs, clusterLocation(), guestPublish("g1"))...)
	ctx := context.Background()
	reconcileOnce(t, r, "g1") // finalizer
	p := reconcileOnce(t, r, "g1")
	var job batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName(p)}, &job); err != nil {
		t.Fatal(err)
	}
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "reader", Labels: map[string]string{"job-name": job.Name}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if err := c.Create(ctx, running); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "g1")
	if h := holdOf(t, c); h != "g1" {
		t.Fatalf("hold released while a Job pod still reads the disk: %q", h)
	}
	running.Status.Phase = corev1.PodFailed
	if err := c.Status().Update(ctx, running); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "g1"}}); err != nil {
		t.Fatal(err)
	}
	if h := holdOf(t, c); h != "" {
		t.Fatalf("hold kept after the Job pods ended: %q", h)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(p), p); !apierrors.IsNotFound(err) {
		t.Fatalf("request not gone: %v", err)
	}
}

func TestPublish_GuestRefusals(t *testing.T) {
	shared := guestSourceFixtures(swiftv1alpha1.SwiftGuestPhaseStopped)
	shared[3].(*swiftv1alpha1.SwiftGuestClass).Spec.SharedBaseDisk = true
	kernel := guestSourceFixtures(swiftv1alpha1.SwiftGuestPhaseStopped)
	kernel[0].(*swiftv1alpha1.SwiftGuest).Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
	for name, objs := range map[string][]client.Object{"shared base": shared, "kernel boot": kernel} {
		t.Run(name, func(t *testing.T) {
			r, c := setup(t, append(append(objs, secrets()...), clusterLocation(), guestPublish("g1"))...)
			reconcileOnce(t, r, "g1") // finalizer
			p := reconcileOnce(t, r, "g1")
			if p.Status.Phase != imagev1alpha1.SwiftImagePublishFailed || reason(p) != ReasonSourceUnsupported {
				t.Fatalf("phase %s reason %s", p.Status.Phase, reason(p))
			}
			if h := holdOf(t, c); h != "" {
				t.Fatalf("refused guest was held: %q", h)
			}
		})
	}
}
