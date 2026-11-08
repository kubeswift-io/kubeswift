package swiftvolumeattachment

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/hotplug"
)

const ns = "team"

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gv := schema.GroupVersion{Group: swiftv1alpha1.GroupName, Version: swiftv1alpha1.Version}
	s.AddKnownTypes(gv,
		&swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{},
		&swiftv1alpha1.SwiftVolumeAttachment{}, &swiftv1alpha1.SwiftVolumeAttachmentList{})
	metav1.AddToGroupVersion(s, gv)
	return s
}

func guest(agent bool) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: ns, UID: "guest-uid"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestAgent: &swiftv1alpha1.GuestAgentSpec{Enabled: agent}},
	}
}

func launcher(features string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vm", Namespace: ns, UID: "launcher-uid",
			Labels:      map[string]string{guestLabel: "vm"},
			Annotations: map[string]string{launcherFeaturesAnnotation: features},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "swift.kubeswift.io/v1alpha1", Kind: "SwiftGuest", Name: "vm", UID: "guest-uid", Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "launcher", Image: "launcher:v1"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func claim(name string, block bool, pv string) *corev1.PersistentVolumeClaim {
	mode := corev1.PersistentVolumeFilesystem
	if block {
		mode = corev1.PersistentVolumeBlock
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeMode:  &mode,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:  pv,
		},
	}
}

func csiPV(name string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.example", VolumeHandle: name},
		}},
	}
}

func attachment(name, pvc string, created time.Time) *swiftv1alpha1.SwiftVolumeAttachment {
	return &swiftv1alpha1.SwiftVolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID("0123abcd-4567-89ef-0123-" + name + "00000000")[:36],
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: swiftv1alpha1.SwiftVolumeAttachmentSpec{
			GuestRef: corev1.LocalObjectReference{Name: "vm"},
			PVCRef:   corev1.LocalObjectReference{Name: pvc},
		},
	}
}

type env struct {
	t *testing.T
	c client.Client
	r *Reconciler
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	c := fake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&swiftv1alpha1.SwiftVolumeAttachment{}, &corev1.Pod{}).
		// The API server assigns a UID on create; the fake client does not.
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, o, opts...); err != nil {
				return err
			}
			if o.GetUID() == "" {
				o.SetUID(types.UID("uid-" + o.GetName()))
				return c.Update(ctx, o)
			}
			return nil
		}}).
		WithIndex(&swiftv1alpha1.SwiftVolumeAttachment{}, GuestIndex, func(o client.Object) []string {
			return []string{o.(*swiftv1alpha1.SwiftVolumeAttachment).Spec.GuestRef.Name}
		}).Build()
	return &env{t: t, c: c, r: &Reconciler{Client: c}}
}

func (e *env) reconcile() {
	e.t.Helper()
	if _, err := e.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "vm"}}); err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
}

func (e *env) sva(name string) *swiftv1alpha1.SwiftVolumeAttachment {
	e.t.Helper()
	var sva swiftv1alpha1.SwiftVolumeAttachment
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &sva); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		e.t.Fatal(err)
	}
	return &sva
}

func (e *env) attachmentPods(name string) []corev1.Pod {
	e.t.Helper()
	var pods corev1.PodList
	if err := e.c.List(context.Background(), &pods, client.InNamespace(ns), client.MatchingLabels{swiftv1alpha1.LabelVolumeAttachment: name}); err != nil {
		e.t.Fatal(err)
	}
	return pods.Items
}

func (e *env) launcher() *corev1.Pod {
	e.t.Helper()
	var p corev1.Pod
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "vm"}, &p); err != nil {
		e.t.Fatal(err)
	}
	return &p
}

