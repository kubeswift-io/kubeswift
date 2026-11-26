package swiftsandbox

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ValidateEnv checks spec.env. A value may come from a Secret
// (valueFrom.secretKeyRef), which the launcher reads with its own account and
// hands to the guest without writing it anywhere else. Other valueFrom
// sources have no meaning for a sandbox, and are refused rather than dropped.
// The controller runs it too.
func ValidateEnv(env []corev1.EnvVar) error {
	for i, e := range env {
		if e.Name == "" {
			return fmt.Errorf("spec.env[%d].name is required", i)
		}
		vf := e.ValueFrom
		if vf == nil {
			continue
		}
		if e.Value != "" {
			return fmt.Errorf("spec.env[%d] (%s): set value or valueFrom, not both", i, e.Name)
		}
		if vf.FieldRef != nil || vf.ResourceFieldRef != nil || vf.ConfigMapKeyRef != nil || vf.FileKeyRef != nil {
			return fmt.Errorf("spec.env[%d] (%s): only valueFrom.secretKeyRef is supported", i, e.Name)
		}
		ref := vf.SecretKeyRef
		if ref == nil || ref.Name == "" || ref.Key == "" {
			return fmt.Errorf("spec.env[%d] (%s): valueFrom.secretKeyRef needs a name and a key", i, e.Name)
		}
	}
	return nil
}

// GuestEnvName is whether name can be set as a variable on a cold boot: the
// guest's init sets the workload's environment with its shell, which takes
// only letters, digits and _, not starting with a digit. Kubernetes allows
// more (a "." or "-"), and such a name ended init and panicked the guest. A
// warm checkout passes env through the guest agent, which takes any name.
func GuestEnvName(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}
