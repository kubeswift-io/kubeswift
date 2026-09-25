package swiftmigration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// Why a destination pod is not Ready.
//
// A destination pod that never becomes Ready used to fail its migration with
// only "never reached Ready within 1m0s budget". The cause was elsewhere: in
// the lab it was the storage refusing to attach the volume to the target node
// (Longhorn does not live-migrate a degraded volume), visible only as a
// FailedAttachVolume event on the pod and in Longhorn's attachment ticket. An
// attach failure leaves the pod's own status saying nothing more than
// PodInitializing or ContainerCreating, so the pod's recent Warning events are
// read as well.

// eventInvolvedObjectNameField is the Event field selector the apiserver
// supports for the object an event is about.
const eventInvolvedObjectNameField = "involvedObject.name"

// notReadyDetailMaxLen bounds each free-text message (event, container,
// scheduling) quoted in a failure message. Event messages from CSI drivers can
// run to kilobytes.
const notReadyDetailMaxLen = 300

// notReadyMaxEvents is how many Warning events, each with a different reason,
// a failure message quotes. The latest alone can be the least specific one (a
// kubelet FailedMount timeout that follows the FailedAttachVolume naming the
// node), so two.
const notReadyMaxEvents = 2

// eventReasonDestinationPodNeverReady is the reason of the Warning event
// recorded on a SwiftMigration that fails DstNeverReady. Its message is the
// failure message.
const eventReasonDestinationPodNeverReady = "DestinationPodNeverReady"

// eventReasonDestinationPodNotReady is the reason of the Warning event recorded
// on an offline SwiftMigration whose spec.timeout ran out while its destination
// pod was not Ready. Its message says why.
const eventReasonDestinationPodNotReady = "DestinationPodNotReady"

// podNotReadyCause describes why pod is not Ready, from its own status and its
// most recent Warning events, as "; "-separated parts. Empty when neither says
// anything, so the caller's message stays as it was.
func (r *SwiftMigrationReconciler) podNotReadyCause(ctx context.Context, pod *corev1.Pod) string {
	parts := podStatusNotReadyCause(pod)
	parts = append(parts, r.podWarningEvents(ctx, pod)...)
	return strings.Join(parts, "; ")
}

// podStatusNotReadyCause reads why pod is not Ready from its status alone: a
// kubelet rejection (phase Failed), an unschedulable PodScheduled=False, and
// the containers that have not started or are not ready.
func podStatusNotReadyCause(pod *corev1.Pod) []string {
	var parts []string
	if pod.Status.Phase == corev1.PodFailed {
		parts = append(parts, notReadyDetail("pod Failed", pod.Status.Reason, pod.Status.Message))
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			parts = append(parts, notReadyDetail("not scheduled", c.Reason, c.Message))
		}
	}

	// The first init container that has not completed holds up the rest and
	// every container, whose own statuses then say only PodInitializing. A
	// native sidecar (restartPolicy Always) runs for the pod's life, so it
	// counts as done once it runs.
	sidecars := map[string]bool{}
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars[c.Name] = true
		}
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		label := fmt.Sprintf("init container %q", cs.Name)
		switch {
		case cs.State.Waiting != nil:
			return append(parts, notReadyDetail(label+" waiting", cs.State.Waiting.Reason, cs.State.Waiting.Message))
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
			t := cs.State.Terminated
			return append(parts, notReadyDetail(fmt.Sprintf("%s exited %d", label, t.ExitCode), t.Reason, t.Message))
		case cs.State.Running != nil && !sidecars[cs.Name]:
			return append(parts, label+" still running")
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		label := fmt.Sprintf("container %q", cs.Name)
		switch {
		case cs.State.Waiting != nil:
			parts = append(parts, notReadyDetail(label+" waiting", cs.State.Waiting.Reason, cs.State.Waiting.Message))
		case cs.State.Terminated != nil:
			t := cs.State.Terminated
			parts = append(parts, notReadyDetail(fmt.Sprintf("%s exited %d", label, t.ExitCode), t.Reason, t.Message))
		case cs.State.Running != nil && !cs.Ready:
			parts = append(parts, label+" running but not ready")
		}
	}
	return parts
}

