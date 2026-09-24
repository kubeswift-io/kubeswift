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
	if got := NodeDir(s); got != "/var/lib/kubeswift/snapshots/team-a_snap1" {
		t.Errorf("NodeDir (not captured) = %q", got)
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
		Snapshot: s, Image: "img", Name: "dl", Namespace: "team-a", Node: "worker-1",
		Component: "snapshot-s3-download", ExtraLabels: map[string]string{"owner": "x"},
	})
	pod := job.Spec.Template.Spec
	if pod.NodeName != "worker-1" || pod.RestartPolicy != corev1.RestartPolicyOnFailure {
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
	if hp == nil || hp.Path != "/var/lib/kubeswift/snapshots/team-a_snap1" || *hp.Type != corev1.HostPathDirectoryOrCreate {
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

// Two snapshots never share a directory: "<ns>-<name>" gave namespace "team" +
// "a-db" and namespace "team-a" + "db" the same one, and a local hostPath was
// whatever the author wrote.
func TestSnapshotDir_Injective(t *testing.T) {
	if a, b := SnapshotDir("team", "a-db"), SnapshotDir("team-a", "db"); a == b {
		t.Fatalf("collision: %q", a)
	}
	if got := SnapshotDir("ns", "db.v1"); got != "/var/lib/kubeswift/snapshots/ns_db.v1" {
		t.Errorf("SnapshotDir = %q", got)
	}
}

// A 253-character name would give a 300-character directory name, past
// NAME_MAX; the bounded one keeps the namespace prefix that binds it.
func TestSnapshotDir_LongNameBounded(t *testing.T) {
	name := strings.Repeat("a", 253)
	dir := SnapshotDir("team", name)
	seg := strings.TrimPrefix(dir, HostPathBase)
	if len(seg) > 255 {
		t.Fatalf("segment is %d bytes", len(seg))
	}
	if !SnapshotDirNamespaced(dir, "team") {
		t.Fatalf("%q lost its namespace prefix", dir)
	}
	if other := SnapshotDir("team", name[:252]+"b"); other == dir {
		t.Fatalf("two long names share %q", dir)
	}
}

func TestSnapshotDirNamespaced(t *testing.T) {
	for _, tc := range []struct {
		dir, ns string
		want    bool
	}{
		{"/var/lib/kubeswift/snapshots/team_db", "team", true},
		{"/var/lib/kubeswift/snapshots/team_db/", "team", true},
		{"/var/lib/kubeswift/snapshots/team-a_db", "team", false}, // namespace team-a
		{"/var/lib/kubeswift/snapshots/team-db", "team", false},   // legacy, ambiguous
		{"/var/lib/kubeswift/snapshots/team_db/x", "team", false}, // nested
		{"/var/lib/kubeswift/snapshots/_db", "", false},
		{"/var/lib/other/team_db", "team", false},
	} {
		if got := SnapshotDirNamespaced(tc.dir, tc.ns); got != tc.want {
			t.Errorf("SnapshotDirNamespaced(%q, %q) = %v, want %v", tc.dir, tc.ns, got, tc.want)
		}
	}
}

// NodeDir is where the capture went: recorded when it began, derived before
// then, and for a capture an earlier version began (a node, no handle), the
// directory that version used.
func TestNodeDir(t *testing.T) {
	local := func(hostPath string) *snapshotv1alpha1.SwiftSnapshot {
		s := s3Snap("team", "db")
		s.Spec.Backend = snapshotv1alpha1.SwiftSnapshotBackend{
			Type:  snapshotv1alpha1.SnapshotBackendLocal,
			Local: &snapshotv1alpha1.LocalBackend{HostPath: hostPath},
		}
		return s
	}

	s := local("/var/lib/kubeswift/snapshots/mine/")
	if got := NodeDir(s); got != "/var/lib/kubeswift/snapshots/team_db" {
		t.Errorf("not begun: %q, want the derived dir whatever hostPath says", got)
	}

	s.Status.NodeName = "n1"
	if got := NodeDir(s); got != "/var/lib/kubeswift/snapshots/mine" {
		t.Errorf("begun by an earlier version (local): %q, want its hostPath", got)
	}
	s3 := s3Snap("team", "db")
	s3.Status.NodeName = "n1"
	if got := NodeDir(s3); got != "/var/lib/kubeswift/snapshots/team-db" {
		t.Errorf("begun by an earlier version (s3): %q, want <ns>-<name>", got)
	}

	s.Status.MemorySnapshot = &snapshotv1alpha1.MemorySnapshotRef{Handle: "/var/lib/kubeswift/snapshots/team_db/"}
	if got := NodeDir(s); got != "/var/lib/kubeswift/snapshots/team_db" {
		t.Errorf("recorded: %q, want the handle", got)
	}
}
