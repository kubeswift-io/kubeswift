package clonecommon

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// failedPod is a pod of reportJob whose container failed at finished with msg
// as its termination message. init puts the failure in an init container.
func failedPod(name, msg string, finished time.Time, init, controlled bool) *corev1.Pod {
	pod := jobPod(name, "j", "", false)
	if !controlled {
		pod.OwnerReferences = nil
	}
	pod.Status.Phase = corev1.PodFailed
	cs := corev1.ContainerStatus{Name: "x", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 1, Message: msg, FinishedAt: metav1.NewTime(finished)}}}
	if init {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{cs}
		pod.Status.ContainerStatuses = nil
	} else {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	}
	return pod
}

const backoff = "Job has reached the specified backoff limit"

func TestJobFailureMessage(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	tlsErr := "2026/10/05 08:00:01 pushing config.json\nsnapshot-oras: push: Get \"https://reg/v2/\": tls: failed to verify certificate: x509: certificate signed by unknown authority"

	// The last attempt's error comes first, then the Job's condition.
	c := newReader(reportJob(),
		failedPod("j-1", "snapshot-oras: an older attempt", t0, false, true),
		failedPod("j-2", tlsErr, t0.Add(time.Minute), false, true))
	got := JobFailureMessage(ctx, c, reportJob(), backoff)
	want := "snapshot-oras: push: Get \"https://reg/v2/\": tls: failed to verify certificate: x509: certificate signed by unknown authority (" + backoff + ")"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// A pod that only carries the label is not the Job's: it says nothing.
	c = newReader(reportJob(),
		failedPod("j-1", "snapshot-oras: the real error", t0, false, true),
		failedPod("planted", "snapshot-oras: not this one", t0.Add(time.Hour), false, false))
	if got := JobFailureMessage(ctx, c, reportJob(), backoff); !strings.HasPrefix(got, "snapshot-oras: the real error") {
		t.Errorf("a pod the Job does not control was read: %q", got)
	}

	// An init container's failure counts (a SwiftImage OCI import pulls in one).
	c = newReader(reportJob(), failedPod("j-1", "snapshot-oras: download-image: manifest unknown", t0, true, true))
	if got := JobFailureMessage(ctx, c, reportJob(), backoff); !strings.Contains(got, "manifest unknown") {
		t.Errorf("init container failure missed: %q", got)
	}

	// No pod left: the condition alone, as before.
	if got := JobFailureMessage(ctx, newReader(reportJob()), reportJob(), backoff); got != backoff {
		t.Errorf("no pods: got %q", got)
	}
}

func TestFailureText(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"progress, then the error": {
			"2026/10/05 08:00:00 snapshot-s3 upload: 3 artifact(s)\n2026/10/05 08:00:01   put memory-ranges (1 bytes -> zstd)\nsnapshot-s3: put memory-ranges: AccessDenied: Access Denied\n",
			"snapshot-s3: put memory-ranges: AccessDenied: Access Denied",
		},
		"cosign's reason is kept, its tlog notice is not": {
			"WARNING: Skipping tlog verification is an insecure practice\nError: no signatures found\nmain.go:74: error during command execution: no signatures found\nsnapshot-oras: download-image: cosign verify r@sha256:ab: exit status 10",
			"Error: no signatures found; main.go:74: error during command execution: no signatures found; snapshot-oras: download-image: cosign verify r@sha256:ab: exit status 10",
		},
		"only progress lines": {
			"2026/10/05 08:00:00 snapshot-s3 delete: 2 object(s)\n2026/10/05 08:00:01   rm a\n",
			"rm a",
		},
		"empty": {"", ""},
	}
	for name, c := range cases {
		if got := failureText(c.in); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", name, got, c.want)
		}
	}

	long := strings.Repeat("é", 1000) + " the end"
	got := failureText(long)
	if len(got) > maxFailureText+len("...") || !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "the end") || !utf8.ValidString(got) {
		t.Errorf("a long message keeps its valid tail: %d bytes, %q...", len(got), got[:12])
	}
}
