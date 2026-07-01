package clonecommon

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
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
}

// JobTransferReport reads the byte report a completed snapshot-s3 Job left in
// its pod's container termination message. Returns (report, true, nil) when a
// terminated container carried a parseable report; (_, false, nil) when none is
// available (pod GC'd, message absent/garbled). A missing report is NOT an error
// the caller should fail on — it is a metrics/status surface only (Design
// Principle #6: never fabricate, but never fail the operation on a missing
// metric). Pods are matched by the standard `job-name` label, which the Job
// controller applies alongside `batch.kubernetes.io/job-name` for compatibility.
func JobTransferReport(ctx context.Context, c client.Reader, namespace, jobName string) (TransferReport, bool, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName},
	); err != nil {
		return TransferReport{}, false, err
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
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
