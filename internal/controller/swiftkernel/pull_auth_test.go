package swiftkernel

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

// ociRef.pullSecret was set only as the pod's imagePullSecrets, which reach
// the kubelet pulling the oras image, so the kernel artifact itself was pulled
// anonymously and a private registry refused it (#708). The pull must read the
// Secret's Docker config.
func TestStartPullOnNode_PullSecretReachesTheOrasPull(t *testing.T) {
	for _, secret := range []string{"", "regcreds"} {
		sk := &kernelv1alpha1.SwiftKernel{
			ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "default", UID: "sk-uid"},
			Spec: kernelv1alpha1.SwiftKernelSpec{OCIRef: kernelv1alpha1.OCIRef{
				Image: "registry.example.com/kernels/k:1", PullSecret: secret,
			}},
		}
		c := fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(sk).Build()
		r := &SwiftKernelReconciler{Client: c, Scheme: kscheme.Scheme}
		if err := r.StartPullOnNode(context.Background(), sk, "node-a"); err != nil {
			t.Fatalf("secret=%q: %v", secret, err)
		}
		var job batchv1.Job
		key := types.NamespacedName{Namespace: "default", Name: pullJobName("k", "node-a", kernelv1alpha1.KernelLocalPath("default", "k"))}
		if err := c.Get(context.Background(), key, &job); err != nil {
			t.Fatalf("secret=%q: pull Job: %v", secret, err)
		}
		pod := job.Spec.Template.Spec
		script := strings.Join(pod.Containers[0].Command, " ")
		flag := strings.Contains(script, "--registry-config "+pullAuthDir+"/config.json")
		var vol *corev1.Volume
		for i := range pod.Volumes {
			if pod.Volumes[i].Name == "oras-auth" {
				vol = &pod.Volumes[i]
			}
		}
		var mount *corev1.VolumeMount
		for i := range pod.Containers[0].VolumeMounts {
			if pod.Containers[0].VolumeMounts[i].Name == "oras-auth" {
				mount = &pod.Containers[0].VolumeMounts[i]
			}
		}

		if secret == "" {
			if flag || vol != nil || mount != nil {
				t.Errorf("no pullSecret: want an anonymous pull, got flag=%v volume=%v mount=%v", flag, vol != nil, mount != nil)
			}
			continue
		}
		if !flag {
			t.Errorf("oras pull does not read the credentials: %s", script)
		}
		if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret ||
			len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != corev1.DockerConfigJsonKey || vol.Secret.Items[0].Path != "config.json" {
			t.Errorf("credentials volume = %+v, want %s's %s as config.json", vol, secret, corev1.DockerConfigJsonKey)
		}
		if mount == nil || mount.MountPath != pullAuthDir || !mount.ReadOnly {
			t.Errorf("credentials mount = %+v, want read-only at %s", mount, pullAuthDir)
		}
	}
}
