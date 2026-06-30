# KubeSwift Controllers

## SwiftImage controller

The SwiftImage controller reconciles `SwiftImage` resources through lifecycle phases: Pending, Importing, Validating, Preparing, Ready, Failed.

### Immutability when Ready

When `status.phase` is Ready, SwiftImage spec MUST NOT be mutated. Enforcement options:

- **Webhook (recommended)**: Add a validating webhook in add-api-validation-and-defaulting that rejects spec changes when `status.phase` is Ready.
- **Controller**: The controller returns early when Ready and does not process spec changes. A webhook is still recommended to reject at admission time.

Status-only updates (e.g., conditions) are allowed when Ready.

## SwiftGuest controller

The SwiftGuest controller reconciles `SwiftGuest` resources by creating and managing pods that run guests via swiftletd.

### Resolver integration (required)

**The controller MUST call `resolved.Resolver.Resolve` first** before performing any reconciliation logic. Resolution fetches referenced resources (SwiftGuestClass, SwiftImage, SwiftSeedProfile), validates existence and compatibility, and produces a normalized `ResolvedGuest` model.

- **On `ResolutionError`**: The controller MUST set a condition on the SwiftGuest (e.g. `Resolved=False`) with the error reason, and return without creating or updating the pod. Do not use any guest spec fields directly when resolution fails.
- **On success**: The controller MUST use only the returned `ResolvedGuest` for all subsequent logic (building runtime intent, pod spec, etc.). Do not read from SwiftGuest.Spec or referenced CRDs for runtime decisions.

### Example invocation

```go
import (
    "github.com/kubeswift-io/kubeswift/internal/resolved"
)

func (r *SwiftGuestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var guest swiftv1alpha1.SwiftGuest
    if err := r.Client.Get(ctx, req.NamespacedName, &guest); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    res := resolved.NewResolver(r.Client)
    rg, err := res.Resolve(ctx, &guest)
    if err != nil {
        var re *resolved.ResolutionError
        if errors.As(err, &re) {
            // Set condition Resolved=False with re.Reason, re.AffectedResource
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }

    // Use rg (ResolvedGuest) only for building pod and runtime intent
    // ...
}
```
