package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestValidateEnv(t *testing.T) {
	secret := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "url"}}
	if err := ValidateEnv([]corev1.EnvVar{{Name: "A", Value: "1"}, {Name: "DB", ValueFrom: secret}}); err != nil {
		t.Errorf("literal and secretKeyRef: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		e          corev1.EnvVar
	}{
		{"fieldRef", "only valueFrom.secretKeyRef", corev1.EnvVar{Name: "X", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}},
		{"resourceFieldRef", "only valueFrom.secretKeyRef", corev1.EnvVar{Name: "X", ValueFrom: &corev1.EnvVarSource{
			ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}}},
		{"configMapKeyRef", "only valueFrom.secretKeyRef", corev1.EnvVar{Name: "X", ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "c"}, Key: "k"}}}},
		{"value and valueFrom", "not both", corev1.EnvVar{Name: "X", Value: "v", ValueFrom: secret}},
		{"no key", "needs a name and a key", corev1.EnvVar{Name: "X", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}}}}},
		{"no name", "name is required", corev1.EnvVar{Value: "v"}},
	} {
		err := ValidateEnv([]corev1.EnvVar{tc.e})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}
