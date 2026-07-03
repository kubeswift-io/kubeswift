package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CDI constants for the reference driver. cdiVersion 0.6.0 is what containerd
// 1.7.x understands (the dev cluster's runtime); the spec only uses env-only
// containerEdits, which every CDI version supports.
const (
	cdiVersion = "0.6.0"
	cdiKind    = "kubeswift.io/gpu"
)

// cdiSpec is the minimal CDI spec JSON shape the driver writes. Deliberately
// hand-rolled (no tags.cncf.io dependency): env-only edits, one device per
// claim.
type cdiSpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"`
	Devices    []cdiDevice `json:"devices"`
}

type cdiDevice struct {
	Name           string            `json:"name"`
	ContainerEdits cdiContainerEdits `json:"containerEdits"`
}

type cdiContainerEdits struct {
	Env []string `json:"env,omitempty"`
}

// writeClaimCDISpec writes the per-claim CDI spec file: a single CDI device
// (named after the claim UID) whose containerEdits inject the device identity
// envs that gpu-init and swiftletd consume:
//
//	GPU_PCI_ADDRESSES=<bdf>[,<bdf>...]
//	GPU_PARTITION_ID=-1
//
// One CDI device per CLAIM (not per GPU): the env is a single combined list,
// so per-device CDI entries would collide on the same variable.
// Returns the fully-qualified CDI device ID ("kubeswift.io/gpu=claim-<uid>").
func writeClaimCDISpec(cdiDir, claimUID string, bdfs []string) (string, error) {
	deviceName := "claim-" + claimUID
	spec := cdiSpec{
		CDIVersion: cdiVersion,
		Kind:       cdiKind,
		Devices: []cdiDevice{{
			Name: deviceName,
			ContainerEdits: cdiContainerEdits{
				Env: []string{
					"GPU_PCI_ADDRESSES=" + joinBDFs(bdfs),
					"GPU_PARTITION_ID=-1",
				},
			},
		}},
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cdiDir, 0o755); err != nil {
		return "", fmt.Errorf("create CDI dir %s: %w", cdiDir, err)
	}
	// Atomic write: temp file + rename, so the runtime never reads a torn spec.
	path := claimCDISpecPath(cdiDir, claimUID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", fmt.Errorf("write CDI spec %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("rename CDI spec into place: %w", err)
	}
	return cdiKind + "=" + deviceName, nil
}

// removeClaimCDISpec removes the per-claim CDI spec file (idempotent).
func removeClaimCDISpec(cdiDir, claimUID string) error {
	err := os.Remove(claimCDISpecPath(cdiDir, claimUID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func claimCDISpecPath(cdiDir, claimUID string) string {
	return filepath.Join(cdiDir, "gpu.kubeswift.io-claim-"+claimUID+".json")
}

func joinBDFs(bdfs []string) string {
	out := ""
	for i, b := range bdfs {
		if i > 0 {
			out += ","
		}
		out += b
	}
	return out
}
