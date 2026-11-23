package podmode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

func request(t *testing.T, op admissionv1.Operation, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op, Namespace: "tenant", Object: runtime.RawExtension{Raw: raw},
	}}
}

func pod(rc *string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "tenant"},
		Spec: corev1.PodSpec{RuntimeClassName: rc, Containers: []corev1.Container{{
			Name: "c", Image: "docker.io/library/python:3.13",
			Ports: []corev1.ContainerPort{{ContainerPort: 8888, Protocol: corev1.ProtocolTCP}},
		}}},
	}
}

func handler() *Handler {
	return &Handler{ShimImage: func() string { return "ghcr.io/kubeswift-io/kubeswift/sandbox-materialize:v0.27.0" }}
}

func TestOtherPodsUntouched(t *testing.T) {
	for name, p := range map[string]*corev1.Pod{
		"no runtime class":    pod(nil),
		"other runtime class": pod(ptr.To("gvisor")),
	} {
		resp := handler().Handle(context.Background(), request(t, admissionv1.Create, p))
		if !resp.Allowed || len(resp.Patches) != 0 {
			t.Errorf("%s: allowed %v patches %d", name, resp.Allowed, len(resp.Patches))
		}
	}
	resp := handler().Handle(context.Background(), request(t, admissionv1.Update, pod(ptr.To(podmode.RuntimeClassName))))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Errorf("update: allowed %v patches %d", resp.Allowed, len(resp.Patches))
	}
}

func TestRewritePatches(t *testing.T) {
	resp := handler().Handle(context.Background(), request(t, admissionv1.Create, pod(ptr.To(podmode.RuntimeClassName))))
	if !resp.Allowed || len(resp.Patches) == 0 {
		t.Fatalf("allowed %v patches %d: %v", resp.Allowed, len(resp.Patches), resp.Result)
	}
	var removesRC, setsWorkload bool
	for _, op := range resp.Patches {
		if op.Path == "/spec/runtimeClassName" && op.Operation == "remove" {
			removesRC = true
		}
		if strings.Contains(op.Path, "podmode.kubeswift.io~1workload") || op.Path == "/metadata/annotations" {
			setsWorkload = true
		}
	}
	if !removesRC || !setsWorkload {
		t.Errorf("patches %+v", resp.Patches)
	}
}

func TestRefusalDenies(t *testing.T) {
	p := pod(ptr.To(podmode.RuntimeClassName))
	p.Spec.HostNetwork = true
	resp := handler().Handle(context.Background(), request(t, admissionv1.Create, p))
	if resp.Allowed || resp.Result == nil || !strings.Contains(resp.Result.Message, "spec.hostNetwork") {
		t.Errorf("allowed %v result %+v", resp.Allowed, resp.Result)
	}
}
