package swiftimage

import (
	"context"
	"errors"
	"fmt"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

// ImageConverter prepares an image for runtime. Stub implementation: pass-through when formats match; error for conversion.
type ImageConverter interface {
	Prepare(ctx context.Context, sourcePath string, sourceFormat, targetFormat imagev1alpha1.DiskFormat) (preparedPath string, size int64, err error)
}

// StubConverter is the default converter. Pass-through when sourceFormat == targetFormat.
// qcow2→raw: conversion is done by the import job when spec.format is qcow2; Prepare succeeds as no-op.
type StubConverter struct{}

func (StubConverter) Prepare(ctx context.Context, sourcePath string, sourceFormat, targetFormat imagev1alpha1.DiskFormat) (string, int64, error) {
	if sourceFormat == targetFormat {
		return sourcePath, 0, nil
	}
	if sourceFormat == imagev1alpha1.DiskFormatQcow2 && targetFormat == imagev1alpha1.DiskFormatRaw {
		return sourcePath, 0, nil
	}
	return "", 0, fmt.Errorf("conversion from %s to %s not implemented", sourceFormat, targetFormat)
}

var ErrConversionNotImplemented = errors.New("conversion not implemented")
