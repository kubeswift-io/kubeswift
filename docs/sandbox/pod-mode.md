# Pod mode

Pod mode runs an ordinary Pod's container in a microVM. The Pod sets
`runtimeClassName: kubeswift-microvm` in a namespace that opted in, and nothing
else changes for whoever created it: Deployments, Jobs, Services,
NetworkPolicies, quota and kubernetes-sigs/agent-sandbox (Sandbox,
SandboxTemplate, SandboxWarmPool, SandboxClaim, its SDK and router) work
unmodified. Preview, off by default.

```yaml
apiVersion: v1
kind: Pod
metadata: {name: web, namespace: team-a}
spec:
  runtimeClassName: kubeswift-microvm
  containers:
  - name: main
    image: docker.io/library/busybox:1.37.0
    command: [sh, -c, "mkdir -p /www && uname -r > /www/index.html && exec httpd -f -p 8080 -h /www"]
    ports: [{containerPort: 8080}]
    resources: {limits: {cpu: "1", memory: 512Mi}}
```

## How it works

Admission rewrites the Pod into a **stand-in**. The stand-in keeps the Pod's
name, labels, annotations, owner, scheduling, resource requests and readiness
probe. It runs one container, the `pod-shim`: unprivileged, no shell, Pod
Security `restricted`. The workload runs in a SwiftSandbox `pm-<pod uid>` in
the pod-mode namespace (`kubeswift-sandboxes`), on the stand-in's node, with
network mode `restricted`. Deleting the Pod deletes the microVM.

| On the Pod | Where it goes |
|---|---|
| Traffic to the Pod's TCP ports | the shim forwards it to the microVM |
| `kubectl exec <pod> -- sh` (or `bash`, `env CMD`) | runs in the guest, through a relay in the controller-manager |
| `kubectl logs <pod>` | the guest console |
| readiness | Ready only while the microVM runs (readiness gate `podmode.kubeswift.io/microvm-ready`) and the Pod's own probe passes |
| the workload exits | the Pod ends `Succeeded` or `Failed` with its exit code; with `restartPolicy` `Always` (or `OnFailure` after a failure) the next run boots a new microVM and the restart count goes up |

The microVM's size is the container's CPU limit (else request, rounded up, at
least 1 vCPU) and memory limit (else request, else 512Mi, at least 256Mi).

## Enable it

1. Install the chart with `webhook.enabled` (the default) and
   `sandboxMaterialize.podMode.enabled: true`. It needs Kubernetes 1.30 or later
   (ValidatingAdmissionPolicy). It creates the pod-mode namespace (Pod Security
   `privileged`, for the launchers), the RuntimeClass, the webhook, the
   admission policies and the relay's RBAC. It also runs the controller-manager
   with at least 2 replicas and a PodDisruptionBudget, because Pod creation in
   opted-in namespaces depends on its webhook.

2. Create the kernel the microVMs boot, in the pod-mode namespace:

   ```yaml
   apiVersion: kernel.kubeswift.io/v1alpha1
   kind: SwiftKernel
   metadata: {name: sandbox, namespace: kubeswift-sandboxes}
   spec:
     profile: sandbox
     kernelCmdline: console=ttyS0
     ociRef: {image: ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.25}
   ```

3. Opt a namespace in:

   ```bash
   kubectl label namespace team-a kubeswift.io/pod-mode=enabled
   ```

Stand-ins schedule only on nodes labeled `kubeswift.io/kernel-node=true`.

## What carries over

Into the guest:
- image, command, args, working directory;
- env;
- TCP ports;
- `runAsUser`, `runAsGroup`, `runAsNonRoot`;
- Secrets and ConfigMaps (below).

`emptyDir` volumes are accepted and not mounted: the guest's root filesystem is
writable.

Refused, by field name, at admission:
- more than one container, and init containers;
- liveness and startup probes, lifecycle hooks, `tty`/`stdin`;
- host namespaces;
- volumes other than `emptyDir`, `secret` and `configMap` (projected volumes
  too), and `subPathExpr`;
- UDP ports and host ports;
- `privileged`, added capabilities, resource claims;
- `valueFrom` other than `secretKeyRef` and `configMapKeyRef`;
- env names other than letters, digits and `_` (the guest sets the
  environment with its shell).

