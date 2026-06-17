package swiftmigration

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftguest"
)

func TestForceLifecycleStart_ForcesStartPreservesRest(t *testing.T) {
	in := map[string]string{
		swiftguest.IntentFile: `{"lifecycle":"stop","cpu":2,"network":true}`,
		"unrelated":           "x",
	}
	out, err := forceLifecycleStart(in)
	if err != nil {
		t.Fatalf("forceLifecycleStart: %v", err)
	}
	if out["unrelated"] != "x" {
		t.Errorf("unrelated key not preserved")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out[swiftguest.IntentFile]), &obj); err != nil {
		t.Fatalf("unmarshal frozen: %v", err)
	}
	if obj["lifecycle"] != "start" {
		t.Errorf("lifecycle: want start, got %v", obj["lifecycle"])
	}
	if obj["cpu"] != float64(2) {
		t.Errorf("cpu field not preserved: %v", obj["cpu"])
	}
	if obj["network"] != true {
		t.Errorf("network field not preserved: %v", obj["network"])
	}
	// Input not mutated.
	if in[swiftguest.IntentFile] != `{"lifecycle":"stop","cpu":2,"network":true}` {
		t.Errorf("input map was mutated")
	}
}

func TestForceLifecycleStart_MissingKey_Errors(t *testing.T) {
	if _, err := forceLifecycleStart(map[string]string{"x": "y"}); err == nil {
		t.Errorf("expected error when the intent key is absent")
	}
}

func TestEnsureFrozenDstIntent_CreatesFrozenStartCM(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"}}
	live := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "guest" + swiftguest.IntentConfigMapSuffix, Namespace: "default"},
		Data:       map[string]string{swiftguest.IntentFile: `{"lifecycle":"stop","cpu":2}`},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, live).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}
	ctx := context.Background()

	name, err := r.ensureFrozenDstIntent(ctx, guest, "guest-mig-abcdef")
	if err != nil {
		t.Fatalf("ensureFrozenDstIntent: %v", err)
	}
	want := "guest-mig-abcdef" + swiftguest.IntentConfigMapSuffix
	if name != want {
		t.Errorf("frozen CM name: want %q, got %q", want, name)
	}

	var frozen corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &frozen); err != nil {
		t.Fatalf("get frozen CM: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(frozen.Data[swiftguest.IntentFile]), &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["lifecycle"] != "start" {
		t.Errorf("frozen lifecycle: want start (not the live's stop), got %v", obj["lifecycle"])
	}
	if len(frozen.OwnerReferences) != 1 || frozen.OwnerReferences[0].Name != "guest" {
		t.Errorf("frozen CM must be guest-owned; got %+v", frozen.OwnerReferences)
	}
	// Idempotent.
	if _, err := r.ensureFrozenDstIntent(ctx, guest, "guest-mig-abcdef"); err != nil {
		t.Errorf("idempotent ensure errored: %v", err)
	}
}

func TestEnsureFrozenDstIntent_MissingLiveCM_SkipsGracefully(t *testing.T) {
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	name, err := r.ensureFrozenDstIntent(context.Background(), guest, "guest-mig-abcdef")
	if err != nil {
		t.Fatalf("missing live CM must not error (graceful skip); got %v", err)
	}
	if name != "" {
		t.Errorf("missing live CM must skip the freeze (empty name); got %q", name)
	}
}

func TestNewDstPod_RepointsRuntimeIntentVolume(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigrationWithUID("mig-a", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Target.NodeName = "miles"
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"}}
	src := templateSrcPod("guest", "default")
	// Source carries the runtime-intent volume pointing at the live CM.
	src.Spec.Volumes = append(src.Spec.Volumes, corev1.Volume{
		Name: runtimeIntentVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "guest" + swiftguest.IntentConfigMapSuffix},
			},
		},
	})

	frozenName := "guest-mig-abcdef" + swiftguest.IntentConfigMapSuffix
	dst, err := newDstPod(mig, guest, src, scheme, dstSidecarConfig{}, frozenName, nil)
	if err != nil {
		t.Fatalf("newDstPod: %v", err)
	}
	if got := intentVolumeCM(dst); got != frozenName {
		t.Errorf("runtime-intent volume should be repointed to the frozen CM %q; got %q", frozenName, got)
	}

	// Empty frozen name leaves the inherited live CM in place.
	dst2, _ := newDstPod(mig, guest, src, scheme, dstSidecarConfig{}, "", nil)
	if got := intentVolumeCM(dst2); got != "guest"+swiftguest.IntentConfigMapSuffix {
		t.Errorf("empty frozen name must leave the live CM; got %q", got)
	}
}

func intentVolumeCM(pod *corev1.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.Name == runtimeIntentVolumeName && v.ConfigMap != nil {
			return v.ConfigMap.Name
		}
	}
	return ""
}
