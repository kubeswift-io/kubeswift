package swiftsnapshot

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func ociSnap(mut func(*snapshotv1alpha1.OCIBackend)) *snapshotv1alpha1.SwiftSnapshot {
	oci := &snapshotv1alpha1.OCIBackend{Repository: "zot.svc:5000/vm-snapshots"}
	if mut != nil {
		mut(oci)
	}
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "team-a"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef:      snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"},
			IncludeMemory: true,
			Backend:       snapshotv1alpha1.SwiftSnapshotBackend{Type: snapshotv1alpha1.SnapshotBackendOCI, OCI: oci},
		},
	}
}

func TestOCITag_DefaultAndExplicit(t *testing.T) {
	if got := ociTag(ociSnap(nil)); got != "team-a-snap1" {
		t.Errorf("default tag = %q, want team-a-snap1", got)
	}
	explicit := ociSnap(func(o *snapshotv1alpha1.OCIBackend) { o.Tag = "prod-42" })
	if got := ociTag(explicit); got != "prod-42" {
		t.Errorf("explicit tag = %q, want prod-42", got)
	}
	if got := ociReference(ociSnap(nil)); got != "zot.svc:5000/vm-snapshots:team-a-snap1" {
		t.Errorf("reference = %q", got)
	}
}

func TestBuildOCIPushJob_WithCredentials(t *testing.T) {
	snap := ociSnap(func(o *snapshotv1alpha1.OCIBackend) {
		o.Insecure = true
		o.CredentialsSecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "reg-creds"}
	})
	job := buildOCIPushJob(snap, "img:tag", "boba")

	if job.Name != "snap1-oci-push" || job.Namespace != "team-a" {
		t.Errorf("job meta = %s/%s", job.Namespace, job.Name)
	}
	pod := job.Spec.Template.Spec
	if pod.NodeName != "boba" {
		t.Errorf("not pinned to capture node: %q", pod.NodeName)
	}
	c := pod.Containers[0]
	args := strings.Join(c.Args, " ")
	for _, want := range []string{
		"--mode=upload", "--dir=/snap",
		"--repository=zot.svc:5000/vm-snapshots", "--tag=team-a-snap1",
		"--snapshot=team-a/snap1", "--insecure", "--include-memory",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q; got %q", want, args)
		}
	}
	// Runs as root to read the 0600 capture artifacts.
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("push container must RunAsUser 0")
	}
	// DOCKER_CONFIG env + the dockerconfigjson-mounted volume are present.
	var hasDockerCfg bool
	for _, e := range c.Env {
		if e.Name == "DOCKER_CONFIG" && e.Value == ociAuthMount {
			hasDockerCfg = true
		}
	}
	if !hasDockerCfg {
		t.Errorf("DOCKER_CONFIG env missing")
	}
	var authVol bool
	for _, v := range pod.Volumes {
		if v.Name == "oras-auth" {
			if v.Secret == nil || v.Secret.SecretName != "reg-creds" {
				t.Errorf("oras-auth volume not from the creds Secret: %+v", v.Secret)
			}
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != ".dockerconfigjson" || v.Secret.Items[0].Path != "config.json" {
				t.Errorf("dockerconfigjson not remapped to config.json: %+v", v.Secret.Items)
			}
			authVol = true
		}
	}
	if !authVol {
		t.Errorf("oras-auth volume missing")
	}
}