Fields that only harden the container are dropped, since the microVM is the
boundary, and so is the injected ServiceAccount token.

## Secrets and ConfigMaps

`env[].valueFrom.secretKeyRef` / `configMapKeyRef`, `envFrom` (`secretRef`,
`configMapRef`, `prefix`) and Secret and ConfigMap volumes (`items`, modes,
`optional`, `subPath`) work as on a Pod. When it creates each microVM, the
controller copies exactly the keys the Pod names, from the Pod's namespace, into
one Secret `pm-<pod uid>` in the pod-mode namespace. The SwiftSandbox refers to
it by key, so no value is written into the SwiftSandbox, and the launcher can
read only that Secret. The copy is deleted with the Pod.

A missing Secret, ConfigMap or key that is not `optional` holds the microVM
until it exists, as the kubelet holds a container. The readiness gate shows
`MicroVMConfigError` and the reason. An `envFrom` key that is not a valid
variable name in the guest is left out, with an `InvalidEnvironmentVariableNames`
event.

Unlike on a Pod, files are written once when the microVM starts, are not
refreshed, and are writable in the guest.

## Security

- The stand-in is unprivileged and runs nothing but the shim. A
  ValidatingAdmissionPolicy (`kubeswift-pod-mode`) keeps it that way: no other
  container, no ephemeral containers, no changes to its workload. The
  controller deletes a stand-in that was changed anyway.
- It fails closed. The webhook is `failurePolicy: Fail`, and the policy refuses
  any Pod that still names the RuntimeClass after admission, such as one in a
  namespace that did not opt in. The RuntimeClass's handler exists on no node.
- `kubectl exec` and `kubectl logs` go through the relay. The shim
  authenticates with its own ServiceAccount token, which has audience
  `kubeswift-podmode` and is bound to the Pod, and the relay serves that Pod's
  microVM only. Whoever may exec into the Pod may exec into its microVM, and no
  tenant gets exec on a launcher. A second policy
  (`kubeswift-pod-mode-exec-gate`) limits the controller-manager's own exec to
  the guest agent bridge of a pod-mode launcher.
- The stand-in's egress is limited to the relay and to its microVM's launcher,
  and only the stand-in may reach the launcher. The guest has `restricted`
  egress: DNS and the internet; no node addresses, API server or cloud metadata.
- Reading Secrets: the controller reads only what an admitted Pod names, in the
  Pod's own namespace. Anyone who may create that Pod could mount those Secrets
  anyway.

## Troubleshooting

`kubectl describe pod <pod>` shows the readiness gate's reason and the events:

| Reason | Meaning |
|---|---|
| `MicroVMStarting` | the SwiftSandbox is being created or is booting; the message has its phase |
| `MicroVMConfigError` | a referenced Secret, ConfigMap or key is missing |
| `MicroVMRestarting` | the previous run's microVM is being removed before the next boots |
| `MicroVMExited` | the workload ended |
| event `NamespaceNotOptedIn` | a stand-in label in a namespace without `kubeswift.io/pod-mode=enabled` |
| event `StandInTampered` | the stand-in was changed; it and its microVM are deleted |

A Deployment or Job in a namespace that enforces Pod Security `restricted` may
warn `would violate PodSecurity` when it is created. The warning is about the
Pod template as written. Its Pods are rewritten into stand-ins, which pass.

The microVM itself is `kubectl -n kubeswift-sandboxes get swiftsandbox pm-<pod uid>`.

## Limits

- One container per Pod; network mode `restricted` only.
- `kubectl exec` does not see the workload's environment variables.
- The node counts the VM's memory twice: once for the stand-in's requests and
  once for the launcher's.
- `kubectl attach` attaches to the shim, not the workload. `kubectl
  port-forward` works on the Pod's declared ports (the shim forwards them).

## Test

`make sandbox-pod-mode-test` (`test/sandbox/pod-mode.sh`) runs, on a cluster
with pod mode enabled and a KVM node:
- fail-closed admission;
- a Service, exec and logs on a Pod;
- Secrets and ConfigMaps, including a late Secret;
- exit codes, a Job and restarts;
- relay authorization;
- teardown.

`make sandbox-pod-mode-test-cleanup` removes what it made.
