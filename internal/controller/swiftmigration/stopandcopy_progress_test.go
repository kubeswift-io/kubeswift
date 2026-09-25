package swiftmigration

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

func TestStampTransferProgress(t *testing.T) {
	podWith := func(v string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnotationMigrationProgressEstimate: v},
		}}
	}
	withID := func(p *corev1.Pod, id string) *corev1.Pod {
		p.Annotations[AnnotationMigrationProgressEstimateID] = id
		return p
	}
	cases := []struct {
		name string
		pod  *corev1.Pod
		want *int32 // nil => field must be left unchanged (nil)
	}{
		{"valid percent", podWith("45"), ptr.To[int32](45)},
		{"zero", podWith("0"), ptr.To[int32](0)},
		{"hundred", podWith("100"), ptr.To[int32](100)},
		{"clamp above 100", podWith("150"), ptr.To[int32](100)},
		{"clamp below 0", podWith("-5"), ptr.To[int32](0)},
		{"absent annotation", &corev1.Pod{}, nil},
		{"unparseable", podWith("soon"), nil},
		{"nil pod", nil, nil},
		// The estimate outlives its send: one naming another send is the
		// previous send's last value.
		{"this send's estimate", withID(podWith("40"), "m:send:1"), ptr.To[int32](40)},
		{"a previous send's estimate", withID(podWith("95"), "old:send:1"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := &migrationv1alpha1.SwiftMigrationStatus{}
			stampTransferProgress(status, tc.pod, "m:send:1")
			switch {
			case tc.want == nil && status.TransferProgress != nil:
				t.Errorf("want unchanged (nil); got %d", *status.TransferProgress)
			case tc.want != nil && (status.TransferProgress == nil || *status.TransferProgress != *tc.want):
				t.Errorf("TransferProgress = %v, want %d", status.TransferProgress, *tc.want)
			}
		})
	}
}
