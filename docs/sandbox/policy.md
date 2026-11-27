# Sandbox policy pack

Platform rules for agent sandboxes, enforced by the API server. Off by default.
With `sandboxPolicy.enabled: true` the chart installs ValidatingAdmissionPolicies
(Kubernetes 1.30+) that apply to SwiftSandboxes and SwiftSandboxPools in
namespaces labelled:

```bash
kubectl label namespace agents kubeswift.io/sandbox-policy=enforced
```

Unlabelled namespaces are not affected.

| Rule | Value | Default |
|---|---|---|
| The network mode is one of these | `sandboxPolicy.allowedNetworkModes` | `[brokered, proxied]` |
| No Secret reaches the guest: no `env[].valueFrom.secretKeyRef`, no `secretFiles` | `sandboxPolicy.forbidGuestSecrets` | `true` |
| `nestedVirtualization: Enabled` is refused | `sandboxPolicy.requireNestedDisabled` | `true` |
| An egress proxy is a chart preset, or one of these images | `sandboxPolicy.allowedProxyImages` | `[]` (presets only) |
| What a violation does | `sandboxPolicy.validationActions` | `[Deny]` |

The defaults describe an agent sandbox whose only way out is an egress proxy
that holds its credentials: the guest has an allowlist and placeholders, never
the real secret ([modes proxied and brokered](overview.md#network-modes),
[egress credentials](overview.md#egress-credentials)). A sandbox the guest could use to reach
anything, or that hands it a token, is refused with a message naming the rule.

Try the rules first with `validationActions: [Warn, Audit]`: violations are
admitted with a warning to the client and an audit annotation.

## What it does not do

- Only a create, or a change of `spec`, is checked. An object admitted before
  its namespace was labelled keeps running, and its metadata (finalizers,
  labels) can still change, so it can always be deleted. A change of its spec
  must comply.
- A secrets rule does not apply to a pool: a pool carries no Secrets, a
  checkout brings its own.
- Pod mode runs its microVMs in the pod-mode namespace (`kubeswift-sandboxes`),
  network mode `restricted`. Label that namespace and pod mode stops working;
  leave it unlabelled.
- The label is the opt-in. Whoever may edit a namespace's labels can remove it,
  so keep label edits on tenant namespaces with the platform team.

## Kyverno

The same rules for clusters that use Kyverno instead (shown with the defaults):

```yaml
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: kubeswift-sandbox-policy
spec:
  validationFailureAction: Enforce
  background: false
  rules:
  - name: network-mode
    match:
      any:
      - resources:
          kinds: [sandbox.kubeswift.io/v1alpha1/SwiftSandbox, sandbox.kubeswift.io/v1alpha1/SwiftSandboxPool]
          namespaceSelector:
            matchLabels: {kubeswift.io/sandbox-policy: enforced}
    validate:
      message: "network mode must be brokered or proxied"
      deny:
        conditions:
          any:
          - key: "{{ request.object.spec.network.mode || 'restricted' }}"
            operator: AnyNotIn
            value: [brokered, proxied]
  - name: no-guest-secrets
    match:
      any:
      - resources:
          kinds: [sandbox.kubeswift.io/v1alpha1/SwiftSandbox]
          namespaceSelector:
            matchLabels: {kubeswift.io/sandbox-policy: enforced}
    validate:
      message: "no env valueFrom.secretKeyRef and no secretFiles; use spec.network.egress.credentials"
      deny:
        conditions:
          any:
          - key: "{{ length(request.object.spec.env[?valueFrom.secretKeyRef] || `[]`) }}"
            operator: GreaterThan
            value: 0
          - key: "{{ length(request.object.spec.secretFiles || `[]`) }}"
            operator: GreaterThan
            value: 0
  - name: nested-disabled
    match:
      any:
      - resources:
          kinds: [sandbox.kubeswift.io/v1alpha1/SwiftSandbox, sandbox.kubeswift.io/v1alpha1/SwiftSandboxPool]
          namespaceSelector:
            matchLabels: {kubeswift.io/sandbox-policy: enforced}
    validate:
      message: "nestedVirtualization: Enabled is not allowed"
      pattern:
        spec:
          =(nestedVirtualization): "!Enabled"
  - name: proxy-presets-only
    match:
      any:
      - resources:
          kinds: [sandbox.kubeswift.io/v1alpha1/SwiftSandbox, sandbox.kubeswift.io/v1alpha1/SwiftSandboxPool]
          namespaceSelector:
            matchLabels: {kubeswift.io/sandbox-policy: enforced}
    validate:
      message: "the egress proxy must be a preset"
      pattern:
        spec:
          =(network):
            =(egress):
              =(proxy):
                X(image): "null"
```

The Kyverno version is an example and is not tested by KubeSwift; the chart's
policies are.

## Test

`make sandbox-policy-test` (`test/sandbox/sandbox-policy.sh`) checks each rule
with server-side dry-run, plus a pool admitted before the label: its metadata
can change and it can be deleted, its spec cannot change while it does not
comply.
