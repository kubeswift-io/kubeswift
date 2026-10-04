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
