package clonecommon

import (
	"context"
	"encoding/json"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TransferReport mirrors the JSON the snapshot-s3 and snapshot-oras binaries
// write to their container termination message on a successful transfer
// (cmd/snapshot-s3 transferStats / cmd/snapshot-oras transferStats). Keep the
// json tags in sync with those structs: the two sides are coupled only by this
// wire shape (the binaries stay minimal and do not import this package).
// transferredBytes + skippedBytes == totalBytes.
type TransferReport struct {
	// TransferredBytes is the artifact bytes actually moved over the wire
	// (excludes resume-skipped objects) — the bandwidth/cost figure.
	TransferredBytes int64 `json:"transferredBytes"`
	// SkippedBytes is the artifact bytes skipped because already present + verified.
	SkippedBytes int64 `json:"skippedBytes"`
	// TotalBytes is the snapshot's full artifact footprint.
	TotalBytes int64 `json:"totalBytes"`
	// Reference is the pushed artifact reference (oci backend only; empty for s3).
	Reference string `json:"reference,omitempty"`
	// ManifestDigest is the sha256 of the pushed OCI manifest (oci backend only;
	// empty for s3). Restore pins the artifact by this digest.
	ManifestDigest string `json:"manifestDigest,omitempty"`
	// Signed is true when the oci artifact was cosign-signed as an OCI referrer
	// (oci backend only; the controller stamps status.oci.signed from it).
	Signed bool `json:"signed,omitempty"`
}

// JobTransferReport reads the report a completed snapshot-s3 / snapshot-oras
// Job left in its pod's container termination message. Returns (report, true,
// nil) when a terminated container carried a parseable report; (_, false, nil)
// when none is available (pod GC'd, message absent/garbled). For byte counts a
// missing report is not a failure (Design Principle #6: never fabricate, but
// never fail the operation on a missing metric). An OCI push is different:
// its report carries the manifest digest restores pin the artifact by, and the
// SwiftSnapshot controller will not go Ready without it. Pods are matched by
// the standard `job-name` label, which the Job controller applies alongside
// `batch.kubernetes.io/job-name` for compatibility.
//
// Only a Succeeded pod the Job controls counts. Matching the label alone let
// any pod in the namespace carrying `job-name: <job>` supply the report, and
// with it the manifest digest a restore pins the artifact by.
func JobTransferReport(ctx context.Context, c client.Reader, namespace, jobName string) (TransferReport, bool, error) {
	var job batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: jobName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return TransferReport{}, false, nil
		}
		return TransferReport{}, false, err
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName},
	); err != nil {
		return TransferReport{}, false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded || !metav1.IsControlledBy(pod, &job) {
			continue
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated == nil || cs.State.Terminated.Message == "" {
				continue
			}
			var r TransferReport
			if json.Unmarshal([]byte(cs.State.Terminated.Message), &r) == nil {
				return r, true, nil
			}
		}
	}
	return TransferReport{}, false, nil
}
