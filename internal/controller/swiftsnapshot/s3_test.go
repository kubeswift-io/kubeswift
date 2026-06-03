package swiftsnapshot

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

func s3Snap(name, ns string, mut func(*snapshotv1alpha1.SwiftSnapshot)) *snapshotv1alpha1.SwiftSnapshot {
	s := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendS3,
				S3: &snapshotv1alpha1.S3Backend{
					Bucket:               "backups",
					Prefix:               "kubeswift",
					CredentialsSecretRef: &snapshotv1alpha1.SecretObjectReference{Name: "s3-creds"},
				},
			},
		},
	}
	if mut != nil {
		mut(s)
	}
	return s
}

func TestS3Helpers(t *testing.T) {
	s := s3Snap("snap1", "team-a", nil)
	if got := s3KeyPrefix(s); got != "kubeswift/team-a/snap1" {
		t.Errorf("keyPrefix = %q", got)
	}
	if got := s3Location(s); got != "s3://backups/kubeswift/team-a/snap1/" {
		t.Errorf("location = %q", got)
	}
	if got := s3LocalDir(s); got != "/var/lib/kubeswift/snapshots/team-a-snap1" {
		t.Errorf("localDir = %q", got)
	}
	if got := s3UploadJobName(s); got != "snap1-s3-upload" {
		t.Errorf("jobName = %q", got)
	}
	// Empty prefix => <ns>/<name>.
	s2 := s3Snap("snap2", "ns", func(s *snapshotv1alpha1.SwiftSnapshot) { s.Spec.Backend.S3.Prefix = "" })
	if got := s3KeyPrefix(s2); got != "ns/snap2" {
		t.Errorf("empty-prefix keyPrefix = %q", got)
	}
}

func TestBuildUploadJob_Pinning_Mount_Creds(t *testing.T) {
	s := s3Snap("snap1", "team-a", func(s *snapshotv1alpha1.SwiftSnapshot) {
		s.Spec.IncludeMemory = true
		s.Spec.Backend.S3.Region = "us-east-1"
		s.Spec.Backend.S3.Endpoint = "minio.svc:9000"
		s.Spec.Backend.S3.ForcePathStyle = true
	})
	job := buildUploadJob(s, "ghcr.io/x/snapshot-s3:t", "boba")
	pod := job.Spec.Template.Spec

	if pod.NodeName != "boba" {
		t.Errorf("job must pin to the capture node; got %q", pod.NodeName)
	}
	if pod.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("restartPolicy = %q, want OnFailure (idempotent resume)", pod.RestartPolicy)
	}
	c := pod.Containers[0]

	// hostPath mounted read-only at /snap.
	vm := c.VolumeMounts[0]
	if vm.MountPath != s3UploadMount || !vm.ReadOnly {
		t.Errorf("snapshot volume must mount %s read-only; got %+v", s3UploadMount, vm)
	}
	hp := pod.Volumes[0].VolumeSource.HostPath
	if hp == nil || hp.Path != "/var/lib/kubeswift/snapshots/team-a-snap1" {
		t.Errorf("hostPath = %+v", hp)
	}

	// args carry the derived prefix + flags.
	a := strings.Join(c.Args, " ")
	for _, want := range []string{"--mode=upload", "--bucket=backups", "--key-prefix=kubeswift/team-a/snap1",
		"--region=us-east-1", "--endpoint=minio.svc:9000", "--path-style", "--include-memory", "--snapshot=team-a/snap1"} {
		if !strings.Contains(a, want) {
			t.Errorf("args missing %q; got %q", want, a)
		}
	}

	// creds from the Secret as AWS env (never plaintext args).
	wantKeys := map[string]string{"AWS_ACCESS_KEY_ID": "accessKeyId", "AWS_SECRET_ACCESS_KEY": "secretAccessKey", "AWS_SESSION_TOKEN": "sessionToken"}
	got := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			if e.ValueFrom.SecretKeyRef.Name != "s3-creds" {
				t.Errorf("env %s must source from the creds Secret; got %q", e.Name, e.ValueFrom.SecretKeyRef.Name)
			}
			got[e.Name] = e.ValueFrom.SecretKeyRef.Key
		}
	}
	for env, key := range wantKeys {
		if got[env] != key {
			t.Errorf("env %s should map to Secret key %q; got %q", env, key, got[env])
		}
	}

	// hardened container.
	sc := c.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("upload container must be hardened (non-root, ro-rootfs, drop ALL); got %+v", sc)
	}
}

func TestBuildUploadJob_OptionalFlagsOmitted(t *testing.T) {
	// No region/endpoint/path-style/memory => those flags absent.
	s := s3Snap("snap1", "ns", nil)
	job := buildUploadJob(s, "img", "miles")
	a := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, absent := range []string{"--region", "--endpoint", "--path-style", "--include-memory"} {
		if strings.Contains(a, absent) {
			t.Errorf("flag %q should be omitted when unset; got %q", absent, a)
		}
	}
}