// podWarningEvents returns pod's most recent Warning events, latest first, one
// per reason, at most notReadyMaxEvents. Events are listed through the uncached
// APIReader and only for this pod, so no informer holds every Event in the
// cluster; with no APIReader, or when the list fails (an install whose role
// lacks events list), there are none and the pod's status is reported alone.
func (r *SwiftMigrationReconciler) podWarningEvents(ctx context.Context, pod *corev1.Pod) []string {
	if r.APIReader == nil {
		return nil
	}
	var events corev1.EventList
	if err := r.APIReader.List(ctx, &events,
		client.InNamespace(pod.Namespace),
		client.MatchingFields{eventInvolvedObjectNameField: pod.Name},
	); err != nil {
		log.FromContext(ctx).Info("could not list the pod's events; reporting its status only",
			"pod", pod.Name, "error", err.Error())
		return nil
	}

	warnings := make([]*corev1.Event, 0, len(events.Items))
	for i := range events.Items {
		ev := &events.Items[i]
		if ev.Type != corev1.EventTypeWarning || ev.InvolvedObject.Kind != "Pod" {
			continue
		}
		// An offline migration's destination pod has its source pod's name;
		// the UID keeps the source pod's events out.
		if pod.UID != "" && ev.InvolvedObject.UID != pod.UID {
			continue
		}
		warnings = append(warnings, ev)
	}
	// Latest first; the name breaks ties so the same events give the same
	// message.
	sort.Slice(warnings, func(i, j int) bool {
		ti, tj := eventLastSeen(warnings[i]), eventLastSeen(warnings[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return warnings[i].Name < warnings[j].Name
	})

	var parts []string
	seen := map[string]bool{}
	for _, ev := range warnings {
		if seen[ev.Reason] {
			continue
		}
		seen[ev.Reason] = true
		parts = append(parts, notReadyDetail("Warning "+ev.Reason, "", ev.Message))
		if len(parts) == notReadyMaxEvents {
			break
		}
	}
	return parts
}

// eventLastSeen is when an event last occurred. core/v1 recorders set
// lastTimestamp; events.k8s.io recorders (the scheduler) set eventTime and,
// once the event repeats, series.lastObservedTime.
func eventLastSeen(ev *corev1.Event) time.Time {
	t := ev.LastTimestamp.Time
	if ev.EventTime.After(t) {
		t = ev.EventTime.Time
	}
	if ev.Series != nil && ev.Series.LastObservedTime.After(t) {
		t = ev.Series.LastObservedTime.Time
	}
	if t.IsZero() {
		t = ev.FirstTimestamp.Time
	}
	if t.IsZero() {
		t = ev.CreationTimestamp.Time
	}
	return t
}

// notReadyDetail joins label with an optional reason and message, the message
// on one line and bounded to notReadyDetailMaxLen characters.
func notReadyDetail(label, reason, message string) string {
	s := label
	if reason != "" {
		s += ": " + reason
	}
	message = strings.Join(strings.Fields(message), " ")
	if runes := []rune(message); len(runes) > notReadyDetailMaxLen {
		message = string(runes[:notReadyDetailMaxLen]) + "..."
	}
	if message != "" {
		s += ": " + message
	}
	return s
}

// offlineTimeoutFailure is timeoutFailure for an offline migration. Past the
// offline cutover (StopAndCopy, Resuming) the migration waits on the guest's
// launcher pod on the target, and a pod that never becomes Ready there ran the
// migration into spec.timeout with only "did not complete in time". The
// message now also says why the pod is not Ready, as DstNeverReady does for a
// live migration.
func (r *SwiftMigrationReconciler) offlineTimeoutFailure(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	phase migrationv1alpha1.SwiftMigrationPhase,
) *phaseResult {
	res := timeoutFailure(mig)
	if phase != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy &&
		phase != migrationv1alpha1.SwiftMigrationPhaseResuming {
		return res
	}
	// Best-effort: the timeout fails the migration whether or not the pod
	// can be read.
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &pod); err != nil {
		return res
	}
	if dstPodReady(&pod) {
		return res
	}
	cause := r.podNotReadyCause(ctx, &pod)
	if cause == "" {
		return res
	}
	detail := fmt.Sprintf("destination pod %q is not Ready: %s", pod.Name, cause)
	res.FailureMsg += "; " + detail
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeWarning, eventReasonDestinationPodNotReady, detail)
	}
	return res
}
