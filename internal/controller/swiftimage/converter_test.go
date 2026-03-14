package swiftimage

import (
	"context"
	"strings"
	"testing"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

func TestStubConverter_PassThroughWhenFormatsMatch(t *testing.T) {
	ctx := context.Background()
	c := StubConverter{}
	path, size, err := c.Prepare(ctx, "/tmp/image.raw", imagev1alpha1.DiskFormatRaw, imagev1alpha1.DiskFormatRaw)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if path != "/tmp/image.raw" {
		t.Errorf("path = %q, want /tmp/image.raw", path)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
}

func TestStubConverter_ErrorWhenConversionRequired(t *testing.T) {
	ctx := context.Background()
	c := StubConverter{}
	_, _, err := c.Prepare(ctx, "/tmp/image.qcow2", imagev1alpha1.DiskFormatQcow2, imagev1alpha1.DiskFormatRaw)
	if err == nil {
		t.Fatal("expected error for qcow2 to raw conversion")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error should mention not implemented, got %q", err.Error())
	}
}