func (e *env) setPodPhase(p *corev1.Pod, phase corev1.PodPhase) {
	e.t.Helper()
	p.Status.Phase = phase
	if err := e.c.Status().Update(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

// report writes the launcher's report for the set now on the launcher.
func (e *env) report(sts ...hotplug.Status) {
	e.t.Helper()
	p := e.launcher()
	b := mustJSON(sts)
	if sts == nil {
		b = []byte("[]")
	}
	p.Annotations[hotplug.AnnotationStatus] = string(b)
	p.Annotations[hotplug.AnnotationObserved] = hotplug.SetHash(p.Annotations[hotplug.AnnotationVolumes])
	if err := e.c.Update(context.Background(), p); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) set() []hotplug.Volume {
	e.t.Helper()
	vols, err := hotplug.DecodeVolumes(e.launcher().Annotations[hotplug.AnnotationVolumes])
	if err != nil {
		e.t.Fatal(err)
	}
	return vols
}

func reason(sva *swiftv1alpha1.SwiftVolumeAttachment) string {
	if c := apimeta.FindStatusCondition(sva.Status.Conditions, swiftv1alpha1.VolumeAttachmentConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func TestAttachThenDetach(t *testing.T) {
	now := time.Now()
	e := newEnv(t, guest(true), launcher("artifact-requirements hotplug-volumes"),
		claim("data", true, "pv-data"), csiPV("pv-data"), attachment("data", "data", now))

	e.reconcile()
	sva := e.sva("data")
	if !strings.HasPrefix(sva.Status.Serial, "ks") || len(sva.Status.Serial) != 20 {
		t.Fatalf("serial %q", sva.Status.Serial)
	}
	if sva.Status.Phase != swiftv1alpha1.VolumeAttachmentAttaching || reason(sva) != swiftv1alpha1.VolumeAttachmentReasonAttachingToNode {
		t.Fatalf("want Attaching/AttachingToNode, got %s/%s", sva.Status.Phase, reason(sva))
	}
	pods := e.attachmentPods("data")
	if len(pods) != 1 || pods[0].Spec.NodeName != "node-a" || len(sva.Status.AttachmentPods) != 1 || sva.Status.AttachmentPods[0].UID != pods[0].UID {
		t.Fatalf("want one recorded attachment pod on node-a; pods=%v status=%v", len(pods), sva.Status.AttachmentPods)
	}
	if c := pods[0].Spec.Containers[0]; c.Image != "launcher:v1" || *c.SecurityContext.AllowPrivilegeEscalation || pods[0].Spec.SecurityContext.RunAsNonRoot == nil {
		t.Fatalf("attachment pod not hardened or wrong image: %+v", c)
	}
	if len(e.set()) != 0 {
		t.Fatalf("no set before the attachment pod runs; got %v", e.set())
	}

	e.setPodPhase(&pods[0], corev1.PodRunning)
	e.reconcile()
	set := e.set()
	if len(set) != 1 || set[0].PVName != "pv-data" || set[0].AttachmentPodUID != pods[0].UID || set[0].Serial != sva.Status.Serial {
		t.Fatalf("set %+v", set)
	}
	if got := e.sva("data"); reason(got) != swiftv1alpha1.VolumeAttachmentReasonPlugging {
		t.Fatalf("want Plugging, got %s", reason(got))
	}

	e.report(hotplug.Status{Name: "data", State: hotplug.StateAttached})
	e.reconcile()
	if got := e.sva("data"); got.Status.Phase != swiftv1alpha1.VolumeAttachmentAttached {
		t.Fatalf("want Attached, got %s", got.Status.Phase)
	}

	// Detach: out of the set, the pod stays until the launcher reports.
	if err := e.c.Delete(context.Background(), e.sva("data")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if len(e.set()) != 0 || len(e.attachmentPods("data")) != 1 {
		t.Fatalf("want empty set and the pod kept; set=%v pods=%d", e.set(), len(e.attachmentPods("data")))
	}
	if got := e.sva("data"); got.Status.Phase != swiftv1alpha1.VolumeAttachmentDetaching {
		t.Fatalf("want Detaching, got %s", got.Status.Phase)
	}

	// The launcher reports InUse: still held.
	e.report(hotplug.Status{Name: "data", State: hotplug.StateInUse, Message: "mounted at /srv"})
	e.reconcile()
	got := e.sva("data")
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, swiftv1alpha1.VolumeAttachmentConditionInUse) || len(e.attachmentPods("data")) != 1 {
		t.Fatalf("want InUse and the pod kept")
	}

	// Released: the pod goes, then the finalizer.
	e.report()
	e.reconcile()
	if len(e.attachmentPods("data")) != 0 {
		t.Fatalf("want the attachment pod deleted")
	}
	e.reconcile()
	if e.sva("data") != nil {
		t.Fatalf("want the attachment gone once its pod is")
	}
}

// A report from before the set changed says nothing about the new set: the
// launcher may be plugging the volume in right now.
func TestUnsettledReportHoldsThePod(t *testing.T) {
	e := newEnv(t, guest(true), launcher("hotplug-volumes"),
		claim("data", true, "pv-data"), csiPV("pv-data"), attachment("data", "data", time.Now()))
	e.reconcile()
	e.setPodPhase(&e.attachmentPods("data")[0], corev1.PodRunning)
	e.reconcile() // set written; the launcher has not reported for it
	if err := e.c.Delete(context.Background(), e.sva("data")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.reconcile()
	if len(e.attachmentPods("data")) != 1 {
		t.Fatalf("the pod must be kept until the launcher reports for the current set")
	}
}

func TestRefusals(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		objs   []client.Object
		phase  swiftv1alpha1.VolumeAttachmentPhase
		reason string
	}{
		{"no guest agent", []client.Object{guest(false), launcher("hotplug-volumes"), claim("data", true, "")},
			swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonVolumeUnsupported},
		{"filesystem claim", []client.Object{guest(true), launcher("hotplug-volumes"), claim("data", false, "")},
			swiftv1alpha1.VolumeAttachmentFailed, swiftv1alpha1.VolumeAttachmentReasonVolumeUnsupported},
		{"no claim", []client.Object{guest(true), launcher("hotplug-volumes")},
			swiftv1alpha1.VolumeAttachmentPending, swiftv1alpha1.VolumeAttachmentReasonPVCNotFound},
		{"old launcher", []client.Object{guest(true), launcher("artifact-requirements"), claim("data", true, "")},
			swiftv1alpha1.VolumeAttachmentPending, swiftv1alpha1.VolumeAttachmentReasonLauncherUnsupported},
		{"no launcher", []client.Object{guest(true), claim("data", true, "")},
			swiftv1alpha1.VolumeAttachmentPending, swiftv1alpha1.VolumeAttachmentReasonGuestNotRunning},
		{"no guest", []client.Object{claim("data", true, "")},
			swiftv1alpha1.VolumeAttachmentPending, swiftv1alpha1.VolumeAttachmentReasonGuestNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, append(tc.objs, attachment("data", "data", now))...)
			e.reconcile()
			sva := e.sva("data")
			if sva.Status.Phase != tc.phase || reason(sva) != tc.reason {
				t.Fatalf("want %s/%s, got %s/%s", tc.phase, tc.reason, sva.Status.Phase, reason(sva))
			}
			if len(e.attachmentPods("data")) != 0 {
				t.Fatalf("no attachment pod for a refused attachment")
			}
		})
	}
}

func TestSecondAttachmentOfTheSameClaimFails(t *testing.T) {
	now := time.Now()
	e := newEnv(t, guest(true), launcher("hotplug-volumes"), claim("data", true, "pv-data"), csiPV("pv-data"),
		attachment("first", "data", now), attachment("second", "data", now.Add(time.Second)))
	e.reconcile()
	if got := e.sva("first"); got.Status.Phase != swiftv1alpha1.VolumeAttachmentAttaching {
		t.Fatalf("the earlier attachment proceeds; got %s", got.Status.Phase)
	}
	if got := e.sva("second"); got.Status.Phase != swiftv1alpha1.VolumeAttachmentFailed {
		t.Fatalf("the later attachment of the same claim fails; got %s", got.Status.Phase)
	}
}

// Stopping the guest frees the volume: its launcher no longer holds it.
func TestGuestStopDeletesAttachmentPods(t *testing.T) {
	e := newEnv(t, guest(true), launcher("hotplug-volumes"), claim("data", true, "pv-data"), csiPV("pv-data"),
		attachment("data", "data", time.Now()))
	e.reconcile()
	if len(e.attachmentPods("data")) != 1 {
		t.Fatal("want an attachment pod")
	}
	if err := e.c.Delete(context.Background(), e.launcher()); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if len(e.attachmentPods("data")) != 0 {
		t.Fatal("want the attachment pod deleted when the guest has no launcher")
	}
	e.reconcile() // a deleted pod stays recorded until it is gone
	if got := e.sva("data"); reason(got) != swiftv1alpha1.VolumeAttachmentReasonGuestNotRunning || len(got.Status.AttachmentPods) != 0 {
		t.Fatalf("want GuestNotRunning and no recorded pods, got %s %v", reason(got), got.Status.AttachmentPods)
	}
}

// Only a pod whose UID the controller recorded is the attachment's.
func TestForeignAndOrphanPods(t *testing.T) {
	sva := attachment("data", "data", time.Now())
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fake", Namespace: ns, UID: "foreign",
		Labels: map[string]string{swiftv1alpha1.LabelVolumeAttachment: "data"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	orphan := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: ns, UID: "orphan",
		Labels: map[string]string{swiftv1alpha1.LabelVolumeAttachment: "data"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "swift.kubeswift.io/v1alpha1", Kind: "SwiftVolumeAttachment",
			Name: "data", UID: sva.UID, Controller: ptr.To(true)}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	e := newEnv(t, guest(true), launcher("hotplug-volumes"), claim("data", true, "pv-data"), csiPV("pv-data"), sva, foreign, orphan)
	e.reconcile()
	pods := e.attachmentPods("data")
	names := map[string]bool{}
	for _, p := range pods {
		names[p.Name] = true
	}
	if names["orphan"] || !names["fake"] || len(pods) != 2 {
		t.Fatalf("want the orphan deleted, the foreign pod left and a new pod created; got %v", names)
	}
	if got := e.sva("data"); len(got.Status.AttachmentPods) != 1 || got.Status.AttachmentPods[0].UID == "foreign" {
		t.Fatalf("recorded %v", got.Status.AttachmentPods)
	}
}
