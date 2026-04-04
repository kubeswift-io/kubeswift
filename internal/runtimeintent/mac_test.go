package runtimeintent

import (
	"regexp"
	"testing"
)

func TestGenerateMAC_Deterministic(t *testing.T) {
	seed := "default/myguest/eth0"
	mac1 := GenerateMAC(seed)
	mac2 := GenerateMAC(seed)
	if mac1 != mac2 {
		t.Errorf("GenerateMAC not deterministic: %q != %q", mac1, mac2)
	}
}

func TestGenerateMAC_Uniqueness(t *testing.T) {
	seeds := []string{
		"default/guest1/eth0",
		"default/guest2/eth0",
		"default/guest1/eth1",
		"other/guest1/eth0",
	}
	seen := make(map[string]string) // mac -> seed
	for _, seed := range seeds {
		mac := GenerateMAC(seed)
		if prev, ok := seen[mac]; ok {
			t.Errorf("MAC collision: seeds %q and %q both produced %q", prev, seed, mac)
		}
		seen[mac] = seed
	}
}

func TestGenerateMAC_Format(t *testing.T) {
	mac := GenerateMAC("test/seed/value")
	// Must match XX:XX:XX:XX:XX:XX with 52:54:00 prefix
	pattern := `^52:54:00:[0-9a-f]{2}:[0-9a-f]{2}:[0-9a-f]{2}$`
	matched, err := regexp.MatchString(pattern, mac)
	if err != nil {
		t.Fatalf("regexp error: %v", err)
	}
	if !matched {
		t.Errorf("GenerateMAC(%q) = %q, does not match pattern %s", "test/seed/value", mac, pattern)
	}
}

func TestInterfaceMACSeed(t *testing.T) {
	seed := InterfaceMACSeed("myns", "myguest", "eth0")
	want := "myns/myguest/eth0"
	if seed != want {
		t.Errorf("InterfaceMACSeed = %q, want %q", seed, want)
	}
}
