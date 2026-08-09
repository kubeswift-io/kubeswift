# Audit logging

KubeSwift's gateway records every RPC it serves (`kubeswift-gateway`, v0.13.5+):
mutations unconditionally, reads at `-v=1`. That covers the UI and anything else
going through the gateway.

It does **not** cover `kubectl`. A `kubectl delete swiftguest` never touches the
gateway, so nothing on the KubeSwift side sees it. Closing that needs apiserver
audit logging, which is cluster config, not a KubeSwift setting.

## Why you want it for VMs specifically

A SwiftGuest carries no finalizer and no owner reference. A delete therefore
takes the object, its launcher pod and its events at the same time and leaves
nothing behind to inspect — there is no tombstone, no terminating state to catch,
and the PVC is the only survivor. If you did not record the call, the deletion is
unattributable afterwards. This is not hypothetical; it is why the gateway grew
an audit interceptor.

## Policy

`RequestResponse` on deletes of KubeSwift objects — you want the object body, so
you can see *what* was removed, not just that something was. Everything else
stays at `Metadata` or is dropped, because the volume otherwise buries the lines
you care about.

```yaml
# /var/lib/k0s/audit-policy.yaml
apiVersion: audit.k8s.io/v1
kind: Policy
# Drop the reads first: without this, watch/list traffic from controllers and
# the gateway swamps the log and the delete you are looking for scrolls away.
omitStages:
  - RequestReceived
rules:
  # 1. KubeSwift object deletions — the whole point. Full body.
  - level: RequestResponse
    verbs: ["delete", "deletecollection"]
    resources:
      - group: "swift.kubeswift.io"
      - group: "sandbox.kubeswift.io"
      - group: "gpu.kubeswift.io"
      - group: "snapshot.kubeswift.io"
      - group: "migration.kubeswift.io"
      - group: "image.kubeswift.io"
      - group: "kernel.kubeswift.io"
      - group: "seed.kubeswift.io"
      - group: "fleet.kubeswift.io"
      - group: "cells.kubeswift.io"

  # 2. Other KubeSwift writes — who changed a spec, without the body.
  - level: Metadata
    verbs: ["create", "update", "patch"]
    resources:
      - group: "swift.kubeswift.io"
      - group: "sandbox.kubeswift.io"
      - group: "gpu.kubeswift.io"
      - group: "snapshot.kubeswift.io"
      - group: "migration.kubeswift.io"
      - group: "image.kubeswift.io"
      - group: "kernel.kubeswift.io"
      - group: "seed.kubeswift.io"
      - group: "fleet.kubeswift.io"
      - group: "cells.kubeswift.io"

  # 3. Launcher pods and the seed Secrets are the node-level trust boundary.
  - level: Metadata
    verbs: ["create", "update", "patch", "delete"]
    resources:
      - group: ""
        resources: ["pods", "secrets", "serviceaccounts"]

  # 4. Drop the rest of the read traffic.
  - level: None
    verbs: ["get", "list", "watch"]

  # 5. Everything else, one line each.
  - level: Metadata
```

The group list has to be enumerated — audit policy has no `*.kubeswift.io`
wildcard — so it will go stale as groups are added. That is survivable by
design: rule 4 only drops reads, so a delete in a group nobody added to rule 1
still lands via rule 5 at `Metadata`. You lose the object body, not the fact
that it happened. Keep it that way round; a policy whose omissions are silent is
the thing this document exists to avoid.

Verify the list against a live cluster rather than trusting it:

```bash
kubectl get crd -o jsonpath='{range .items[*]}{.spec.group}{"\n"}{end}' \
  | grep kubeswift | sort -u
```

## Applying it on k0s

k0s runs the apiserver as a host process; the flags live in the k0s config, not
in a static pod manifest. On **each controller node**:

1. Write the policy above to `/var/lib/k0s/audit-policy.yaml`.
2. Add the flags to `/etc/k0s/k0s.yaml`:

```yaml
spec:
  api:
    extraArgs:
      audit-policy-file: /var/lib/k0s/audit-policy.yaml
      audit-log-path: /var/log/k0s/audit.log
      audit-log-maxage: "30"
      audit-log-maxbackup: "10"
      audit-log-maxsize: "100"
```

3. `sudo systemctl restart k0scontroller`, then confirm the API comes back:

```bash
kubectl get --raw /readyz
```

**The restart interrupts the API server.** Workloads and running VMs keep going —
the kubelet and Cloud Hypervisor do not depend on the apiserver being up — but
controllers stall and `kubectl` fails for the duration. Do it in a window where
that is acceptable, and one controller at a time on an HA control plane.

## Reading it

```bash
# who deleted a SwiftGuest, and what it looked like
sudo grep '"resource":"swiftguests"' /var/log/k0s/audit.log \
  | jq -r 'select(.verb=="delete")
           | "\(.requestReceivedTimestamp)  \(.user.username)  \(.objectRef.namespace)/\(.objectRef.name)"'
```

The `user.username` is the real caller. Note that a gateway-proxied delete shows
up as the **impersonated** user (the gateway sets impersonation headers), with
`impersonatedUser` recording it — so the gateway log and the audit log agree on
who asked.

## Retention

`audit-log-maxage: 30` keeps a month. That is the number that matters: the
incident which prompted this was reported a day late, and the gateway's own log
happened to still cover it only because the pod had not restarted. Do not rely
on pod lifetime for forensics.
