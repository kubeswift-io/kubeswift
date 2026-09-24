package swiftsnapshot

import (
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// A snapshot name may run to 253 characters. Its derived Job names and the
// name labels its Jobs carry must still be valid, or every Job creation fails
// and the snapshot is later marked Failed for a misleading reason.
func TestDerivedJobs_AreValidForALongSnapshotName(t *testing.T) {
	snap := ociSnap(nil)
	snap.Name = strings.Repeat("s", 90)
	snap.Spec.Backend.S3 = &snapshotv1alpha1.S3Backend{Bucket: "b"}

	jobs := map[string]*batchv1.Job{
		"oci push":   buildOCIPushJob(snap, "img", "worker-1"),
		"oci delete": buildOCIDeleteJob(snap, "img", []ociArtifact{{repository: "r", tag: "t"}}),
		"root chunk": buildChunkJob(snap, "img", "worker-1", diskChunkJobName(snap), "t", "pvc", false),
		"data chunk": buildChunkJob(snap, "img", "worker-1", dataDiskChunkJobName(snap, strings.Repeat("d", 40)), "t", "pvc", false),
		"s3 upload":  buildUploadJob(snap, "img", "worker-1"),
		"s3 delete":  buildDeleteJob(snap, "img"),
	}
	for what, job := range jobs {
		if errs := validation.IsValidLabelValue(job.Name); len(errs) != 0 {
			t.Errorf("%s Job name %q (%d chars): %v", what, job.Name, len(job.Name), errs)
		}
		if errs := validation.IsDNS1123Subdomain(job.Name); len(errs) != 0 {
			t.Errorf("%s Job name %q: %v", what, job.Name, errs)
		}
		for k, v := range job.Labels {
			if errs := validation.IsValidLabelValue(v); len(errs) != 0 {
				t.Errorf("%s Job label %s=%q: %v", what, k, v, errs)
			}
		}
	}
}
