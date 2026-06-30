package gateway

import (
	"context"
	"testing"

	connect "connectrpc.com/connect"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	kubeswiftv1 "github.com/kubeswift-io/kubeswift/gen/kubeswift/v1"
)

func uMigration(ns, name, guest, target, phase string, progress int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "migration.kubeswift.io/v1alpha1",
		"kind":       "SwiftMigration",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec": map[string]interface{}{
			"guestRef": map[string]interface{}{"name": guest},
			"target":   map[string]interface{}{"nodeName": target},
			"mode":     "live",
		},
		"status": map[string]interface{}{
			"phase":            phase,
			"transferProgress": progress,
			"observedDowntime": "1.8s",
			"sourceNode":       "boba",
		},
	}}
}

func TestMigrationService_ListMigrations(t *testing.T) {
	boba := fakeDyn(uMigration("default", "m1", "vm-a", "miles", "StopAndCopy", 65))
	svc := NewMigrationService(&fakeProvider{clients: map[string]dynamic.Interface{"boba": boba}}, NewInsecureAuthenticator())

	resp, err := svc.ListMigrations(context.Background(), connect.NewRequest(&kubeswiftv1.ListMigrationsRequest{}))
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(resp.Msg.Migrations) != 1 {
		t.Fatalf("want 1 migration, got %d", len(resp.Msg.Migrations))
	}
	m := resp.Msg.Migrations[0]
	if m.GetGuest() != "vm-a" || m.GetTargetNode() != "miles" || m.GetSourceNode() != "boba" {
		t.Errorf("node/guest mapping wrong: %+v", m)
	}
	if m.GetPhase() != "StopAndCopy" || m.GetMode() != "live" {
		t.Errorf("phase/mode wrong: %+v", m)
	}
	if m.GetTransferProgress() != 65 {
		t.Errorf("transferProgress = %d, want 65", m.GetTransferProgress())
	}
	if d := m.GetObservedDowntimeSeconds(); d < 1.7 || d > 1.9 {
		t.Errorf("observedDowntime = %v, want ~1.8 (parsed from \"1.8s\")", d)
	}
	if m.GetRef().GetCluster() != "boba" {
		t.Errorf("cluster dimension missing: %+v", m.GetRef())
	}
}
