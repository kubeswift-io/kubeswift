package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// The driver must decline health reporting with the sentinel the API provides,
// promptly and without sending anything. Two things would be worse:
//
//   - Reporting a hardcoded "Healthy": answering this call promises the kubelet
//     a fresh report inside each device's HealthCheckTimeout, so a GPU whose
//     vfio-pci binding had gone would still read as usable.
//   - Blocking until ctx is cancelled: the kubelet's health stream would hang
//     open instead of being told, once, that the driver does not support it.
func TestWatchHealthStatusDeclinesWithTheSentinel(t *testing.T) {
	d := &draDriver{nodeName: "n1"}
	reports := make(chan kubeletplugin.DeviceHealthReport, 1)

	done := make(chan error, 1)
	go func() { done <- d.WatchHealthStatus(context.Background(), reports) }()

	select {
	case err := <-done:
		if !errors.Is(err, kubeletplugin.ErrHealthNotSupported) {
			t.Fatalf("WatchHealthStatus = %v, want ErrHealthNotSupported", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchHealthStatus blocked; it must return promptly so the kubelet stops watching")
	}

	if len(reports) != 0 {
		t.Errorf("sent %d report(s); a declining driver must send none", len(reports))
	}
}
