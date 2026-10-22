package swiftguest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

const (
	networkInitScript   = "../../../rust/swiftletd/scripts/network-init.sh"
	netInitGuestID      = "default/secondary-nad"
	netInitMultusIface  = "net1"
	netInitBridge       = "br1"
	netInitTap          = "tap1"
	netInitGuestMAC     = "52:54:00:12:34:56"
	netInitNICMAC       = "0a:58:0a:14:00:05"
	netInitNADCIDR      = "10.20.0.5/24"
	netInitSecondaryEnv = "secondary-nad.env"
)

// fakeIP stands in for iproute2: it logs every call and reports FAKE_NAD_CIDR as
// the Multus interface's IPv4 address (none when empty).
const fakeIP = `#!/bin/sh
echo "$*" >> "$IP_LOG"
case "$*" in
    "-4 addr show "*) [ -n "$FAKE_NAD_CIDR" ] && echo "    inet $FAKE_NAD_CIDR scope global $3" ;;
    "-o link show "*) echo "3: $4: <BROADCAST,MULTICAST,UP> mtu 1400 qdisc noqueue" ;;
esac
exit 0
`

// fakeCat answers the sysfs MAC read (absent on the test host) and defers everything else to the real cat.
const fakeCat = `#!/bin/sh
case "$1" in
    /sys/class/net/*/address) echo "$FAKE_NIC_MAC"; exit 0 ;;
esac
PATH="$REAL_PATH" exec cat "$@"
`

type netInitResult struct {
	out    string
	ipLog  string
	runDir string
}

// runNetworkInit runs network-init.sh for a guest with one secondary NIC on a NAD,
// with the NAD interface holding nadCIDR (none when empty).
func runNetworkInit(t *testing.T, nadCIDR string) (netInitResult, error) {
	t.Helper()
	for _, bin := range []string{"sh", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	runDir := filepath.Join(root, "run")
	intentPath := filepath.Join(root, "runtime-intent.json")
	ipLog := filepath.Join(root, "ip.log")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"ip": fakeIP, "cat": fakeCat} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	intent := runtimeintent.RuntimeIntent{
		GuestID: netInitGuestID,
		Network: true,
		NICs: []runtimeintent.NICIntent{{
			Name:            "data",
			Type:            "bridge",
			TapDevice:       netInitTap,
			MAC:             netInitGuestMAC,
			MultusInterface: netInitMultusIface,
			Bridge:          netInitBridge,
		}},
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intentPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", networkInitScript)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBESWIFT_INTENT_PATH="+intentPath,
		"KUBESWIFT_RUN_DIR="+runDir,
		"IP_LOG="+ipLog,
		"REAL_PATH="+os.Getenv("PATH"),
		"FAKE_NAD_CIDR="+nadCIDR,
		"FAKE_NIC_MAC="+netInitNICMAC,
	)
	out, runErr := cmd.CombinedOutput()
	logBytes, _ := os.ReadFile(ipLog)
	return netInitResult{
		out:    string(out),
		ipLog:  string(logBytes),
		runDir: filepath.Join(runDir, strings.ReplaceAll(netInitGuestID, "/", "-")),
	}, runErr
}

// A bridge-only NAD (no IPAM) leaves the interface without an IP; the guest
// configures itself, so network-init must bridge it instead of failing.
func TestNetworkInit_SecondaryNADWithoutIPFallsBackToBridge(t *testing.T) {
	res, err := runNetworkInit(t, "")
	if err != nil {
		t.Fatalf("network-init failed on a secondary NAD without IPAM: %v\n%s", err, res.out)
	}
	if !strings.Contains(res.out, "falling back to a plain bridge") {
		t.Errorf("fallback was not logged:\n%s", res.out)
	}
	if !strings.Contains(res.ipLog, "link set "+netInitMultusIface+" master "+netInitBridge) {
		t.Errorf("%s was not enslaved to %s:\n%s", netInitMultusIface, netInitBridge, res.ipLog)
	}
	if strings.Contains(res.ipLog, "addr flush") {
		t.Errorf("the bridge fallback must not flush addresses:\n%s", res.ipLog)
	}
	if _, err := os.Stat(filepath.Join(res.runDir, netInitSecondaryEnv)); !os.IsNotExist(err) {
		t.Errorf("%s written for a NAD without an IP (err=%v); the launcher would start a dnsmasq with no lease", netInitSecondaryEnv, err)
	}
}

// A NAD with IPAM keeps the IP hand-off into the guest.
func TestNetworkInit_SecondaryNADWithIPHandsItOff(t *testing.T) {
	res, err := runNetworkInit(t, netInitNADCIDR)
	if err != nil {
		t.Fatalf("network-init failed: %v\n%s", err, res.out)
	}
	if strings.Contains(res.out, "falling back to a plain bridge") {
		t.Errorf("a NAD with an IP took the bridge fallback:\n%s", res.out)
	}
	env, err := os.ReadFile(filepath.Join(res.runDir, netInitSecondaryEnv))
	if err != nil {
		t.Fatalf("%s not written: %v\n%s", netInitSecondaryEnv, err, res.out)
	}
	for _, want := range []string{"SEC_IP=10.20.0.5", "SEC_PREFIX=24", "SEC_MAC=" + netInitGuestMAC, "SEC_BRIDGE=" + netInitBridge} {
		if !strings.Contains(string(env), want) {
			t.Errorf("%s missing %q:\n%s", netInitSecondaryEnv, want, env)
		}
	}
	if !strings.Contains(res.ipLog, "addr flush dev "+netInitMultusIface) {
		t.Errorf("the NAD IP was not flushed off %s:\n%s", netInitMultusIface, res.ipLog)
	}
}
