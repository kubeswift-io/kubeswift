package clonecommon

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Transfer Jobs (snapshot-s3, snapshot-oras) run their pods with
// TransferRestartPolicy and their containers with TransferTerminationPolicy.
// Under restartPolicy OnFailure the container restarted inside one pod, and
// once the backoff limit was reached the Job controller deleted that pod and
// its log with it: the Job's own condition ("Job has reached the specified
// backoff limit") was all that was left to say why a transfer failed (#737).
// Under Never every attempt keeps its pod until the Job goes, and
// FallbackToLogsOnError puts the tail of a failed container's log in its
// termination message, which JobFailureMessage reads. The success path is
// unchanged: a successful binary writes its report to the termination log
// itself, and JobTransferReport reads only Succeeded pods.
const (
	TransferRestartPolicy     = corev1.RestartPolicyNever
	TransferTerminationPolicy = corev1.TerminationMessageFallbackToLogsOnError
)

// JobFailureMessage is the message for a Job that has failed for good: why its
// last failed pod failed, followed by the Job's own condition message (cond).
// It falls back to cond alone when no failed pod with a message is left.
func JobFailureMessage(ctx context.Context, c client.Reader, job *batchv1.Job, cond string) string {
	why := lastPodFailure(ctx, c, job)
	switch {
	case why == "":
		return cond
	case cond == "":
		return why
	}
	return why + " (" + cond + ")"
}

// lastPodFailure is the condensed termination message of the most recently
// failed container among the Job's own pods, init containers included (a
// SwiftImage OCI import pulls in an init container). Pods are matched by the
// Job controller's `job-name` label and must be controlled by the Job.
func lastPodFailure(ctx context.Context, c client.Reader, job *batchv1.Job) string {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ""
	}
	var latest time.Time
	msg := ""
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, job) {
			continue
		}
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			t := cs.State.Terminated
			if t == nil {
				t = cs.LastTerminationState.Terminated
			}
			if t == nil || t.ExitCode == 0 || strings.TrimSpace(t.Message) == "" {
				continue
			}
			if msg == "" || t.FinishedAt.After(latest) {
				latest, msg = t.FinishedAt.Time, t.Message
			}
		}
	}
	return failureText(msg)
}

// logTimestamp matches the prefix Go's log package writes: the transfer
// binaries log their progress that way, and print their final error without it.
var logTimestamp = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// maxFailureText bounds the part of a termination message a status message
// carries. The kubelet keeps up to 2 KiB of the log tail.
const maxFailureText = 1024

// failureText is the part of a failed transfer container's termination message
// that says why: the lines after its last progress line, which are the
// binary's own error and anything a tool it ran printed before it (cosign
// writes its reason to stderr, not into the error the binary reports). Blank
// lines and "WARNING:" notices are dropped, and the lines are joined with "; ".
// When every line is progress, the last one is used.
func failureText(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	start := 0
	for i, l := range lines {
		if logTimestamp.MatchString(l) {
			start = i + 1
		}
	}
	var keep []string
	for _, l := range lines[start:] {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "WARNING:") {
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) == 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		keep = []string{strings.TrimSpace(logTimestamp.ReplaceAllString(last, ""))}
	}
	s := strings.Join(keep, "; ")
	if len(s) > maxFailureText {
		cut := len(s) - maxFailureText
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		s = "..." + s[cut:]
	}
	return s
}
