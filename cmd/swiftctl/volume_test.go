package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func svaFor(name, guest, pvc string) *swiftv1alpha1.SwiftVolumeAttachment {
	return &swiftv1alpha1.SwiftVolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftVolumeAttachmentSpec{
			GuestRef: corev1.LocalObjectReference{Name: guest},
			PVCRef:   corev1.LocalObjectReference{Name: pvc},
		},
	}
}

func TestFindAttachment(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		svaFor("db-data", "db", "data"),
		svaFor("other-data", "other", "data"),
		svaFor("db-s1", "db", "scratch"),
		svaFor("db-s2", "db", "scratch"),
	).Build()
	ctx := context.Background()
	if a, err := findAttachment(ctx, c, "ns", "db", "db-data"); err != nil || a.Name != "db-data" {
		t.Errorf("by name: %v %v", a, err)
	}
	if a, err := findAttachment(ctx, c, "ns", "db", "data"); err != nil || a.Name != "db-data" {
		t.Errorf("by PVC: %v %v", a, err)
	}
	if _, err := findAttachment(ctx, c, "ns", "db", "other-data"); err == nil {
		t.Error("another guest's attachment matched by name")
	}
	if _, err := findAttachment(ctx, c, "ns", "db", "scratch"); err == nil || !strings.Contains(err.Error(), "2 attachments") {
		t.Errorf("ambiguous PVC: %v", err)
	}
	if _, err := findAttachment(ctx, c, "ns", "db", "nope"); err == nil {
		t.Error("missing attachment found")
	}
}

func TestAttachmentName(t *testing.T) {
	if n, err := attachmentName("db", "data-1"); err != nil || n != "db-data-1" {
		t.Errorf("attachmentName = %q, %v", n, err)
	}
	if _, err := attachmentName("db", strings.Repeat("x", 260)); err == nil {
		t.Error("an over-long default name was accepted")
	}
}

func TestPrintAttachments(t *testing.T) {
	now := time.Now()
	a := svaFor("db-data", "db", "data")
	a.CreationTimestamp = metav1.NewTime(now.Add(-90 * time.Second))
	a.Status.Phase = swiftv1alpha1.VolumeAttachmentAttached
	a.Status.Serial = "ks-abc"
	b := svaFor("other-x", "other", "x")
	b.Spec.ReadOnly = true
	var out bytes.Buffer
	if err := printAttachments(&out, []swiftv1alpha1.SwiftVolumeAttachment{*a, *b}, "db", now); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "db-data") || !strings.Contains(got, "ks-abc") || !strings.Contains(got, "Attached") || strings.Contains(got, "other-x") {
		t.Errorf("filtered table:\n%s", got)
	}
	out.Reset()
	_ = printAttachments(&out, []swiftv1alpha1.SwiftVolumeAttachment{*b}, "", now)
	if !strings.Contains(out.String(), "ro") || !strings.Contains(out.String(), "Pending") {
		t.Errorf("unfiltered table:\n%s", out.String())
	}
	out.Reset()
	_ = printAttachments(&out, nil, "db", now)
	if !strings.Contains(out.String(), "no hot-plugged volumes") {
		t.Errorf("empty: %q", out.String())
	}
}
