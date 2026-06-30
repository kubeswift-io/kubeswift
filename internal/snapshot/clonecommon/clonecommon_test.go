package clonecommon

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func s3Snap(ns, name string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendS3,
				S3: &snapshotv1alpha1.S3Backend{
					Bucket:               "bk",
					Prefix:               "pfx",
					CredentialsSecretRef: &snapshotv1alpha1.SecretObjectReference{Name: "creds"},
				},
			},
		},
	}
}

func TestPaths(t *testing.T) {
	s := s3Snap("team-a", "snap1")
	if got := S3LocalDir(s); got != "/var/lib/kubeswift/snapshots/team-a-snap1" {
		t.Errorf("S3LocalDir = %q", got)
	}
	if got := S3KeyPrefix(s); got != "pfx/team-a/snap1" {
		t.Errorf("S3KeyPrefix = %q", got)
	}
	// empty prefix => <ns>/<name>
	s.Spec.Backend.S3.Prefix = ""
	if got := S3KeyPrefix(s); got != "team-a/snap1" {
		t.Errorf("empty-prefix S3KeyPrefix = %q", got)
	}
	if got := RuntimeDirPrefix("ns", "g"); got != "/var/lib/kubeswift/run/ns-g/" {
		t.Errorf("RuntimeDirPrefix = %q", got)
	}
}

func TestComputeMACRewrites(t *testing.T) {
	// No interfaces => single deterministic eth0 MAC.
	g := &swiftv1alpha1.SwiftGuest{}
	a := ComputeMACRewrites("ns", "clone-a", g)
	b := ComputeMACRewrites("ns", "clone-b", g)
	if a == "" || strings.Contains(a, ",") {
		t.Errorf("default = %q, want single MAC", a)
	}
	if a == b {
		t.Errorf("distinct clone names must yield distinct MACs; both %q", a)
	}
	if a != ComputeMACRewrites("ns", "clone-a", g) {
		t.Errorf("MAC must be deterministic for the same (ns,name)")
	}
	// Two interfaces => CSV of two MACs.
	g2 := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{
		Interfaces: []swiftv1alpha1.GuestInterface{{Name: "mgmt"}, {Name: "data"}},
	}}
	csv := ComputeMACRewrites("ns", "g", g2)
	if parts := strings.Split(csv, ","); len(parts) != 2 || parts[0] == parts[1] {
		t.Errorf("two-iface = %q, want 2 distinct MACs", csv)
	}
}

func TestBuildDownloadJob(t *testing.T) {
	s := s3Snap("team-a", "snap1")
	s.Spec.Backend.S3.Region = "us-east-1"
	s.Spec.Backend.S3.Endpoint = "minio:9000"
	s.Spec.Backend.S3.ForcePathStyle = true
	s.Spec.Backend.S3.Insecure = true
	s.Spec.IncludeMemory = true
	job := BuildDownloadJob(DownloadJobParams{
		Snapshot: s, Image: "img", Name: "dl", Namespace: "team-a", Node: "boba",
		Component: "snapshot-s3-download", ExtraLabels: map[string]string{"owner": "x"},
	})
	pod := job.Spec.Template.Spec
	if pod.NodeName != "boba" || pod.RestartPolicy != corev1.RestartPolicyOnFailure {
		t.Errorf("node/restart wrong: %q %q", pod.NodeName, pod.RestartPolicy)
	}
	if job.Labels["owner"] != "x" || job.Labels["app.kubernetes.io/component"] != "snapshot-s3-download" {
		t.Errorf("labels = %+v", job.Labels)
	}
	c := pod.Containers[0]
	// RW DirectoryOrCreate cache mount.
	if c.VolumeMounts[0].MountPath != DownloadMount || c.VolumeMounts[0].ReadOnly {
		t.Errorf("mount = %+v", c.VolumeMounts[0])
	}
	hp := pod.Volumes[0].VolumeSource.HostPath
	if hp == nil || hp.Path != "/var/lib/kubeswift/snapshots/team-a-snap1" || *hp.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("hostPath = %+v", hp)
	}
	a := strings.Join(c.Args, " ")
	for _, w := range []string{"--mode=download", "--bucket=bk", "--key-prefix=pfx/team-a/snap1",
		"--region=us-east-1", "--endpoint=minio:9000", "--path-style", "--insecure", "--include-memory"} {
		if !strings.Contains(a, w) {
			t.Errorf("args missing %q; got %q", w, a)
		}
	}
	// creds from the Secret; runs as root, hardened.
	if c.Env[0].ValueFrom.SecretKeyRef.Name != "creds" {
		t.Errorf("creds env = %+v", c.Env[0])
	}
	sc := c.SecurityContext
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 || !*sc.ReadOnlyRootFilesystem ||
		*sc.AllowPrivilegeEscalation || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("securityContext = %+v", sc)
	}
}
