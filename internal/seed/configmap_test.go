package seed

import (
	"testing"
)

func TestBuildConfigMap_KeysPresent(t *testing.T) {
	cm := BuildConfigMap("test-seed", "ns", "user", "meta", "net")
	if cm.Name != "test-seed" || cm.Namespace != "ns" {
		t.Errorf("name/namespace: %s/%s", cm.Name, cm.Namespace)
	}
	if cm.Data[KeyUserData] != "user" {
		t.Errorf("userData = %q", cm.Data[KeyUserData])
	}
	if cm.Data[KeyMetaData] != "meta" {
		t.Errorf("metaData = %q", cm.Data[KeyMetaData])
	}
	if cm.Data[KeyNetworkConfig] != "net" {
		t.Errorf("networkConfig = %q", cm.Data[KeyNetworkConfig])
	}
}

func TestBuildConfigMap_EmptyOmitted(t *testing.T) {
	cm := BuildConfigMap("test", "ns", "user", "", "")
	if _, ok := cm.Data[KeyUserData]; !ok {
		t.Error("userData should be present")
	}
	if _, ok := cm.Data[KeyMetaData]; ok {
		t.Error("metaData should be omitted when empty")
	}
	if _, ok := cm.Data[KeyNetworkConfig]; ok {
		t.Error("networkConfig should be omitted when empty")
	}
}
