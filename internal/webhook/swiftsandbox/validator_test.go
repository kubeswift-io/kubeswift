package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func sb(image string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: image},
	}
}

func TestValidateCreate(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), sb("")); err == nil {
		t.Error("empty image should be rejected")
	}
	if _, err := v.ValidateCreate(context.Background(), sb("alpine:3.20")); err != nil {
		t.Errorf("valid sandbox rejected: %v", err)
	}
	bad := sb("alpine")
	d := metav1.Duration{Duration: -1}
	bad.Spec.Timeout = &d
	if _, err := v.ValidateCreate(context.Background(), bad); err == nil {
		t.Error("negative timeout should be rejected")
	}
}

func TestValidateUpdate_Immutable(t *testing.T) {
	v := &Validator{}
	old := sb("alpine:3.20")

	// A launch-affecting change is rejected.
	chg := old.DeepCopy()
	chg.Spec.Image = "alpine:3.21"
	if _, err := v.ValidateUpdate(context.Background(), old, chg); err == nil {
		t.Error("image change should be rejected")
	}

	// A ttl change is allowed (retention is adjustable).
	ttlChg := old.DeepCopy()
	d := metav1.Duration{Duration: 60_000_000_000}
	ttlChg.Spec.TTL = &d
	if _, err := v.ValidateUpdate(context.Background(), old, ttlChg); err != nil {
		t.Errorf("ttl change should be allowed: %v", err)
	}

	// Deletion carve-out: an update while deleting must pass even with a spec diff.
	del := old.DeepCopy()
	now := metav1.Now()
	del.DeletionTimestamp = &now
	del.Spec.Image = "x"
	if _, err := v.ValidateUpdate(context.Background(), old, del); err != nil {
		t.Errorf("update during deletion must pass: %v", err)
	}
}

func TestValidateDelete_PassThrough(t *testing.T) {
	if _, err := (&Validator{}).ValidateDelete(context.Background(), sb("alpine")); err != nil {
		t.Errorf("delete must pass through: %v", err)
	}
}

func TestValidateCreate_VerifyKeySecretRef(t *testing.T) {
	v := &Validator{}

	// A ref with a name is accepted.
	ok := sb("alpine:3.20")
	ok.Spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: "cosign-pub"}
	if _, err := v.ValidateCreate(context.Background(), ok); err != nil {
		t.Errorf("verifyKeySecretRef with a name should be accepted: %v", err)
	}

	// A ref with an empty name is rejected (shape check).
	bad := sb("alpine:3.20")
	bad.Spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: ""}
	if _, err := v.ValidateCreate(context.Background(), bad); err == nil {
		t.Error("verifyKeySecretRef with an empty name should be rejected")
	}
}

func TestValidateUpdate_VerifyKeySecretRefImmutable(t *testing.T) {
	v := &Validator{}
	old := sb("alpine:3.20")
	// Adding a verify key after creation is a launch-affecting change (the rootfs
	// would be verified where before it was not) — immutable, recreate to change.
	chg := old.DeepCopy()
	chg.Spec.VerifyKeySecretRef = &sandboxv1alpha1.SecretObjectReference{Name: "cosign-pub"}
	if _, err := v.ValidateUpdate(context.Background(), old, chg); err == nil {
		t.Error("adding verifyKeySecretRef should be rejected as immutable")
	}
}

func TestValidateCreate_GPUResourceClaim(t *testing.T) {
	v := &Validator{}

	// A single-template claim is valid.
	tmpl := sb("nvcr.io/nvidia/cuda:12.4.0-runtime-ubuntu22.04")
	tmpl.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimTemplateName: "single-vfio-gpu", Tier: "pcie"}
	if _, err := v.ValidateCreate(context.Background(), tmpl); err != nil {
		t.Errorf("single-template gpuResourceClaim rejected: %v", err)
	}

	// A single shared-claim is valid.
	shared := sb("cuda:12.4")
	shared.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimName: "gpu-claim"}
	if _, err := v.ValidateCreate(context.Background(), shared); err != nil {
		t.Errorf("single shared-claim gpuResourceClaim rejected: %v", err)
	}

	// Neither name nor template — rejected.
	neither := sb("cuda:12.4")
	neither.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{Tier: "pcie"}
	if _, err := v.ValidateCreate(context.Background(), neither); err == nil {
		t.Error("gpuResourceClaim with neither name nor template should be rejected")
	}

	// Both name and template — rejected.
	both := sb("cuda:12.4")
	both.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimName: "c", ResourceClaimTemplateName: "t"}
	if _, err := v.ValidateCreate(context.Background(), both); err == nil {
		t.Error("gpuResourceClaim with both name and template should be rejected")
	}
}

func TestValidateCreate_GPUAndPoolMutuallyExclusive(t *testing.T) {
	v := &Validator{}
	s := sb("cuda:12.4")
	s.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimTemplateName: "single-vfio-gpu"}
	s.Spec.PoolRef = &corev1.LocalObjectReference{Name: "warm-pool"}
	if _, err := v.ValidateCreate(context.Background(), s); err == nil {
		t.Error("gpuResourceClaim + poolRef should be rejected (GPU sandboxes boot cold)")
	}
}

func TestValidateCreate_GPUProfileAndClaimMutuallyExclusive(t *testing.T) {
	v := &Validator{}
	s := sb("cuda:12.4")
	s.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gtx"}
	s.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimTemplateName: "single-vfio-gpu"}
	if _, err := v.ValidateCreate(context.Background(), s); err == nil {
		t.Error("gpuProfileRef + gpuResourceClaim should be rejected (pick one GPU backend)")
	}
}

func TestValidateCreate_GPUProfileAndPoolMutuallyExclusive(t *testing.T) {
	v := &Validator{}
	s := sb("cuda:12.4")
	s.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gtx"}
	s.Spec.PoolRef = &corev1.LocalObjectReference{Name: "warm-pool"}
	if _, err := v.ValidateCreate(context.Background(), s); err == nil {
		t.Error("gpuProfileRef + poolRef should be rejected (a GPU sandbox boots cold)")
	}
}

func TestValidateCreate_GPUProfileRef_OK(t *testing.T) {
	v := &Validator{}
	s := sb("cuda:12.4")
	s.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "gtx"}
	if _, err := v.ValidateCreate(context.Background(), s); err != nil {
		t.Errorf("gpuProfileRef alone should be accepted, got: %v", err)
	}
}

func TestValidateCreate_GPUProfileRef_RequiresName(t *testing.T) {
	v := &Validator{}
	s := sb("cuda:12.4")
	s.Spec.GPUProfileRef = &corev1.LocalObjectReference{}
	if _, err := v.ValidateCreate(context.Background(), s); err == nil {
		t.Error("gpuProfileRef with an empty name should be rejected")
	}
}
