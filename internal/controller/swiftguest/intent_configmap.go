package swiftguest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnvIntentSHA256 carries the SHA-256 (hex) of the runtime intent a launcher
// was built for. It is set in the pod spec, which cannot change once the pod
// exists, and swiftletd refuses to start on an intent file with another
// digest. A launcher without it skips the check.
const EnvIntentSHA256 = "KUBESWIFT_INTENT_SHA256"

// IntentDigest is the EnvIntentSHA256 value for a serialized intent.
func IntentDigest(intentJSON []byte) string {
	sum := sha256.Sum256(intentJSON)
	return hex.EncodeToString(sum[:])
}

// SetIntentDigest stamps EnvIntentSHA256 on the pod's "launcher" container.
func SetIntentDigest(pod *corev1.Pod, intentJSON []byte) {
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != "launcher" {
			continue
		}
		for j := range c.Env {
			if c.Env[j].Name == EnvIntentSHA256 {
				c.Env[j].Value = IntentDigest(intentJSON)
				return
			}
		}
		c.Env = append(c.Env, corev1.EnvVar{Name: EnvIntentSHA256, Value: IntentDigest(intentJSON)})
		return
	}
}

// IntentConflictError reports a ConfigMap at a launcher's intent name that the
// launcher's owner does not control.
type IntentConflictError struct{ Name string }

func (e *IntentConflictError) Error() string {
	return fmt.Sprintf("ConfigMap %s exists and is not KubeSwift's: a launcher reads its configuration from it, so it is never used; delete it", e.Name)
}

// EnsureImmutableIntentConfigMap creates cm, immutable, controlled by owner,
// and returns nil once a ConfigMap by that name holds exactly cm's data and is
// controlled by owner.
//
// The launcher pod mounts its intent by name, and the privileged launcher
// starts the VM from it. A ConfigMap of that name that someone else created
// first used to be accepted as it was, so a tenant able to create ConfigMaps
// chose what the launcher ran. A ConfigMap owner does not control is refused
// (IntentConflictError) and never modified. One it controls with other data is
// replaced, since it is immutable.
//
// For intents whose launcher pod does not exist yet. A launcher already
// running keeps the intent it started with.
func EnsureImmutableIntentConfigMap(ctx context.Context, c client.Client, owner metav1.Object, cm *corev1.ConfigMap) error {
	cm.Immutable = ptr.To(true)
	err := c.Create(ctx, cm)
	if err == nil || !apierrors.IsAlreadyExists(err) {
		return err
	}
	var existing corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), &existing); err != nil {
		return err
	}
	if !metav1.IsControlledBy(&existing, owner) {
		return &IntentConflictError{Name: cm.Name}
	}
	if ptr.Deref(existing.Immutable, false) && equality.Semantic.DeepEqual(existing.Data, cm.Data) {
		return nil
	}
	// Ours, but stale or mutable: replace it. The UID precondition keeps a
	// ConfigMap someone recreated in between from being deleted.
	if err := c.Delete(ctx, &existing, client.Preconditions{UID: &existing.UID}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return c.Create(ctx, cm)
}
