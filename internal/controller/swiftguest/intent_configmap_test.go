package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// A guest's launcher reads its configuration from <guest>-runtime-intent. One
// that someone else created first is neither adopted nor rewritten, and no
// launcher is started until it is removed.
func TestReconcile_ForeignIntentConfigMapHoldsTheGuest(t *testing.T) {
	ctx := context.Background()
	forged := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testGuestName + IntentConfigMapSuffix, Namespace: "ns"},
		Data:       map[string]string{IntentFile: `{"tenant":"supplied"}`},
	}
	c := guestClientBuilder(kernelGuest(), testGuestClass(), readyKernel(), forged).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: c.Scheme()}

	g, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if cond := guestCondition(t, g, ConditionResolved); cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "is not KubeSwift's") {
		t.Errorf("Resolved = %+v, want False naming the ConfigMap", cond)
	}
	if g.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase %q, want Pending", g.Status.Phase)
	}
	if err := c.Get(ctx, testGuestKey, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("a launcher was created next to a foreign intent (err %v)", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(forged), &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[IntentFile] != `{"tenant":"supplied"}` || len(cm.OwnerReferences) != 0 {
		t.Errorf("someone else's ConfigMap was modified or adopted: %+v", cm)
	}
}

func TestEnsureImmutableIntentConfigMap(t *testing.T) {
	ctx := context.Background()
	owner := kernelGuest()
	desired := func(data string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "x" + IntentConfigMapSuffix, Namespace: "ns",
				OwnerReferences: guestControllerRef()},
			Data: map[string]string{IntentFile: data},
		}
	}

	c := guestClientBuilder(owner).Build()
	if err := EnsureImmutableIntentConfigMap(ctx, c, owner, desired(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// Idempotent for the same data.
	if err := EnsureImmutableIntentConfigMap(ctx, c, owner, desired(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// Ours with other data: replaced.
	if err := EnsureImmutableIntentConfigMap(ctx, c, owner, desired(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "x" + IntentConfigMapSuffix}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[IntentFile] != `{"a":2}` || cm.Immutable == nil || !*cm.Immutable {
		t.Errorf("not replaced with an immutable copy of the new intent: %+v", cm)
	}

	// Someone else's: refused, untouched.
	foreign := desired(`{"tenant":1}`)
	foreign.Name = "y" + IntentConfigMapSuffix
	foreign.OwnerReferences = nil
	c = guestClientBuilder(owner, foreign).Build()
	want := desired(`{"a":1}`)
	want.Name = foreign.Name
	err := EnsureImmutableIntentConfigMap(ctx, c, owner, want)
	if _, ok := err.(*IntentConflictError); !ok {
		t.Fatalf("err = %v, want IntentConflictError", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), &cm); err != nil || cm.Data[IntentFile] != `{"tenant":1}` {
		t.Errorf("someone else's ConfigMap changed: %+v (err %v)", cm, err)
	}
}

func TestSetIntentDigest(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "init-like"}, {Name: "launcher"}}}}
	SetIntentDigest(pod, []byte(`{"a":1}`))
	SetIntentDigest(pod, []byte(`{"a":2}`))
	var n int
	for _, e := range pod.Spec.Containers[1].Env {
		if e.Name == EnvIntentSHA256 {
			n++
			if e.Value != IntentDigest([]byte(`{"a":2}`)) {
				t.Errorf("digest %q is not the latest intent's", e.Value)
			}
		}
	}
	if n != 1 || len(pod.Spec.Containers[0].Env) != 0 {
		t.Errorf("want exactly one %s on the launcher only; launcher has %d", EnvIntentSHA256, n)
	}
}
