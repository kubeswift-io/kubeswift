package swiftguest

import (
	"fmt"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/webhook/hostpath"
)

// checkHostPaths re-applies the host-path allowlist in the CONTROLLER, not only
// in the validating webhook.
//
// The webhook is the primary gate, but `webhook.enabled` defaults to false
// (it requires cert-manager), so on a default install nothing would enforce
// this at all. The same structural gap already bit the snapshot backend, whose
// hostPath prefix rule also lives only in its webhook — a comment there even
// says "the webhook ensures", which is not a control.
//
// Since the launcher is privileged, an unconfined host path is node-root. A
// guard that only runs in an optional component is not a guard, so it is
// enforced here too: the controller refuses to build the pod, which surfaces
// as a Resolved=False condition rather than a silently over-privileged VM.
func checkHostPaths(guest *swiftv1alpha1.SwiftGuest, allowed []string) error {
	spec := &guest.Spec
	for i := range spec.Filesystems {
		fs := &spec.Filesystems[i]
		if fs.Source.HostPath == nil {
			continue
		}
		if err := hostpath.Validate(
			fmt.Sprintf("spec.filesystems[%d].source.hostPath", i),
			*fs.Source.HostPath, allowed); err != nil {
			return err
		}
	}
	// vhost-user sockets: the pod builder mounts filepath.Dir(socket).
	for i := range spec.Interfaces {
		if sock := spec.Interfaces[i].Socket; sock != "" {
			if err := hostpath.ValidateDir(
				fmt.Sprintf("spec.interfaces[%d].socket", i), sock, allowed); err != nil {
				return err
			}
		}
	}
	for i := range spec.VhostUserDevices {
		if sock := spec.VhostUserDevices[i].Socket; sock != "" {
			if err := hostpath.ValidateDir(
				fmt.Sprintf("spec.vhostUserDevices[%d].socket", i), sock, allowed); err != nil {
				return err
			}
		}
	}
	return nil
}