func TestBuildOCIPushJob_Anonymous(t *testing.T) {
	job := buildOCIPushJob(ociSnap(nil), "img:tag", "miles")
	pod := job.Spec.Template.Spec
	for _, e := range pod.Containers[0].Env {
		if e.Name == "DOCKER_CONFIG" {
			t.Errorf("anonymous push must not set DOCKER_CONFIG")
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == "oras-auth" {
			t.Errorf("anonymous push must not mount a credential volume")
		}
	}
	// The snapshot hostPath volume is always present.
	var snapVol bool
	for _, v := range pod.Volumes {
		if v.Name == "snapshot" {
			snapVol = true
		}
	}
	if !snapVol {
		t.Errorf("snapshot hostPath volume missing")
	}
	// Not-insecure snapshot omits the --insecure flag.
	if strings.Contains(strings.Join(pod.Containers[0].Args, " "), "--insecure") {
		t.Errorf("--insecure should be absent when Insecure=false")
	}
	// No signing key -> no --sign-key and no signing volumes.
	if strings.Contains(strings.Join(pod.Containers[0].Args, " "), "--sign-key") {
		t.Errorf("--sign-key must be absent without a signing-key Secret")
	}
	for _, v := range pod.Volumes {
		if v.Name == "oras-signing-key" || v.Name == "cosign-home" {
			t.Errorf("signing volume %q must be absent without a signing-key Secret", v.Name)
		}
	}
}

func TestBuildOCIPushJob_WithSigning(t *testing.T) {
	snap := ociSnap(func(o *snapshotv1alpha1.OCIBackend) {
		o.SigningKeySecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "cosign-key"}
	})
	pod := buildOCIPushJob(snap, "img:tag", "boba").Spec.Template.Spec
	c := pod.Containers[0]

	if !strings.Contains(strings.Join(c.Args, " "), "--sign-key=/oras-signing-key/cosign.key") {
		t.Errorf("--sign-key arg missing; got %q", strings.Join(c.Args, " "))
	}

	// COSIGN_PASSWORD from the signing Secret (optional), plus writable HOME/TMPDIR.
	var pwFromSecret, homeSet, tmpSet bool
	for _, e := range c.Env {
		switch e.Name {
		case "COSIGN_PASSWORD":
			if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
				e.ValueFrom.SecretKeyRef.Name != "cosign-key" || e.ValueFrom.SecretKeyRef.Key != "cosign.password" ||
				e.ValueFrom.SecretKeyRef.Optional == nil || !*e.ValueFrom.SecretKeyRef.Optional {
				t.Errorf("COSIGN_PASSWORD must be an optional secretKeyRef to cosign-key/cosign.password; got %+v", e.ValueFrom)
			}
			pwFromSecret = true
		case "HOME":
			homeSet = e.Value == ociCosignHome
		case "TMPDIR":
			tmpSet = e.Value == ociCosignHome
		}
	}
	if !pwFromSecret || !homeSet || !tmpSet {
		t.Errorf("expected COSIGN_PASSWORD + HOME + TMPDIR env; pw=%v home=%v tmp=%v", pwFromSecret, homeSet, tmpSet)
	}

	// The key volume (cosign.key remapped) + the writable cosign-home emptyDir.
	var keyVol, homeVol bool
	for _, v := range pod.Volumes {
		switch v.Name {
		case "oras-signing-key":
			if v.Secret == nil || v.Secret.SecretName != "cosign-key" ||
				len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "cosign.key" || v.Secret.Items[0].Path != "cosign.key" {
				t.Errorf("oras-signing-key volume malformed: %+v", v.Secret)
			}
			keyVol = true
		case "cosign-home":
			if v.EmptyDir == nil {
				t.Errorf("cosign-home must be an emptyDir")
			}
			homeVol = true
		}
	}
	if !keyVol || !homeVol {
		t.Errorf("expected oras-signing-key + cosign-home volumes; key=%v home=%v", keyVol, homeVol)
	}
}

func TestOCISigningRequested(t *testing.T) {
	if ociSigningRequested(ociSnap(nil)) {
		t.Error("no signing-key Secret must not request signing")
	}
	if ociSigningRequested(ociSnap(func(o *snapshotv1alpha1.OCIBackend) {
		o.SigningKeySecretRef = &snapshotv1alpha1.SecretObjectReference{Name: ""}
	})) {
		t.Error("an empty signing-key name must not request signing")
	}
	if !ociSigningRequested(ociSnap(func(o *snapshotv1alpha1.OCIBackend) {
		o.SigningKeySecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "cosign-key"}
	})) {
		t.Error("a named signing-key Secret must request signing")
	}
}
