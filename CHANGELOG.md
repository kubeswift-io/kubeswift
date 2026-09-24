# Changelog

All notable changes to KubeSwift are documented here.

---

## [Unreleased]

### Security

- **The privileged image-import Job ran whatever a mutable tag served.** The
  import, OCI-import and measure Jobs used `ubuntu:22.04` and `apt-get
  install`ed curl, qemu-utils and util-linux on every run, as root and (for
  Linux images) privileged, pulling from whatever the tag and the package
  mirror served that day. They now run the launcher image, which already
  carries those tools and is on every node that runs guests, the same way
  the root-disk clone Job does. CI's raw tool downloads (kind, trivy,
  kubeconform, kube-linter, gitleaks) are now checked against pinned sha256
  sums.

- **Gateway console and sandbox-exec sessions left no audit record.** The
  raw WebSocket routes bypass the Connect audit interceptor, so opening a
  console into a (privileged) launcher, or running a command in a sandbox,
  logged nothing naming who did it. Each session now logs `ws session opened`
  and `ws session closed` lines with the user, cluster, namespace, target,
  pod, duration and, for sandbox exec, the command (truncated at 512 bytes).

- **The controller's metrics can now be served to authorized scrapers
  only.** `/metrics` is plain HTTP to anyone who can reach the pod, and it
  names every tenant's guests, images and namespaces.
  `controllerManager.metrics.secure=true` (flag `--metrics-secure`) serves it
  over HTTPS and only to callers the API server authenticates (TokenReview)
  and authorizes to `get /metrics` (SubjectAccessReview). Bind the new
  `kubeswift-metrics-reader` ClusterRole to your scraper; the ServiceMonitor
  follows the setting. It is off by default because it changes how every
  scraper connects. The controller also gained `/healthz` and `/readyz`
  probes. Readiness waits for the webhook server when webhooks are enabled,
  since their `failurePolicy: Fail` made a Ready-but-not-serving pod fail
  every guarded write.

- **The kustomize install (`make deploy`, the local-cluster quickstart, e2e)
  lagged the Helm chart's security hardening.** Its controller ClusterRole
  still granted what the chart had removed (writes to `swiftgpuprofiles`,
  create/delete on `swiftgpunodes`, write verbs on `pods/log`). Its launcher
  reporter role carried extra `update` verbs. It never installed the sandbox
  reporter role or the launcher-ServiceAccount admission gate, and the
  controller Deployment had no securityContext. The controller RBAC and the
  admission gate are now generated from the chart templates by `make
  generate` (`hack/sync-kustomize.sh`), and CI fails if they drift. The
  Deployment gets the chart's non-root, read-only, no-capabilities context.
  `config/default` now includes a ValidatingAdmissionPolicy, which needs
  Kubernetes 1.30+.

- **swiftletd wiped whatever directory a snapshot capture named.** Before a
  capture, swiftletd empties the destination directory, and it took that
  path from the launcher pod's `snapshot-action-args` annotation without
  checking it. Every launcher mounts the node-wide
  `/var/lib/kubeswift/snapshots` read-write, so anyone who could patch a
  launcher pod could point a capture at that root and delete every
  namespace's snapshots on the node. swiftletd now accepts only
  `file:///var/lib/kubeswift/snapshots/<name>/`, with `<name>` a single safe
  segment, which is the rule the controller already applies.

- **A checked-out warm-pool sandbox ran with no ingress isolation.** A warm
  slot's deny-ingress NetworkPolicy selected the pod by its sandbox label, and
  checkout rewrites that label to the claiming sandbox's name. From the moment
  of checkout the policy matched nothing, and the workload accepted inbound
  traffic from the whole cluster. Slot pods now carry a stable slot-name label
  that the policy selects on. At checkout the slot's NetworkPolicy and intent
  ConfigMap move to the claiming sandbox, as its pod does, so they are removed
  with it instead of piling up until the pool is deleted.

- **A legacy token Secret could mint a launcher ServiceAccount token.** The
  launcher-SA admission gate stops a pod from naming a launcher ServiceAccount,
  but creating a `kubernetes.io/service-account-token` Secret annotated with
  that ServiceAccount still had the token controller mint a long-lived token
  for it, with no pod involved. That token can patch the privileged launcher,
  which is node root. Anyone who can create Secrets in the namespace (the
  built-in `edit` role) could do it. A second ValidatingAdmissionPolicy under
  the same `launcherSAGate` switch now rejects such Secrets unless the
  controller creates them. `docs/security-audit.md` no longer claims the
  escalation is closed. It lists the routes still open (TokenRequest,
  impersonation, and `pods/exec` into the launcher, all in `edit`/`admin`) and
  says to treat those roles as node-admin in launcher namespaces.

- **GPU passthrough could take a host NIC or disk away from the node.**
  `gpu-init` bound every non-bridge device in the GPU's IOMMU group to
  vfio-pci. On a board without ACS that group can also hold another card, such
  as the node's NIC or NVMe controller, which was then unbound from its host
  driver. The node lost its network or disk, and the device was handed to the
  guest. A peer is now bound only if it is another function of the same card,
  another GPU allocated to the guest, or already on vfio-pci. Any other device
  in the group makes `gpu-init` refuse before touching anything, naming the
  device.

- **Release signature checks accepted a signature from any branch.** The
  documented and CI `cosign verify` commands matched the signing workflow's
  identity with `…release-(stable|rc).yaml@.*`, so a signature produced by that
  workflow file on any ref, a branch included, verified as a release. Releases
  are only built from tag pushes, so the identity is now pinned to
  `@refs/tags/v.*` (anchored, with dots escaped) in the release and verify
  workflows and in `docs/releases.md`. CI's generated-code check now also
  covers `charts/kubeswift/crds`, the CRD copy operators actually install.

- **With `auth-mode=insecure`, any web page could drive the gateway.** There is
  no token in that mode, yet the Connect API answered every origin with
  `Access-Control-Allow-Origin: *`, mutating RPCs included. The WebSocket
  origin check also accepted any request whose Origin host matched its Host
  header, which a DNS-rebinding page satisfies. A page the operator visited
  could therefore delete VMs or open a console or sandbox shell. In insecure
  mode, browser requests to both the RPC and WebSocket surfaces are now
  accepted only from explicitly listed origins, or same-origin via localhost or
  an IP address (a port-forward). A same-origin DNS name must be listed in
  `gateway.corsAllowOrigin`. Authenticated modes are unchanged.

- **A vhost-user socket path could attach a host disk to the guest.** Cloud
  Hypervisor takes each device as one comma-separated `key=value` string, and
  swiftletd wrote the guest's vhost-user socket paths, virtiofs tags and
  generic-device `virtioId` into those strings unescaped. A socket such as
  `/srv/vm/x,path=/dev/sda` passes the host-path allowlist, yet made CH attach
  the node's `/dev/sda` to the guest. These values are now restricted to a safe
  character set by the SwiftGuest controller (the webhook is optional), the
  webhook and, as a last check before launch, swiftletd.

- **A pooled sandbox could run with the pool's network access and without
  its image verification.** Checking a SwiftSandbox out of a warm pool ignored
  the sandbox's own `network.mode`, `verifyKeySecretRef` and `image`. A
  `restricted` sandbox claiming a slot of an `open` pool got open egress, a
  sandbox that required a verified image ran the pool's image unverified, and
  slots warmed before a pool edit kept the old settings until claimed. Each
  warm slot now records the image, network mode and verification key it booted
  with. A sandbox claims only a slot that matches its own, and otherwise boots
  cold. The pool replaces warm slots booted under different settings, so
  existing warm slots are recycled once after upgrading.

- **The source node's migration private key was copied into the tenant
  namespace unnecessarily.** With live-migration mTLS enabled, Validating copied
  both participating nodes' identity Secrets — cert **and** private key — into
  the guest's namespace, but only the destination node's copy is ever mounted
  (by the destination pod's stunnel server). The source pod reads the per-guest
  Secret instead, so the source node's full identity in the tenant namespace was
  dead weight that widened exposure of a node-wide private key to anyone who can
  read Secrets there. Only the destination node's identity is copied now; the
  source node's identity is still required to exist (the per-guest copy fails if
  it is not provisioned). The destination node's identity copy is now reclaimed
  when the migration ends (at the terminal transition, and on mid-flight
  deletion), guarded so a copy another active migration in the namespace still
  needs is kept — previously these copies carried no owner and were never
  cleaned up, so a node private key sat in the tenant namespace indefinitely.

- **`swiftctl ssh` leaked the user's private key into logs.** The key was
  embedded in the pod-exec command, which the Kubernetes apiserver records in
  its audit log's request URI and which appears in `/proc/<pid>/cmdline` of the
  `sh` process for the whole session — readable by anyone with exec ("console")
  into the privileged launcher. The key is now streamed over the exec's stdin
  into a mode-0600 temp file (stdin content is not logged that way) and ssh runs
  `ssh -i <path>`, removing the file on exit via a trap. The guest's `primaryIP`
  (an unvalidated pod annotation) and the SSH user are now passed as quoted
  positional args instead of being spliced into the script, closing a shell
  injection through a hostile annotation.

- **A member kubeconfig could exfiltrate the gateway's own token or run code as
  the gateway.** A member `Cluster`'s credential Secret is supplied by whoever
  registers it, and the gateway loaded its kubeconfig with every field honoured,
  so one with `tokenFile: /var/run/secrets/.../token` and an attacker-controlled
  `server` made the gateway send its own ServiceAccount token to the attacker,
  and an `exec`/auth-provider plugin ran as the gateway process. The gateway now
  rejects a member kubeconfig that references gateway-local files or plugins
  (`tokenFile`, `exec`, auth-provider, client cert/key/CA file paths); inline
  credential data is unaffected.

- **OIDC mode did not require `email_verified`.** The default username claim is
  `email`, but the gateway never checked `email_verified`, so on an IdP that lets
  a user set or change their own email an attacker could claim a privileged
  operator's address and be impersonated as them on every federated member. The
  gateway now requires `email_verified=true` when the username claim is `email`,
  matching kube-apiserver's OIDC authenticator.

- **An unauthenticated request could OOM the gateway.** The Connect service
  handlers had no read-size cap, and Connect reads and decompresses a request
  message in full before the handler — and therefore before authentication —
  runs, so a single small gzip body could inflate to gigabytes and exhaust the
  gateway (chart memory limit 256Mi). Every Connect handler now caps the
  decompressed request at 4 MiB (`connect.WithReadMaxBytes`), and the raw
  WebSocket planes (`/console`, `/sandbox-exec`) cap a single inbound message at
  1 MiB (`SetReadLimit`), which gorilla otherwise leaves unbounded.

- **The controller no longer caches every Secret in the cluster.** The default
  cached client backs each typed read with an informer, so a single Secret read
  made controller-runtime watch and hold every Secret in the cluster in the
  controller's memory — seed data, migration mTLS keys, registry credentials and
  every unrelated tenant Secret — under the manager's 512Mi limit, an OOM risk
  on large clusters and a large exposure if the controller is compromised.
  Secrets are now read directly from the apiserver (no controller watches or
  owns them, so nothing relies on a cached Secret watch), which also removes
  read-after-write staleness for the seed and cert Secrets the controllers
  create and re-read.

- **Kernel artifacts collided across namespaces on a node.** The per-node
  kernel directory was `/var/lib/kubeswift/kernels/<namespace>-<name>`, and
  since both a namespace and a name can contain `-`, the join was ambiguous:
  namespace `team` + kernel `a-prod` and namespace `team-a` + kernel `prod`
  mapped to the same directory. One tenant's pull Job would then overwrite the
  other tenant's kernel and initramfs, which the victim's guests and sandboxes
  boot. The namespace and name are now separate path segments
  (`/var/lib/kubeswift/kernels/<namespace>/<name>`); neither can contain `/`, so
  the mapping is unambiguous. The path is derived, never stored, so existing
  kernels re-pull to the new layout on the next reconcile (a no-op if already
  present).

- **A virtio-fs sandbox could poison the node's shared rootfs cache.** The
  launcher container — which runs the untrusted guest — mounted the node rootfs
  cache (`/var/lib/kubeswift/sandbox-rootfs`) read-write. That cache is shared,
  keyed only by image digest, and reused as-is on a cache hit, and for a
  virtiofs sandbox virtiofsd shares whatever it can reach, so guest code that
  remounted the share or escaped its chroot to the lower layer could write into
  the cache and every later sandbox of that image on the node — in any namespace
  — would then boot the tampered rootfs, defeating cosign verify-before-boot.
  The launcher now mounts the cache read-only (the materialize init container
  keeps it read-write to populate it); block-mode rootfs was already opened
  `readonly=on` by Cloud Hypervisor. The read-only bind mount is authoritative —
  virtiofsd gets `EROFS` on any write regardless of its own flags.

- **A SwiftGuest annotation could mount an arbitrary node path into the
  privileged launcher.** Restore mode is selected by the
  `snapshot.kubeswift.io/active-restore` annotation, and
  `snapshot.kubeswift.io/restore-snapshot-path` was mounted into the privileged
  restore launcher as a hostPath verbatim. The controller host-path allowlist
  (`checkHostPaths`) validates `spec.filesystems[].source.hostPath` but never
  saw this annotation-sourced path, so a tenant who can patch their own
  SwiftGuest could set `active-restore` plus `restore-snapshot-path: /` and get
  the host root — or any node path — mounted into a privileged pod, i.e. node
  root. The restore snapshot path is now constrained to the snapshot base
  (`/var/lib/kubeswift/snapshots/`) plus one safe segment at the same controller
  chokepoint, matching the only values the SwiftRestore and cloneFromSnapshot
  controllers ever write (the snapshot's node-local dir). A guest carrying an
  out-of-bounds restore path now fails loudly instead of building the launcher.

- **A local-backend SwiftSnapshot could delete every namespace's snapshots on
  a node.** `spec.backend.local.hostPath` is mounted into a privileged Job and
  handed to `rm -rf` (cleanup) and swiftletd's `remove_dir_all` (capture), but
  the guard only checked the prefix and rejected `..`. The prefix itself
  (`/var/lib/kubeswift/snapshots/`) passed, so pointing a snapshot at the shared
  root and deleting it wiped every namespace's snapshots and s3/oci caches on
  the node; a segment like `*` or one carrying `;`/`$`/spaces passed too, and
  the cleanup Pod ran it through `sh -c` unquoted. The hostPath is now
  constrained to the prefix plus exactly one `[A-Za-z0-9._-]` segment (rejecting
  the shared root, globs, shell metacharacters and nested paths), enforced by
  the same validator in the webhook and — because `webhook.enabled` defaults to
  false — in the controller before any capture, and again before cleanup. The
  cleanup Pod no longer uses a shell: the path is passed as an argv operand to
  `rm`. The controller only ever generates `<ns>-<name>` names, so no legitimate
  snapshot is affected.

- **A SwiftImage import could reach data outside the image it named.** The
  import Job runs privileged (Linux images need a loop-mount to patch GRUB for
  the serial console), and it processed the tenant-supplied disk two ways that
  did not stay inside that disk. The GRUB patch mounted the image's partitions
  and rewrote `grub.cfg` with `sed`/`mv`; a symlink planted in the image (say
  `boot/grub/grub.cfg.tmp` → a host device, or `grub.cfg` itself → a host path)
  redirected that write out of the image, because the mount followed symlinks.
  And for a qcow2 source, `qemu-img convert` transparently follows a backing
  file or external data file named in the header, so an image referencing a
  host path or another tenant's file copied those bytes into the imported raw,
  where the booted guest could read them. Anyone able to create a SwiftImage in
  their own namespace could use either. The GRUB loop-mount now uses
  `nosymfollow,nodev,nosuid,noexec`, so the kernel refuses to follow any symlink
  on it (the patch of a real `grub.cfg` is unchanged; a redirected write fails
  closed and is skipped), and the qcow2 path now refuses any image whose header
  declares a backing or external data file before it converts. The launcher pod
  remains a node-level trust boundary by design; this closes two paths that let
  the *import* Job, not the launcher, act on data the operator never allow-listed.

### Fixed

- **QEMU vCPU pinning was skipped for the large GPU guests it exists for.**
  QEMU answers QMP only after hugepage preallocation and VFIO DMA mapping,
  which for a guest with hundreds of GiB of RAM takes minutes. swiftletd
  queried it once with a 5 s timeout, logged a warning and never retried, so
  the guest ran unpinned. Pins were also applied to the controller's chosen
  CPUs as given. Under the kubelet's static CPU Manager those are often
  outside the pod's cpuset, which the kernel rejects, and pinning stopped at
  the first rejection. Pinning now runs on its own thread, waiting up to 15
  minutes for QMP. Pins outside the pod's cpuset move to free allowed CPUs,
  on the same NUMA node when possible, and every pin is attempted.

- **Building a shared base could evict other bases for nothing, or overfill
  the pool.** Making room for a new base asked the pool for the image's full
  raw size, although only its non-zero blocks are written. A 10 GiB image
  holding 1.5 GiB of data demanded 10 GiB free and evicted cached bases to
  get it. The requirement is now the pool blocks the image file's data
  extents touch, an upper bound on what the write allocates. The free-space
  check was also not reserved. Two builds of different images could each see
  room, both write, and together fill the pool, which stalls and then fails
  every guest on the node. A build now reserves its space in the node
  registry, other builds don't count it as free, and it is released when the
  write finishes (or lapses after six hours if the build died).

- **The gateway probed slow member clusters about once a second, forever.**
  Every Cluster update re-probed the member, and every probe wrote
  `status.lastConnected`, which is itself an update. The loop stopped only
  when two probes finished within the same second, which never happened for
  a member more than about a second away. Each iteration cost roughly ten
  member API calls, a hub status write, and a WatchClusters event to every
  UI. Status-only updates no longer trigger a probe. Instead every member is
  re-probed every two minutes, which also keeps Ready/Reachable current for
  members whose loop used to end at once.

- **Rebuilding an unfinished shared base could leak its thin device.** A base
  whose population never finished is thrown away and rebuilt under the same
  id. Failures to unmap or delete the old device were ignored. If it was
  still busy, `create_thin` then found the id in the pool, and the
  "registry fell behind" recovery forgot the id. That left the old device,
  up to the image's full size, in the pool with nothing naming it. The
  rebuild now stops on such a failure and retries on the next attempt.

- **A checked-out sandbox's workload output could vanish from its logs.**
  swiftletd appended the output of a workload run in a claimed warm slot to
  the console log. Cloud Hypervisor writes that file at its own offset (it
  does not open it for append), so the next console line overwrote the
  appended output. The output now goes to `workload.log` in the run
  directory. `swiftctl sandbox logs` and the gateway print it after the
  console, and follow both files.

- **A successful live migration could be reported as failed on some Cloud
  Hypervisor builds.** swiftletd picks between CH v52's blocking
  send-migration and v53's non-blocking one from the CH version. It read
  git-describe or dirty builds (`v53.0-3-gabc1234`, `v53.0-dirty`) as
  unparseable and assumed v52. On v53 it then saw the guest still running
  right after the send was accepted and wrote `migration-status: failed`,
  while the migration completed in the background. The version's minor
  component is now read up to its first non-digit, and a failed probe is
  retried before the v52 assumption is used.

- **A guest could grow swiftletd's memory, or stall it, through the vsock
  agent reply.** swiftletd read the in-guest agent's reply (identity
  regeneration, warm-slot exec) until a newline, with no size limit and a
  timeout that restarted on every read. A guest that never ended its reply
  grew swiftletd's memory without bound, and one that sent a byte at a time
  held the action loop indefinitely. Replies are now capped at 16 MiB, and
  the timeout covers the whole reply.

- **A vhost-user queue size below 1 kept the launcher from starting.**
  `vhostUserDevices[].queueSizes` is signed in the API but read as unsigned by
  swiftletd, so a negative entry made the whole runtime intent unreadable and
  the launcher exited before it could report why. The CRD now requires each
  size to be at least 1, and the controller refuses such a guest (covering
  objects written before the schema change).

- **Long SwiftSnapshot, SwiftRestore, SwiftGuest or SwiftImage names broke
  their Jobs.** Derived Job names (`<snapshot>-s3-upload`, `-oci-push`,
  `-oci-disk-<disk>`, `<restore>-oci-download`, `swiftguest-rootclone-<guest>`,
  `<guest>-datafill-<disk>` and others) and name-bearing labels could exceed
  the 63-character limit Kubernetes puts on Job names and label values, so
  every Job create failed. A snapshot then failed later with a misleading
  "capture deadline exceeded", and a restore sat Pending. Names over the
  limit are now shortened with a hash of the full name, which keeps them
  deterministic and distinct. Names that already fit are unchanged.

- **An OCI snapshot could go Ready without the digest every restore needs.**
  The manifest digest of a pushed memory or disk artifact was read,
  best-effort, from the push pod's termination message. When that wasn't
  readable (the pod's status trailing the Job's in the cache, or the pod
  gone), the snapshot still went Ready with an empty digest, and every restore
  and clone of it was then refused. The controller now waits for the report
  and fails the snapshot, naming why, if it is still missing two minutes
  after the push completed. The report is also taken only from a Succeeded
  pod the Job controls. Any pod labelled `job-name: <job>` used to be able to
  supply it, and with it the digest a restore would pull.

- **A failed root-disk clone was never reported.** Every error from
  preparing a disk-boot guest's root disk, including a clone or download Job
  that had failed for good, was dropped on requeue. The guest sat in
  Scheduling with nothing naming the cause. `StorageReady` is now False with
  reason `RootDiskCloning` while the disk is being prepared, or
  `RootDiskCloneFailed` with the Job's failure message when retrying won't
  help. A storage pre-flight failure already on the condition takes
  precedence.

- **The SwiftGuest controller sent a status patch on every reconcile.** Its
  "nothing changed" check compared the stored status with a pointer, which
  never matched. With the optimistic lock added in this release, a stale
  cached read turned that no-op patch into a conflict and an extra reconcile.
  An unchanged status is no longer patched.

- **A stopped guest kept its last run's pid, console socket and interface
  addresses.** Clearing the run state when a launcher goes away dropped the
  conditions and the primary IP but left `status.runtime.pid`,
  `status.console`, `status.network.interfaces`, `status.network.ready` and
  `status.network.egress`, so `swiftctl describe` showed a process, a serial
  socket and network state that no longer existed. They are now cleared as
  well. `status.runtime.hypervisor` is kept,
  since it describes the guest rather than the run.

- **A stopping guest could briefly get a new launcher.** If its launcher
  finished terminating between the controller's stop check and its launcher
  lookup in the same pass, the controller created a fresh launcher, which
  exited at once. It now waits for the next pass, which records the guest
  Stopped.

- **A sandbox pool could delete a slot a checkout had just claimed.**
  Scale-down, stale-slot recycling and pool deletion delete warm slots from a
  list read earlier. A checkout that claimed one of those slots in between
  lost it, and the sandbox failed with `SlotLost`. The pool now deletes a warm
  slot only if it is unchanged since it was read.

- **Deleting a SwiftGuestPool deleted every replica's data disk.** PVCs from
  `volumeClaimTemplates` had the pool as their controller owner, so garbage
  collection removed them with the pool. The pool guide says they survive
  pool deletion and are cleaned up by hand. They now carry no owner
  reference, and existing PVCs have the pool's reference removed on the next
  reconcile. A replica also adopted any existing PVC with its name, and names
  can collide across pools (template `data-web` in pool `x`, template `data`
  in pool `web-x`), so two pools' replicas could share one disk. A PVC is now
  reused only if its `swift.kubeswift.io/pool` label names the pool. The docs
  also had the PVC name order backwards: it is
  `<template-name>-<pool-name>-<index>`.

- **A live migration could boot a second copy of the VM, or hang in
  Resuming.** After a successful send, the source Cloud Hypervisor exits and,
  with plaintext transport, the source launcher pod exits 0. Until cutover
  moved the guest to the destination pod, the SwiftGuest controller read that
  exit as a guest shutdown. With `runPolicy: Always` it deleted the launcher
  and started a new one, a second VM on the same disk as the migrated one.
  With any run policy it marked the guest Stopped and cleared the
  `GuestRunning=True` that the destination had written once, so the
  migration waited in Resuming until `spec.timeout`. A launcher that has
  reported `migration-status: complete` is now left alone. Separately,
  deleting a live SwiftMigration after the source reported complete kept the
  destination pod but never cut over to it. The VM ran in a pod nothing
  tracked while the guest pointed at the exited source. Such a deletion now
  finishes the cutover before the object goes.

- **The one-live-migration-per-source-node admission check ignored `mode:
  auto` peers.** It compared `spec.mode` only, so a migration created as `auto`
  (the default for `swiftctl` and node drain) that had gone live was never
  counted, and an explicit live migration from the same node was admitted
  alongside it. Peers are now compared by the mode they resolved to. An `auto`
  migration is still not checked when it is admitted, since its mode is not
  known yet.

- **A native GPU could be allocated on a node its workload could never run
  on.** The allocator took the first SwiftGPUNode with enough free GPUs. It did
  not check `vfioReady` (which the API docs said it did) or the discovery
  phase, whether the Kubernetes Node was cordoned or gone, a SwiftGuest's
  `spec.nodeName`, or a sandbox's `nodeSelector` and kernel-node requirement.
  The launcher is pinned to the GPU's node, so it sat Pending (or failed
  gpu-init) holding GPUs another node could have supplied. New allocations now
  skip such nodes. An allocation a workload already holds is left where it is.

- **GPU discovery could erase an allocation and let a GPU be handed out
  twice.** Each discovery cycle read the SwiftGPUNode, merged in the hardware
  it found, and patched status without a `resourceVersion`. `status.gpus` is an
  atomic list, so when the list changed (a driver rebind, for instance), the
  patch resent all of it from the earlier read. An allocation the controller
  made in between was reverted to free. Discovery's status write is now
  optimistically locked and, on a conflict, re-reads and re-merges.

- **The legacy seed-ConfigMap cleanup could delete a ConfigMap KubeSwift did
  not create.** Retiring the pre-v0.12 plaintext seed ConfigMap deleted any
  ConfigMap named `<guest>-seed` that no pod was mounting, including one the
  user or another tool owned. Only the ConfigMap the guest controls is removed
  now.

- **A refused sandbox exec hung forever in the gateway and in `swiftctl`.** When
  the exec into the launcher was refused (no `pods/exec` permission, pod not
  running), the stream ended without reading stdin, and the vsock handshake
  write into the stdin pipe blocked forever. Every refused console/exec
  attempt leaked a gateway handler, its goroutines and the client connection,
  and `swiftctl sandbox exec` hung instead of reporting the refusal. The pipes
  are now closed with the stream's error when it ends, and the refusal is
  returned to the caller.

- **Deleting an s3 or oci snapshot left the guest's RAM on the capture node,
  and oci artifacts were never deleted.** An s3/oci capture writes the full
  memory image to a node-local directory before uploading it, and nothing
  removed that directory: every such snapshot's RAM, secrets included, stayed
  on the node's disk after the snapshot was deleted. oci snapshots also had no
  cleanup at all, so `deletionPolicy: Delete` left every pushed artifact in the
  registry. Deleting an s3/oci snapshot now removes the capture-node copy
  (under either policy), and deleting an oci snapshot with `Delete` removes
  its memory, disk and data-disk artifacts from the registry. A registry that
  refuses deletes leaves the artifact in place rather than blocking the
  deletion. Download caches written on other nodes by restores and clones are
  still not tracked.

- **With scoped launcher RBAC, a live-migrated guest lost its API access when
  its migration was deleted.** After a live migration the guest runs in the
  renamed destination pod, whose per-pod grant was owned by the
  SwiftMigration. Deleting the migration (a drain migration's 1h TTL does)
  garbage-collected the grant, and the running launcher could no longer report
  status, its IP or action results. The SwiftGuest controller now takes that
  grant over onto the guest.

- **A snapshot schedule burst out stale snapshots after an outage.** The
  catch-up walked forward from the last fire and stopped after 100 ticks,
  firing that tick rather than the latest one. The snapshot it created
  re-triggered the reconcile, which fired the next stale tick, and so on: a
  frequent schedule produced a snapshot per reconcile, each named for a
  long-past time, until it caught up. It now fires only the most recent missed
  tick, found by searching back from the current time.

- **A crash while creating a thin-pool backing file blocked the node's pool
  until someone deleted the file by hand.** The file was created at its final
  path and then preallocated, so a crash in between left a short file there,
  which every later attempt refused as smaller than the configured size. It
  is now built beside the final path and renamed into place only once fully
  allocated and synced. Creation and loop-device attachment also run under a
  per-file lock, so two creators on a node cannot each attach their own file.

- **A memory snapshot could fail right after pausing the guest.** The capture
  deadline (600s by default) ran from the snapshot's creation, so time spent
  Pending (waiting for the guest to come up) counted against it. A snapshot
  that had waited that long failed on its first Capturing poll, after the
  capture had been sent and the guest paused. For a full-state (`includeDisk`)
  capture, which leaves the guest paused for the disk export, the guest was
  then left paused with nothing to export or terminate it. The deadline now
  runs from the new `status.captureStartedAt`, and a full-state capture that
  does exceed it queues a resume so the guest is not left paused.

- **A guest with SR-IOV NICs on two different resources lost the second NIC.**
  swiftletd took each VF's PCI address from its resource's `PCIDEVICE_*` list
  using one counter shared across all resources, so the first NIC on a second
  resource asked for index 1 of a one-entry list, found nothing, and was
  dropped with only a log line. Addresses are now counted per resource.

- **A GPU sandbox released its GPU while still using it, and never released it
  when it finished.** Deleting a native-GPU SwiftSandbox freed its GPU at once,
  but its launcher pod is garbage-collected only after the sandbox is gone, so
  the running Cloud Hypervisor still held the VFIO group and the next
  consumer's bind failed with "Resource busy" (the race already fixed for
  SwiftGuests). A Completed or Failed sandbox, meanwhile, kept its GPU reserved
  until it was deleted, which is never without a TTL. Deletion now deletes the
  launcher and releases the GPU once it is gone, and a finished sandbox
  returns its GPU as soon as its launcher has exited.

- **An offline migration could hang forever and block every later migration
  and drain of its guest.** `spec.timeout` was enforced only for live
  migrations. An offline migration stuck in Preparing (a volume that never
  detached) or Resuming (a target that never boots) kept the guest's
  migration-in-progress marker indefinitely. Offline migrations now fail at
  `spec.timeout`: before the cutover the guest is restarted where it was, and
  after it the guest stays on the target; the marker is released either way.
  `timeoutStrategy: ignore`, which was accepted but never read, now disables
  the timeout (live and offline).

- **A powered-off guest blocked node drains.** The eviction webhook denied the
  eviction of every SwiftGuest launcher and marked the guest for migration,
  including a launcher that had already exited (a stopped or failed guest).
  The drain then waited on a migration that either timed out or powered the
  stopped guest on at the target. An exited launcher is now evicted normally
  (it runs no VM), and the drain controller clears a drain marker on a
  `Stopped`/`Failed` guest instead of migrating it.

- **Deleting a guest or snapshot could hang in `Terminating` when the webhook
  was enabled.** The validating webhooks re-checked the whole spec on every
  update, including the controllers' finalizer removals. After the rules
  tightened (a narrower hostPath allowlist, an upgrade), or the source guest
  changed (a GPU added after a memory snapshot was taken), a SwiftGuest's or
  SwiftSnapshot's finalizer could no longer be removed, and it stayed
  `Terminating` along with its namespace. Updates to an object being deleted,
  and updates that leave its spec unchanged, are no longer re-validated
  (SwiftGuest, SwiftSnapshot, SwiftRestore, SwiftMigration). Spec changes are
  still validated, and immutable specs remain immutable.

- **swiftletd's `GuestRunning` report wiped the guest's other conditions.**
  `status.conditions` is an atomic list, so swiftletd's merge patch of just
  `[GuestRunning]` replaced the whole list, dropping `GPUAllocated`,
  `StorageReady` and the rest, and a running GPU guest then read as Pending.
  In the other direction, the SwiftGuest and GPU controllers' status patches
  resent the full list from a possibly stale read and could put back a
  `GuestRunning` that swiftletd had just changed. swiftletd now reads the
  conditions, updates only `GuestRunning` (keeping its transition time unless
  the status changes), and writes them back with the `resourceVersion` it read,
  retrying on conflict. Both controllers' status patches are optimistically
  locked, and a conflict is retried promptly from a fresh read.

- **A kube-ovn guest got a new IP after a stop/start.** The kube-ovn IP pin was
  taken from `status.network.primaryIP`, which is now correctly cleared when
  the launcher goes away (stop, poweroff, offline migration). Every restarted
  guest was therefore unpinned and handed a fresh address, breaking the
  documented stable static IP. The IP kube-ovn assigns is now recorded on the
  guest as `swift.kubeswift.io/kube-ovn-ip` and pinned from there. Remove the
  annotation to release the pin.

- **A running guest that never reported an IP had no drain protection.** While
  waiting for the guest's IP, the controller returned early every 5 seconds,
  skipping the per-guest Service and the PodDisruptionBudget that keeps a drain
  from evicting the VM. A guest that never reports one (static address, SR-IOV,
  DHCP timeout) therefore never got either, and was polled every 5 seconds
  forever. It now gets both, and the IP is looked for again every 30 seconds
  (the pod watch delivers it sooner).

- **Every guest's status was rewritten on every reconcile.** The SwiftGuest
  controller writes status only when it changed, but its condition helper
  restamped `lastTransitionTime` on every call, so the status always differed:
  each 30s resync of each guest was an apiserver write (and a watch event to
  every client), and the timestamps no longer said when anything happened.
  `lastTransitionTime` now moves only when the condition's status changes.

- **An in-place memory restore could resume old RAM over a newer disk.** A
  local/s3/oci memory snapshot captures memory and device state, not the disk,
  and the in-place restore reopens the guest's live disk. When the guest kept
  running after the capture (`resumeAfterSnapshot: true`, the default) or was
  relaunched from its disk since, the restored kernel's page cache and
  filesystem state were older than the disk underneath, and writing them back
  silently corrupts the filesystem. The documented disaster-recovery walkthrough
  did exactly this. The in-place restore now refuses such a guest with reason
  `DiskDiverged` unless the SwiftRestore carries the annotation
  `snapshot.kubeswift.io/accept-disk-divergence: "true"`. A full-state OCI
  capture (`includeDisk`) is unaffected. The samples, the round-trip e2e test
  and the walkthrough now capture with `resumeAfterSnapshot: false` and let the
  restore replace the paused launcher, rather than killing it (which boots the
  guest from its disk) first.

- **`overwriteExisting: true` restored nothing and reported Ready.** Over an
  existing guest, the csi-volume-snapshot restore found the root-disk PVC and
  the guest already present and skipped both. A memory clone restore returned
  the existing guest unchanged and "resumed" it. Either way the restore went
  `Ready` ("restore complete") with nothing restored. Only the in-place memory
  restore can replace an existing guest's state; any other restore onto an
  existing guest now fails with reason `OverwriteUnsupported`.

- **A failed in-place restore left the guest unable to boot normally.** The
  restore annotations route every launcher the guest gets to the snapshot, and
  they were only removed on success. After a failure, every relaunch retried the
  failed restore, and the guest never booted from its disk again until someone
  removed the annotations by hand. They are now removed when the restore fails.

- **`resumeAfterRestore: false` hung memory clone restores and was ignored by
  in-place ones.** A clone target was created `Stopped`, so it had no launcher
  and the restore waited for one forever. The in-place path resumed the VM
  anyway. Both now bring the launcher up with the snapshot loaded, go `Ready`,
  and leave the VM paused.

- **SwiftGuestPool rolling updates could take the whole pool down, or never
  finish.** Availability was counted from the replicas that existed rather than
  the ones serving, so a replacement created in the same pass (still booting)
  counted as available: with 2 replicas and `maxUnavailable: 1` the second
  replica was deleted while the first one's replacement was still starting, and
  both were down at once. `maxSurge` only filled missing indices below the
  desired count, of which a rollout has none, so the documented zero-downtime
  setting `maxUnavailable: 0, maxSurge: 1` never replaced anything. And rolling
  back a rollout whose new replicas never became ready deadlocked: the broken
  replicas were the unavailable ones, so the budget was spent and none could be
  replaced. A replica now counts as available only while it is `Running` with
  `GuestRunning=True` and not terminating. Outdated replicas that are not
  serving are replaced first, outside the budget. `maxSurge` brings up
  current-template replicas above the desired count (the next indices), which
  are removed once the rollout is done and every replica is serving. The CRD now
  rejects `maxUnavailable` and `maxSurge` both 0, which could never make
  progress. An existing pool set that way reports a `RolloutBlocked` event
  instead of stalling silently. The docs no longer claim percentage values,
  which the integer fields never accepted.

- **A running guest could be stuck reporting `GuestRunning=False`.** The
  controller cleared a guest's run state whenever its launcher pod was
  `Pending`, on the premise that a Pending pod has started nothing. But a pod
  stays Pending while *any* container is still waiting, so a launcher already
  running next to a sidecar that is still starting (the migration mTLS stunnel
  server) read the same. swiftletd reports `GuestRunning=True` only once, so the
  clear stuck: the guest read not-running with no address, and migration
  Resuming, restores and pool rollouts waited on it until they timed out. The
  run state is now cleared only when the launcher container itself is not
  running.

- **A warm GPU pool could free GPUs that were still in use.** The pool's slot
  GPU cleanup matched allocations by the bare `<pool>-slot-` name prefix, so it
  also freed the GPU of a standalone SwiftSandbox named like a slot and of every
  slot of a pool whose own name began with `<pool>-slot-`. Deleting the pool
  released every slot's GPU outright, including those of claimed slots whose
  checkouts were still running. The set of live slot pods also came from the
  informer cache, where a slot created moments earlier might not appear yet. In
  each case the device was handed to the next consumer while a VM still had it.
  The cleanup now matches only this pool's exact slot-name shape, never frees a
  GPU a SwiftSandbox by that name still owns, and reads the live pods uncached.
  Pool deletion removes idle warm slots and then waits, holding its finalizer,
  until the pods of claimed slots are gone before releasing their GPUs.

- **Editing a guest's GPU request could leak the GPU and wedge the guest in
  `Terminating`.** `gpuProfileRef` and `gpuResourceClaim` are mutable, but the
  GPU controller chose what to do from the current spec. Removing the ref after
  allocation made it return early: on delete the finalizer was never removed, so
  the guest stayed `Terminating` forever and its GPU stayed allocated to it.
  Switching from the native backend to DRA ran DRA's no-op release and leaked the
  native GPUs the same way. Release now follows the allocation recorded on the
  SwiftGPUNodes rather than the spec: deletion frees everything the guest holds,
  and a guest that no longer requests native GPUs gets them returned once its
  launcher has let go of the VFIO group, with its stale GPU status cleared. The
  native release also no longer skips a guest whose `status.gpu` is missing,
  which leaked the reservation when the status write after allocation failed.

- **A failed sandbox workload could be reported `Completed` with exit code 0.**
  swiftletd recovers the workload's exit code from the console log, but two bugs
  lost it and let the SwiftSandbox controller fall back to the launcher's own
  exit code (0). The recovery was gated on the block-rootfs path, which a
  `rootfsMode: virtiofs` sandbox does not have, so it never ran for them; and the
  log was read as strict UTF-8, so any non-UTF-8 byte the workload printed failed
  the read. The recovery now runs for every sandbox and reads the log's last
  64 KiB lossily (bounded, so a workload that logged gigabytes no longer makes
  swiftletd load all of it); the last exit-code marker still wins, so a workload
  cannot spoof the real one.

- **A shared-base guest could be left permanently unable to boot.** The node
  recorded a guest's thin device id before creating its snapshot, so a snapshot
  that failed (for example, its base evicted by another build at the same
  moment) or a materialise Job killed between the two left the guest recorded
  against a device that did not exist. Every later attempt took the reactivate
  path and failed with "thin device N does not exist in pool" until the guest was
  deleted. Guests now have the same two-phase record bases already had: a guest is
  marked created only after its snapshot exists. An allocated-but-never-created
  guest never received a disk, so it is created afresh (with a new id — never by
  activating the old one, which could belong to another guest if the registry is
  behind the pool); a created guest whose device is gone still fails loudly, as
  before. Registries written by earlier versions are read with every guest
  treated as created.

- **Deleting an S3 snapshot could delete other snapshots' data.** The delete
  Job listed objects by the snapshot's key prefix with no trailing `/`, and an S3
  prefix list is a plain string match, so deleting snapshot `db` also removed
  every object of `db-1700000000`, `db2` and any other snapshot whose name starts
  with `db` in the same bucket path. A scheduled snapshot's keep-N pruning of its
  oldest snapshot therefore wiped its newer siblings, which stayed `Ready` but
  could no longer be restored. The delete is now scoped to `<prefix>/<ns>/<name>/`.

- **Stopping, deleting or draining a guest killed its VM instead of shutting it
  down.** swiftletd runs as PID 1 in the launcher container and installed no
  SIGTERM handler, and the kernel drops a signal sent to a PID-namespace init
  with no handler. So every launcher-pod deletion — `runPolicy: Stopped`, guest
  delete, node drain, and the source teardown of an offline migration — sent a
  SIGTERM that was silently ignored, and the kubelet SIGKILLed Cloud
  Hypervisor/QEMU at the end of the grace period. The guest never received an
  ACPI power-off and lost its dirty page cache, leaving filesystems needing
  journal replay or damaged (worst for Windows/NTFS), including the disk an
  offline migration then booted on the target. swiftletd now handles SIGTERM by
  pressing the guest's ACPI power button (Cloud Hypervisor `vm.power-button`,
  QEMU `system_powerdown`); the guest shuts down cleanly within the pod's grace
  period and swiftletd reports `VmStopped`. A guest that ignores ACPI is killed
  at the end of the grace period as before.

- **Draining a node could hang on a guest with ordinary storage.** Every drain
  migration uses `mode: auto`, and auto resolution never checked storage, so a
  default disk-boot guest (ReadWriteOnce/Filesystem) resolved to live; its
  destination pod hit Multi-Attach, the migration failed `DstNeverReady`, and
  the drain stayed blocked instead of falling back to offline as documented. Its
  comment assumed Validating-live would reject incapable storage, but no such
  check existed in the controller — only in the webhook, which is off by
  default and only applied to explicit `mode: live`. The storage rule
  (kernel-boot, or ReadWriteMany+Block root storage) now lives in one shared
  helper used by the webhook, by auto resolution (incapable storage resolves
  offline), and by Validating-live (explicit `mode: live` on incapable storage
  fails with `EligibilityMismatch`, the reason defined for exactly this).

- **The gateway UI froze on a member cluster after about an hour, with no
  error.** A multi-cluster guest or migration stream ran one watch per member,
  and when the apiserver ended a member's watch — its routine watch timeout, a
  dropped connection, or a 410 once the resume point was compacted — that
  member's watch just returned while the stream stayed open on the others. The
  UI kept showing the member's last state indefinitely. Member watches now
  re-establish themselves: a routine close resumes from the last resourceVersion
  seen (with bookmarks), so nothing in the gap is lost; an expired
  resourceVersion restarts from current state and reports a per-cluster error
  (deletions inside the gap cannot be replayed); a transient start failure is
  reported and retried with capped backoff.

- **A live-migration cancel or timeout could destroy the only running copy of
  the VM.** The controller treated cutover step 1 (the `PodRefSwapped`
  condition) as the point of no return, but the real commit point is earlier:
  when the source launcher reports `migration-status=complete`, its Cloud
  Hypervisor has already exited and the destination holds the only running copy.
  In the window between those two events a `spec.cancelRequested`, a
  `spec.timeout` expiry, or a timeout landing between cutover step 1 and step 2
  would fail the migration and delete the destination pod — losing the guest.
  The commit point is now defined as "source reported complete" (in one helper)
  and honoured by both the cancel handler (a cancel past it is ignored and the
  migration completes) and the StopAndCopy timeout (not enforced once the source
  has completed or cutover has begun; the migration only moves forward). A cancel
  or timeout *before* the commit point still aborts as before.

- **Deleting a live SwiftMigration mid-transfer could orphan the destination or
  split-brain the guest.** The deletion (finalizer) handler decided pre- vs
  post-cutover by phase, treating all of StopAndCopy as post-cutover: it left
  the destination pod — which the SwiftGuest owns, so it is not garbage-collected
  with the migration — receiving into an orphan that nothing would cut over to,
  and with `runPolicy: Always` the SwiftGuest controller then booted a second
  copy from the same disk. Deletion now uses the same commit point: before it
  (source still running) the deletion is an abort that restores the source and
  deletes the destination pod; after it the destination is preserved as the
  running guest. Offline deletion is unchanged (its commit point is the
  `spec.nodeName` patch).

- **Live migrations hung in `Resuming` until their timeout** (#646,
  regression from #641 in v0.14.0). The cutover pointed the guest's
  `status.podRef` at the destination launcher by name but kept the source
  launcher's UID. Since #641 the guest controller reads a launcher whose UID
  differs from `podRef.uid` as a new run and clears its run state, and the
  destination launcher had already reported `GuestRunning=True` — once, before
  the cutover. The migration then waited for a condition nothing would write
  again until `spec.timeout` (30m by default) failed it; with one in-flight live
  migration allowed per source node, nothing else could live-migrate or drain
  off that node meanwhile. The VM itself had moved and kept running. The
  cutover now sets the UID as well: a live migration carries the same run.
  Offline migration was not affected, nor were guests on a primary UDN, whose
  `GuestRunning` the controller derives itself. A migration already hung when the
  controller is upgraded stays hung until its timeout, and its guest reports
  `GuestRunning=False` while running until its launcher next restarts.

- **A guest whose launcher had not started still reported itself running**
  (#643, follow-up to #634). v0.14.0 clears a guest's run state when its
  launcher changes, which covers a restart but not the state a PREVIOUS
  controller left behind: after an upgrade `status.podRef` can already name the
  current pod, so nothing re-evaluates it. Found while upgrading the fleet to
  v0.14.0, on a guest stuck Pending against a volume that would not attach: it
  reported `GuestRunning=True` with an address, because the launcher that
  reported them was two pods ago. A Pending pod has started no containers, so no
  VM is running behind it whatever the last launcher said. Clearing there
  catches the case and heals a guest upgraded into it. `ClearRunState` is now
  idempotent as well: a condition already False for the same reason is left
  alone, so a guest sitting Pending is not rewritten, and does not claim a
  transition, on every reconcile.

---

## [v0.14.0] — 2026-09-23

Guests of one image can now share a copy-on-write base disk on their node
instead of each copying it. A second guest's first boot costs **25 MiB** of pool
space against **1.75 GiB** for a copy, and its disk is ready in 15 s instead of
55 s. Opt-in per SwiftGuestClass, on nodes that opt in too, because the disk is
node-local and cannot move.

The rest is operational correctness: a deleted GPU guest no longer frees its
device while its VM still holds it, a stopped guest no longer reports itself
running with an address, a pool that cannot resolve its image stops hammering
the registry, and creating a guest no longer runs `apt-get` on the node.

**CRDs changed this release** — `swiftguests`, `swiftguestclasses` and
`swiftimages`. Apply them before upgrading:

```bash
kubectl apply -f charts/kubeswift/crds/
```

### Upgrade

```bash
kubectl apply -f charts/kubeswift/crds/
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.14.0 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
```

Nothing changes for existing guests: shared-base disks are opt-in and no class
has them until you say so.

**Shared-base disks need nodes that accept them.** A pool is a large,
preallocated file of a node's own disk, so no node takes one unless it is
labelled:

```bash
kubectl label node <node> kubeswift.io/basedisk-node=true
```

A guest of a `sharedBaseDisk: true` class waits, saying so on `StorageReady`,
until a node is labelled. Size the pool with the new
`swiftGuest.sharedBaseDisk.poolSize` (default `40Gi`); it sizes a pool when it is
created, and a node refuses to create one that would leave its filesystem under
the kubelet's 10% eviction threshold.

**`SwiftImage.spec.source.upload` is gone.** It was an empty placeholder the
webhook accepted and the import path could never honour, so an image using it
sat in `Pending` forever with the explanation only in its conditions. Applying
the new CRD prunes the field. Nothing that worked stops working — nothing using
it ever worked.

**A stopped guest now reports that it is stopped.** `GuestRunning`,
`NetworkReady`, `EgressReady`, `PortsProgrammed`, `PodScheduled` and
`status.network.primaryIP` describe one launcher, and are cleared when the guest
stops, when its launcher exits, and when a new launcher starts. Two consequences
worth knowing: a `kubectl wait` on those conditions now waits for the run in
front of it rather than returning on the last one, and a rolling
`SwiftGuestPool` update counts a restarting replica unavailable until its VM is
actually up.

`ui.image.tag` stays at `v0.12.4` — kubeswift-ui cut no release this cycle.

### Added

- **Shared-base root disks** (#614, #600). `SwiftGuestClass.spec.sharedBaseDisk:
  true` gives guests of that class a root disk that is a copy-on-write snapshot
  of one node-local base per image, built by a per-guest Job on the node and
  mapped from a device-mapper thin pool. Guests pay for what they write. The
  disk is node-local, so the guest is pinned to the node holding it, and
  migration in every mode, CSI snapshots and full-state (`includeDisk`)
  snapshots are refused with the reason on the object — memory snapshots still
  work. Deleting a guest frees its disk through a release Job on its node,
  including when its whole namespace is deleted. Bases are a cache: kept while
  there is room, evicted least recently used first when a new one needs the
  space, which is also what reclaims a base whose image has been deleted. See
  [docs/shared-base-disks.md](docs/shared-base-disks.md).
- **`swiftGuest.sharedBaseDisk.poolSize`** (default `40Gi`) sizes a node's thin
  pool at creation. Validated at controller startup, so a value that does not
  parse fails the rollout instead of silently becoming the default.
- **The `faas` kernel is published** (#598). `kernels/faas:6.6.3` exists in the
  registry now; the docs previously pointed at a tag that was never published,
  in three files that disagreed about which one it was.

### Fixed

- **A deleted GPU guest freed its device while its VM still held it** (#602,
  #604). The allocation was released the moment the object had a
  `DeletionTimestamp`, but a terminating launcher's Cloud Hypervisor still holds
  the VFIO group, so `SwiftGPUNode` advertised a device that was busy and the
  next consumer failed to boot with `failed to open /dev/vfio/<group> group:
  Resource busy`. A GPU `SwiftSandboxPool` with `minWarm: 1` creates that next
  consumer by itself, so this needed no unusual timing. The allocation is now
  held until no pod carries the guest's launcher label.
- **A stopped guest reported itself running, with an address** (#634). See
  Upgrade above. The same stale values are what the migration controller already
  had to work around, gating completion on the destination pod's own state
  because the condition and IP survived the cutover pod swap.
- **A sandbox pool that could not resolve its image hammered the registry**
  (#603, #605). Every reconcile issued a manifest GET — which a registry counts
  as a pull — at the 10 s poll interval: ~360 an hour against Docker Hub's
  anonymous allowance of 100, forever, recoverable only by removing the demand
  by hand. Resolve failures now back off per pool, 10 s doubling to 10 m,
  cleared on the first success.
- **Creating a guest ran `apt-get` on the node** (#607, #616). The root-disk
  clone Job, the data-disk fill Job and `clone-grow-init` all ran `ubuntu:22.04`
  and installed `qemu-utils` and `gdisk` before doing any work — a network
  round-trip and a reachable apt mirror in the path of creating a guest, and an
  outright failure on nodes with neither. They now run the launcher image, which
  is on every node that runs guests by definition and ships both tools.
- **Golden-image import wrote zeros as allocated blocks** (#608). Only all-zero
  windows are skipped when a golden image is pushed, so a stored window holding
  a few MiB of data was mostly zeros and the import materialised them: 10.75 GiB
  written for 5.69 GiB of real data on a 30 GiB image, and every per-guest clone
  then copied all of it. Zero blocks inside a stored window are now left as
  holes; block devices are still written densely.

### Changed

- **`SwiftImage.spec.source.upload` removed** (#623). See Upgrade.

### Security

- **rustls advisory in the launcher's TLS stack** (#601). RUSTSEC-2026-0285
  (TLS 1.3 handshake messages accepted across encryption level boundaries)
  reached swiftletd through `kube-client`. `cargo update` alone lands on
  0.23.43, one short of the fix; pinned to 0.23.45.
- **grpc 1.84.0 is not proposed any more** (#615). `govulncheck` finds
  GO-2026-6443 reachable from `cmd/kubeswift-dra-driver`, and the fix exists
  only in a v1.85.0 pre-release, so Dependabot re-proposed a version that failed
  the gate every week — the kind of weekly red that teaches people to click past
  it.
- **A flaky `gosec` install stopped marking the tool broken** (#599). When
  `go install` could not reach `sum.golang.org`, the SARIF upload ran anyway on
  a file that was never written, registering a configuration error against gosec
  in the Security tab that outlived the run.

### Docs

- **Shared-base root disks** — what they cost a node, which nodes may hold a
  pool, what they refuse and why, what happens when a guest is deleted, and how
  to read the state on a node.
- **`role: edge` no longer says edge onboarding has not shipped** (#622).
  `values.yaml` said it lands in a follow-up PR while the block twelve lines
  below documented the feature the chart already implements.

### CI

- **The chart README's values table is verified against `values.yaml`** (#624).
  v0.13.14 shipped a stale `ui.image.tag` default in that table, caught by
  reading ~60 rows by hand during a release sweep — exactly the step that gets
  skipped on the release where it matters.

---

## [v0.13.15] — 2026-09-14

A bug-fix and hardening release. Three of these turned an ordinary operational
action into a false failure: draining a node marked the VMs running on it
Failed, a pool then deleted and recreated those replicas in a tight loop, and a
rejected host path was never reported on the guest at all. Also clears two HIGH
CVEs carried in the Debian-based images, and adds a schema that rejects values
keys the chart does not read.

**No CRD changed this release.** `kubectl apply -f charts/kubeswift/crds/` is
harmless but not required — unlike v0.13.14, which added a print column.

### Upgrade

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.15 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
```

**`helm upgrade` can now FAIL where it previously succeeded.** The chart ships a
`values.schema.json` that rejects keys it does not read, so a values file
carrying a misspelt key is refused instead of silently ignoring it. If the
upgrade errors with a schema message naming a key, remove that key: it was never
doing anything. This is the intended behaviour — a mistyped `snapshotOras` (the
real key is `snapshotORAS`) is exactly how an image pin can sit four releases
stale while the values file looks current.

No values keys were added or removed, and `ui.image.tag` stays at `v0.12.4`.

### Security

- **Two HIGH CVEs in the six Debian-based images** (#594) — `libpcre2-8-0`
  CVE-2026-86145 (out-of-bounds write) and CVE-2026-89161 (memory corruption),
  fixed in 10.42-1+deb12u1. `debian:bookworm-slim` still ships the vulnerable
  build and `apt-get install` never upgrades what the base already carries, so
  rebuilding alone would not have cleared it — the images now upgrade base
  packages at build. The distroless and Alpine images were never affected.

### Fixed

- **A cordon marked running guests Failed, and pools deleted them** (#592). A
  guest pinning `spec.nodeName` was checked against its node's taints on every
  reconcile, before anything looked at its launcher, and any hit was terminal:
  `Phase=Failed`, no requeue. So `kubectl cordon` — which adds
  `node.kubernetes.io/unschedulable:NoSchedule` — failed every running guest
  pinned to that node while its VM kept running, froze the guest's status, and
  counted a VM failure. A SwiftGuestPool deletes Failed guests to replace them,
  so a pooled pinned VM was deleted outright. Any taint under a running launcher
  did the same: memory pressure, disk pressure, NotReady. Placement now gates
  creating a launcher, never a running one.

- **A pool replaced a failing replica in a tight loop** (#593). Replacement
  deleted and recreated the same index in the same pass with no delay, so a
  failure a new copy also hits — a missing SwiftImage, class or kernel, or a
  rejected host path — became a delete/create loop, each turn recreating the
  replica's owned objects and bumping `kubeswift_vm_failures_total`. It also hid
  the failure, since the replica was gone before anyone could look. Replacement
  now backs off like the kubelet does for a crashing container: 10s doubling to
  a 5m cap, reset for a replica that lived 10m.

- **A disallowed host path was never reported on the guest** (#591). With the
  webhook off — the chart default — the allowlist was enforced only inside
  `buildPod`, whose errors are logged and retried but never written to status. A
  kernel-boot guest had no phase and no conditions at all; a disk-boot guest
  first provisioned a full root-disk clone, then sat in Scheduling with
  `Resolved=True` for good. The allowlist is now checked before the clone, and
  sets the `Resolved=False` condition that was already promised.

- **A schedule naming a date that never occurs spun the controller** (#586,
  contributed by @dantonioluigi). Cron accepts `0 0 31 4 *` — 31 is a valid day
  and 4 a valid month — and only discovers they never coincide while searching
  forward, after which it returns the ZERO time rather than an error. The zero
  time precedes every "now", so the tick always looked due: a snapshot named for
  year 1, recreated a second after an operator deleted it, a requeue every
  second, and a `Ready=True` saying it was all fine. Exactly six expressions are
  impossible (30 and 31 February, and the 31st of April, June, September and
  November); 29 February is not one of them and keeps working. The reconcile
  loop also guards the zero time directly, since 29 February searched from 2097
  genuinely has no next occurrence inside cron's five-year horizon.

- **A CI check failed when it passed** (#590). `verify-render-coverage.sh` piped
  a ~160 KB render into `grep -q`, which exits at its first match; under
  `pipefail` the writer took SIGPIPE and the pipeline reported 141 for a check
  that had matched, turning Render + validate red at random.

### Added

- **`charts/kubeswift/values.schema.json`** (#588) — Helm now rejects values keys
  the chart does not read, at both the top level and inside each component
  block. See the upgrade note above.

### Changed

- Dependencies: `connectrpc.com/connect` 1.21.0, `golang.org/x/net` 0.59.0,
  `x/sys` 0.48.0, `x/term` 0.46.0 and 21 indirect updates (#595); the
  codeql-action pins move to v4.38.0 (#596). govulncheck reports no reachable
  vulnerabilities.

### Docs

- Chart documentation corrected where it had drifted from the chart (#589):
  `gateway.authMode` has defaulted to `oidc` since the gateway became secure by
  default, not `insecure`; the hub one-liner did not actually render, because
  `role=hub` turns the gateway on and OIDC then requires an issuer and client
  ID; and the auth guide told operators to add raw `--oidc-*` flags that the
  chart's own guard rejects. Missing values rows filled in.
- Install pins swept to 0.13.15, including the GitOps examples, which had sat on
  0.13.11. `hack/verify-doc-versions.sh` now scans `examples/` as well as
  `README.md` and `docs/`.

---

## [v0.13.14] — 2026-09-11

A bug-fix release for scheduled snapshots. Three separate defects in
`SwiftSnapshotSchedule`, each of which let a schedule stop doing its job without
saying so: a cron expression evaluated in the wrong timezone, a parser panic on
one input shape, and an unparseable schedule that was logged and discarded.

Also moves the chart's console to kubeswift-ui v0.12.4, which clears every
outstanding npm advisory in the UI (27 to 0, including an Angular i18n XSS and a
cache-key ambiguity that leaked responses across requests).

**One CRD gains a print column.** No new fields, so a stale schema drops
nothing -- but `kubectl get sss` will not show READY until you apply the CRDs.

### Upgrade

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.14 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # for the READY column on schedules
```

`ui.image.tag` moves from `v0.12.3` to `v0.12.4`. No values keys were added or
removed.

### Fixed

- **Schedules ran on the pod's local wall-clock, not UTC** (#580).
  `spec.schedule` is documented as UTC in the Go type, `docs/crds.md` and the
  guide, but `cron.ParseStandard` leaves an unzoned expression at `time.Local`
  and `metav1.Time` decodes to local, so `Next()` evaluated every schedule
  locally. Under `Europe/Rome`, `0 2 * * *` fired at 00:00 UTC and shifted again
  across DST; with `startingDeadlineSeconds` set, a displaced tick is skipped --
  so the snapshot is not late, it is missing. The shipped distroless image
  carries no tzdata and resolves `time.Local` to UTC, so a default install was
  unaffected; a mounted `/etc/localtime`, a different base image, or running
  outside a container was not. A `CRON_TZ=`/`TZ=` prefix still selects another
  zone.

- **A `TZ=` prefix with no cron fields panicked the controller** (#583).
  cron v3.0.1 extracts the zone with `spec[eq+1:i]`, where `i` is the index of
  the first space; with no space `i` is -1 and the slice panics with
  `slice bounds out of range [:-1]`. `spec.schedule` is user-supplied, so
  `TZ=Europe/Rome` with nothing after it reached that line straight from a CR.
  The controller and the webhook now parse through one guarded helper, so a
  check added to one cannot miss the other. Reported upstream as
  `robfig/cron#470` (also `robfig/cron#566`, `robfig/cron#574`), all still open,
  with the project last pushed in July 2024 -- so guarding locally is the fix,
  not a stopgap.

- **An unparseable schedule was logged and dropped** (#584). The reconciler
  returned without touching status, so the object read as healthy under
  `kubectl get` -- schedule, suspend, guest and age all populated -- and simply
  never fired. A schedule now reports a `Ready` condition (`Scheduled`,
  `InvalidSchedule` or `Suspended`) carrying the parse error in its message,
  surfaced as a `READY` print column. The `Conditions` field and its `Ready`
  constant had been declared and documented since the kind shipped, and
  populated nowhere.

### Changed

- `ui.image.tag` now defaults to `v0.12.4` (was `v0.12.3`).
- `sigs.k8s.io/controller-runtime` 0.24.1 to 0.25.0 (#577); and
  `go-containerregistry` 0.22.1, `docker/cli` 29.8.0, `go-jose/v4` 4.1.5,
  `golang.org/x/crypto` 0.56.0 (#578). govulncheck reports no reachable
  vulnerabilities and the image scan is clean.

### Docs

- `ADOPTERS.md` and `CONTRIBUTING.md` (#579).
- The UTC wording on `spec.schedule` is now unambiguous and the `CRON_TZ=`
  escape hatch is documented (#582).
- Install commands are checked against the chart version by
  `hack/verify-doc-versions.sh`, after the README and eight pages sat three
  releases behind (#576).
- `docs/snapshots/scheduled-snapshots.md` gains a Status section for the new
  condition, and the claim that the webhook validates the cron expression up
  front is corrected -- that holds only when `webhook.enabled` is on, which by
  default it is not.

---

## [v0.13.13] — 2026-09-05

A feature release: guests can now be pinned to dedicated host CPUs and backed
by hugepages, both set on the guest class. Also clears the two fixable HIGH
CVEs that had failed the nightly image scan every night since 2026-08-30, and
moves the whole build onto Go 1.27.

**Two new SwiftGuestClass fields, so `helm upgrade` alone does not deliver
them.** Helm treats `crds/` as install-only.

### Upgrade

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.13 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # REQUIRED this release — the new fields live here
```

One CRD gains fields (`swiftguestclasses.swift.kubeswift.io`). No values keys
added or removed. Every new field defaults to today's behaviour, so existing
guest classes are unchanged and need no edit: `cpuPinning` defaults to `none`
and `hugepages` to unset, which emit byte-identical Cloud Hypervisor arguments
to v0.13.12.

### Added

- **vCPU pinning and SMT placement** (#566). `SwiftGuestClass.spec.cpuPinning:
  static` pins each vCPU to one host CPU, and `smtPolicy` (`spread`/`pack`)
  chooses which hyper-thread siblings it uses — rendered as Cloud Hypervisor
  `--cpus affinity=`.

  The map is computed in swiftletd from the **launcher pod's own effective
  cpuset**, not controller-side from node topology. Under the kubelet CPU
  Manager `static` policy a pod's exclusive CPUs are assigned at admission,
  after the controller has written the runtime intent, so a CPU chosen earlier
  falls outside the pod's cgroup and is clamped or rejected — the guest would
  run unpinned while still reporting as pinned. Reading the cpuset at launch is
  correct under both policies. A cpuset smaller than the vCPU count fails the
  launch with an explicit message rather than pinning partially.
  [`docs/performance/cpu-pinning.md`](docs/performance/cpu-pinning.md).

- **Hugepage-backed guest memory** (#569). `SwiftGuestClass.spec.hugepages`
  (`2Mi`/`1Gi`) backs guest RAM with hugepages via `--memory hugepages=on`.

  Guest RAM **moves** to the `hugepages-<size>` resource rather than being
  requested twice: the kubelet already subtracts reserved hugepages from the
  node's allocatable memory, so a pod booking both would consume twice the
  guest's RAM and stop scheduling long before the pages ran out. The launcher
  requests the guest's memory as hugepages plus only its own overhead as
  ordinary memory, and gets a size-qualified `emptyDir` at `/dev/hugepages`.
  A class requesting a size the node has not reserved does not schedule — the
  failure is visible, and it happens before any VM starts.
  [`docs/performance/hugepages.md`](docs/performance/hugepages.md).

- **`cosign freshness` CI job** (#574). Checks the pinned cosign against the
  latest upstream release and that the images agree with each other.
  `COSIGN_VERSION` is a Containerfile `ARG`, invisible to Dependabot, so the
  pin previously had no watcher at all.

### Fixed

- **Two fixable HIGH CVEs that had failed the nightly image scan since
  2026-08-30** (#567). `kubeswift-dra-driver` carried
  `google.golang.org/grpc` CVE-2026-84304, and `migration-stunnel` carried
  `libcrypto3`/`libssl3` CVE-2026-14456.

  Neither would have fixed itself. grpc is an **indirect** requirement, and
  Dependabot only proposed direct ones. libcrypto3 lives in the **base layer**,
  outside stunnel's dependency closure, and `apk add` leaves an
  already-installed package at whatever version the base image froze — the
  fixed package sat in the Alpine repo the whole time and no rebuild would ever
  have picked it up. The image now runs `apk upgrade` before installing, which
  closes the class rather than the instance.

- **DRA driver did not compile against k8s.io 0.37** (#568).
  `kubeletplugin.DRAPlugin` gained `WatchHealthStatus`. The driver declines
  health reporting with `ErrHealthNotSupported` rather than reporting a
  hardcoded `Healthy`: answering that call promises the kubelet a fresh report
  inside each device's `HealthCheckTimeout`, so a GPU whose vfio-pci binding
  had gone would keep reading as usable. A compile-time interface assertion now
  makes the next such change fail on the type rather than at a call site.

- **`golang.org/x/mod` CVE-2026-56864/56865** (#568), pulled in transitively by
  the k8s 0.37 bump.

### Changed

- **All nine images build on Go 1.27** (#549–#556), and swiftletd's Rust
  builder moves to 1.98 (#563).
- **Dependabot can see indirect Go dependencies, and the `kubernetes` group
  actually fires** (#570). That group had existed since Phase 1 without ever
  producing a PR — every `k8s.io` bump arrived inside `go-minor-patch`
  instead, so an API-breaking Kubernetes minor shipped alongside routine
  patches. Both catch-all groups now exclude what their paired specific group
  owns, which is correct regardless of group evaluation order.
- **Trivy's report pass skips the vendored cosign binary** (#574). It was
  uploading 16 permanently-unactionable alerts describing sigstore's build
  rather than ours — the clearest being an `x/crypto` CRITICAL reporting
  v0.53.0 while KubeSwift's own module graph was already on the fixed v0.55.0.
  The gate had skipped that file all along.
- Third-party project references removed across the tree (#548, #560). CRD
  descriptions only; no schema change.

## [v0.13.12] — 2026-08-24

A bug-fix release, and the headline is that a documented feature has never
worked on any released version. `SwiftSeedProfile.spec.userDataFrom` — the
Secret-backed seed path, which is what the docs tell you to use so cloud-init
credentials stay out of Git — was rejected by the apiserver on every version up
to and including v0.13.11.

**The fix is a CRD schema change, so `helm upgrade` alone does not deliver it.**
Helm treats `crds/` as install-only, with no flag to change that.

### Upgrade

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.12 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # REQUIRED this release — the CRD *is* the fix
```

One CRD changes (`swiftseedprofiles.seed.kubeswift.io`). No values keys added or
removed, no controller behaviour change, no image changes beyond the routine
dependency bumps below. The schema change only *relaxes* a constraint, so
existing SwiftSeedProfiles remain valid and need no edit.

### Fixed
- **`SwiftSeedProfile.spec.userDataFrom` was impossible to use** (#546).
  `UserData` was a bare required field in the schema, so the apiserver rejected
  every `userDataFrom`-only profile with `spec.userData: Required value` —
  *before* admission ran, which meant the validating webhook's own (correct)
  either-or check never saw the object. The path was implemented end to end
  (`internal/seed/render.go`, `internal/resolved/merge.go`), shipped a sample,
  and is what `docs/gitops/secrets.md` recommends; none of it was reachable. The
  Go type had said so since it was written: `// Inline; use UserDataFrom for
  ref`.

  `userData` becomes optional and the either-or moves into a CEL
  `XValidation` rule on the spec, so it is enforced in-tree by the apiserver
  rather than by the webhook — which matters because `webhook.enabled` is
  `false` by default, and a rule that only holds when the webhook is on is not a
  rule. Verified on a cluster in all four cases: neither field rejected,
  `userData` alone accepted, `userDataFrom` alone accepted (previously
  impossible), and `userData: ""` rejected.

  Found by validating every shipped manifest against the CRD schemas
  *client-side*. That is the configuration the defect lives in: `kubectl apply
  --dry-run=server` against a webhook-enabled cluster cannot see this class,
  because the defaulting webhook papers over it.

- **The chart rendered image tags as `vv0.13.11` when used from a checkout**
  (#545). `Chart.yaml` carried `appVersion: "v0.13.11"` and `kubeswift.imageTag`
  prepends `v`, producing a tag that was never published — for the controller
  and for the five launcher images it passes on by env var, so an affected
  install would fail to start VMs, not merely fail to start. **Released charts
  were never affected**: the release workflows package with
  `--app-version "${TAG#v}"`, and the flag wins over the file. Only
  `helm install ./charts/kubeswift`, `helm template` for review, and
  install-from-source were hit, which is how it survived several releases.

  Fixed in three places, because the value alone would be correct only until the
  next hand-edit: the value is bare with the constraint written at the point of
  edit, `imageTag` trims a stray leading `v`, and `hack/verify-image-tags.sh`
  fails the build on any rendered tag that is not `vX.Y.Z`, `sha-<hex>` or
  `latest`. Nothing else in the manifests job covered this — kubeconform checks
  schema, kube-linter checks policy, and a nonexistent tag is valid under both.

- **`config/samples/golden-image/swiftimage-oci.yaml` could not apply without
  the webhook** (#546). It omitted `spec.format`, which is required in the
  schema and supplied only by the defaulting webhook, so it worked on a default
  install and was rejected wherever `webhook.enabled=false`. Now set explicitly.

### Changed
- `golang.org/x/net` 0.57.0 → 0.58.0 and `google.golang.org/protobuf` to the
  1.36.12 release (#536, #537). Routine grouped bumps, no security advisory;
  govulncheck reports no affecting vulnerabilities.
- CI action pins moved forward (#538, #539, #542, #543). The
  `github/codeql-action/*` sub-actions are now grouped for Dependabot: they must
  share a version or `analyze` refuses the config `init` wrote, and ungrouped
  they arrived one at a time, red on arrival. Twice.

### Documentation
- **GitOps docs refreshed for v0.13.x** (#544), having last been written at
  v0.5. The three-layer model covered six of the fifteen CRDs; it now covers all
  of them, plus the inverse — the kinds that must *not* be committed, because
  roughly half the CRD surface is one-shot or controller-owned and Git-managing
  it produces a reconcile loop rather than a mess.
- **`docs/gitops/oci-artifacts.md`** — the registry as the artifact store. Four
  artifact types come out of one registry: the chart, a golden VM disk
  (`SwiftImage.spec.source.oci`), a kernel (`SwiftKernel.spec.ociRef`), and VM
  snapshots pushed back out (`SwiftSnapshot` `backend.type: oci`). Digest-pin
  them for the same reason you pin the chart.
- The Flux reference repo gains a golden OCI image, a kernel, snapshot schedules
  (CSI and OCI) and a warm sandbox pool, and its chart pin moves off
  `semver: ">=0.1.0"` — which on a pre-1.0 project with `v1alpha1` APIs let a
  reconciler roll the platform across a breaking minor unattended.

## [v0.13.11] — 2026-08-16

### Fixed
- **Rook Ceph RBD is usable end to end** (#532). Two defects, both of which left
  a guest broken while reporting nothing wrong. Neither is Ceph-specific in
  origin — Ceph *enforces* where Longhorn tolerates, so it is the driver that
  exposed them.
  - `cloneStrategy: snapshot` silently produced an **unbootable Block root
    disk**. A CSI VolumeSnapshot clones the source *volume*, and the SwiftImage
    import PVC is Filesystem-mode holding the disk as a file inside it, so
    cloning it into a Block root disk yields a block device whose content is a
    filesystem. `clone-grow-init`'s `sgdisk -e` then found no GPT and wrote a
    fresh empty one. Drivers honouring `allow-volume-mode-change` bind the PVC
    happily, so the guest reached `Running` with `StorageReady=True` and never
    booted. The clone path now compares the image PVC's volumeMode against the
    resolved root-disk volumeMode and falls back to the copy path when they
    differ, logging why. Matching modes keep the CoW path. This had made every
    RWX+Block class — the shape live migration requires — unusable with a
    snapshot-strategy image.
  - **Restore always provisioned RWO+Filesystem** regardless of the source disk,
    so restoring a Block+RWX guest asked the driver to reinterpret the snapshot
    bytes. Ceph refuses the mode change and the PVC never bound, leaving
    `SwiftRestore` in `Restoring` indefinitely; once forced past that the
    launcher failed permanently with `volume root-disk has volumeMode
    Filesystem, but is specified in volumeDevices`. The restore now reproduces
    the source disk's accessMode, volumeMode and storageClass.

### Added
- **`SwiftImage.spec.importStorageClassName`** (#534, closes #533) — selects the
  storage class for the import PVC. Empty keeps the previous behaviour (cluster
  default StorageClass), so existing images are unaffected. Without it an image
  was pinned to the default backend, and since `cloneStorageClassName` defaults
  to the import PVC's class, so were its guests — the only workaround was
  flipping the cluster-wide default around the import. Immutable once the import
  has started, since a bound PVC's storage class cannot change.

### Changed
- Clone-strategy compatibility matrix records Rook Ceph RBD as validated, with
  the Block-mode caveat (#531).

## [v0.13.10] — 2026-08-14

An observability-and-hygiene release. A guest that never gets an IP now says why
instead of looking healthy forever; per-launcher-pod RBAC covers every launcher
class; swiftletd moves four majors of kube-rs and sheds 26 crates.

No CRD schema changes, no API changes. One new values key
(`scopedLauncherRBAC.enabled`, default `false`).

### Upgrade

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.10 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # helm does not upgrade CRDs
```

The controller ClusterRole gains `roles` and `delete` on `rolebindings` (needed
by `scopedLauncherRBAC`). `helm upgrade` applies it; a controller image newer
than its ClusterRole logs the failure and keeps running rather than blocking
workloads.

### Fixed

- **A guest that never gets an IP now says so** (#527). `NetworkReady=False` with
  reason `DHCPTimeout` appears on the SwiftGuest once swiftletd's lease poller
  gives up, instead of the guest sitting at `Running` / `GuestRunning=True` with
  an empty `status.network.primaryIP` indefinitely.

  Previously the timeout was a single `log::warn!("lease_poll_timeout")` in the
  launcher and nothing else — nothing on the CR distinguished "never going to get
  an IP" from "still booting", so diagnosing it meant reading launcher logs and
  attaching to a serial console.

  The condition message names the likely cause. The common one is knowable from
  the spec: a **disk-boot guest with no `seedProfileRef` gets no NoCloud seed**,
  so cloud-init finds no datasource, never writes netplan, and the interface is
  never configured — the guest boots fine to `multi-user.target` and simply never
  sends a DHCP request. That case now reads:

  > no DHCP lease after 240s; the guest booted but never requested one. This
  > guest is disk-boot with no seedProfileRef, so it gets no NoCloud seed:
  > cloud-init finds no datasource, never writes netplan, and the interface is
  > never configured. Set spec.seedProfileRef, or use an image that configures
  > its own networking

  The condition recovers to `True` if a lease arrives late, so it never latches
  false. Deliberately **not** a webhook rejection of "disk-boot without a seed" —
  an image that self-configures is a valid shape; the goal is visibility, not
  prohibition. No CRD schema change (conditions are not schema).

### Changed

- **Lab infrastructure names replaced with placeholders across tests, samples
  and docs** (#529). Node names are now `worker-1` / `worker-2` / `cp-1`, fleet
  members `edge-1` / `edge-2` / `edge-3`, and the external-API-server example
  `k8s-api.example.com`. 95 files, 800 insertions and 800 deletions — a pure
  rename with no behaviour, logic or schema change. Affects nothing at run time;
  listed because sample manifests and docs an operator copies from now use the
  placeholder names.

- **swiftletd: kube-rs 0.92 → 4.2, k8s-openapi 0.22 → 0.28** (#499, #501). Four
  majors of kube-rs, no source changes required: swiftletd only uses the stable
  core of the client — `Api::namespaced`, `patch`, `patch_status`, a
  `DynamicObject` for the one `GuestRunning` status patch, and
  `Config::incluster_dns` — and none of those changed signature.

  The `runtime` and `derive` features were enabled but never used by a single
  line; swiftletd is a launcher, not a controller. Dropping them removes
  `kube-runtime`, `kube-derive`, `schemars`, `serde_yaml`, `json-patch`,
  `parking_lot`, `rand`, `backoff` and their subtrees — **237 → 211 unique
  crates** (51 removed, 25 added).

  `k8s-openapi` moves to the `v1_34` feature, matching the fleet, instead of
  trailing it by six minors on `v1_28`. Only `core/v1 Pod` is used.

  Two behaviour notes, neither of which affects the shipped path: kube 4.x adds
  an OS-native trust-store fallback via `rustls-platform-verifier`, which engages
  only when no CA is configured — `incluster_dns` always supplies the
  service-account CA, so the shipped path is unchanged. And `Client::try_from`
  now requires a Tokio reactor; `create_client` is async, so it always has one.
  A unit test pins both, plus the rustls-0.23 crypto-provider trap that would
  otherwise be a runtime panic on first status write.

### Security

- **Per-launcher-pod scoped RBAC** (#515), behind `scopedLauncherRBAC.enabled`
  (default `false`). Every launcher pod — SwiftGuest, migration target,
  SwiftSandbox, and warm pool slot — now gets its own Role + RoleBinding
  granting `pods: get,patch` on **exactly its own pod** (and, for a guest,
  `swiftguests/status` on its own CR). Enabling the gate retires the shared
  namespace-wide RoleBinding, leaving those per-pod grants as the only access.

  This is **defence in depth, not the fix for #443**. RBAC is additive and the
  launcher ServiceAccount is shared, so scoping alone cannot stop an attacker who
  obtains the SA — the ValidatingAdmissionPolicy (`launcherSAGate`, v0.13.8) is
  what does. This bounds what the token is worth if it leaks another way.

  Scope was validated before the mechanism was built: a guest ran its full
  lifecycle on a live cluster under a hand-made self-only Role — boot, IP
  discovery, status reporting, stop — with zero RBAC denials, while
  `patch pod/<other>` was denied.

  A sandbox launcher is granted **no** `swiftguests/status`: it runs untrusted
  code and has no SwiftGuest CR to report to, so granting it would let an escaped
  sandbox forge guest status.

  Warm pool slots take two phases, because a slot pod is created by the pool but
  re-parented to a SwiftSandbox on checkout, so neither CR owns it for its whole
  life. The pool creates the grant owned by itself (the pod does not exist yet,
  and the grant must precede it), then hands ownership to the slot pod, which is
  the only owner whose lifetime matches. A pool also converges grants for slots
  it already has, so enabling the gate on a running pool does not cut off live
  slots.

  Object cost is two per launcher pod, garbage-collected with it. Failure to
  retire the shared binding is logged at ERROR and never blocks a workload from
  booting — the exposure in that case is exactly the pre-change posture.

  Requires `roles` and `delete` on `rolebindings` in the controller ClusterRole;
  `helm upgrade` covers this, but a controller image newer than its ClusterRole
  will log the failure and keep running.

---

## [v0.13.9] — 2026-08-14

A supply-chain release. Both cosign-bearing images drop from **48 fixable CVEs
(1 critical, 24 high) to 4 (0 critical, 3 high)**, the Go toolchain picks up 7
standard-library fixes, and a signing behaviour that could have leaked private
artifact digests is now caught by a test rather than shipped quietly.

No CRD schema changes, no API changes, no values changes.

### Upgrade

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.9 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # helm does not upgrade CRDs
```

### Security

- **cosign 2.6.5 → 3.1.3 in `sandbox-materialize` and `snapshot-oras`** (#486).
  48 fixable CVEs → 4 on each image, measured on the built images; the critical
  is gone. The four that remain are inside cosign's own build.

  Both images were pinned to 2.x because 3.x rejects `--tlog-upload=false`, the
  flag used to sign offline. **Dropping that flag is not the fix**: cosign 3.x
  then signs successfully, exits 0, and uploads the artifact digest to the
  **public Rekor**. For a private VM snapshot or an air-gapped golden image that
  is a disclosure. The signer now passes a signing-config declaring no
  transparency-log service, selects the dialect from the cosign major found at
  run time (so `swiftctl image publish` still works against an operator's own
  2.x install), and **refuses to sign** rather than falling back to the
  silent-upload form.

  Existing v2.6.5 signatures keep verifying. `hack/cosign-interop.sh` proves it
  across both majors in both directions, with negative controls, and fails on
  any transparency-log entry.

- **go 1.26.5 → 1.26.6** — 7 standard-library advisories reachable from our code
  (`net/url`, `html/template`, `crypto/tls`, `net/http`, `encoding/xml`,
  `encoding/asn1`). Not a regression from any change here: the same commit
  passed `govulncheck` and then failed it hours later on a database update.

### Fixed

- **swiftletd no longer logs a 403 at every sandbox exit** (#519). It was trying
  to patch a `SwiftGuest` CR that does not exist for a sandbox. Cosmetic —
  sandbox status was always reported correctly — but it named an RBAC denial on
  every run, which invites granting the privileged launcher ServiceAccount
  `swiftguests/status`. The existing `KUBESWIFT_REPORT_GUEST_CR` switch was
  applied to one of the three places that write the CR; it now covers all three.
  No RBAC change.

### Documentation

- The manual golden-image verify command was **wrong** and would have looked like
  an invalid signature: it omitted `--insecure-ignore-tlog=true`, which offline
  signatures require. Corrected, with an explanation of why the flag is not a
  weakening here.

---

## [v0.13.8] — 2026-08-13

Closes a privilege escalation. Anyone who could create an ordinary Pod in a
namespace where a guest or sandbox ran could reach **node root** — no SwiftGuest
required, no CRD access needed. If you run multi-tenant namespaces, this is the
release to take.

### Upgrade

Drop-in from v0.13.7 — no CRD schema changes, no API changes, no values changes
required.

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.8 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # helm does not upgrade CRDs
```

The new admission policy is **on by default** and cluster-scoped. It renders
only where the cluster serves `admissionregistration.k8s.io/v1
ValidatingAdmissionPolicy` (Kubernetes **1.30+**); on older clusters it is
silently skipped and nothing changes. Disable with
`launcherSAGate.enabled=false` if a policy engine of your own enforces the same
rule — but read the trust-boundary note in `docs/security-audit.md` first,
because turning it off restores the escalation.

### Fixed

- **A tenant who can create a Pod can no longer reach node root** (#443, #514).

  Launcher pods run `privileged: true` and are a node-level trust boundary.
  swiftletd needs `pods: patch` to report status, so the launcher
  ServiceAccounts carry it — and **Kubernetes has no RBAC gate on which
  ServiceAccount a pod may name**. So anyone able to create an ordinary Pod
  could write `serviceAccountName: kubeswift-launcher`, receive that token,
  patch the privileged launcher's `image`, and get node root. kubelet restarts a
  container whose spec hash changed regardless of `restartPolicy: Never`.

  v0.13.6's dedicated ServiceAccounts removed *incidental* inheritance and made
  the grant auditable, but did not close this. Neither would `resourceNames`
  scoping, which is what the issue originally proposed: RBAC is additive and the
  ServiceAccount is shared, so an attacker still inherits the union of every
  launcher pod name in the namespace. Both were tested on a live cluster before
  being ruled out.

  What closes it is a `ValidatingAdmissionPolicy` supplying the missing gate:
  only the KubeSwift controller may create a Pod naming a launcher
  ServiceAccount. A policy rather than a webhook deliberately — the rule matches
  every Pod CREATE, and a webhook with `failurePolicy: Fail` would make all pod
  creation in the cluster depend on our webhook server being up. Admission
  policies are evaluated in-tree.

  Validated on three clusters and two CNIs with the gate live: guest boot, GPU
  guest, live migration (including the `<guest>-mig-<uid>` destination pod),
  sandbox, and warm-pool slots (whose names are random) all admitted normally;
  both launcher ServiceAccounts refused to an ordinary pod. Zero unintended
  denials across the whole exercise.

- **RC releases were publishing an unsigned image** (#512, #513). `release-rc`
  still signed from a hand-written list of seven while building eight, so every
  RC shipped `sandbox-materialize` unsigned — the identical defect #497 fixed in
  `release-stable`, which had shipped it unsigned for five releases. RC now
  derives its sign list from the same digest-pinned refs the build emits and
  verifies its own signatures before publishing. Its pre-release body had
  drifted the same way, listing six of eight.

### Changed

- **The manifest policy checks render through one script**
  (`hack/render-lint-profile.sh`). `helm template` has no
  ValidatingAdmissionPolicy in its default capability set, so a
  capability-guarded template renders to nothing — CI would have linted the new
  security control without being able to see it, while reporting green. Both the
  coverage check and the workflow now share one render, and the coverage check
  asserts the policy is present.

- `sigstore/cosign-installer` v3 → v4 (#502). The explicit
  `cosign-release: 'v2.6.5'` pin is retained deliberately: v4 defaults to cosign
  3.0.5, which rejects `--tlog-upload=false` at runtime — the flag the offline
  artifact-signing path needs.

### Known issues

- **45 fixable CVEs (1 critical, 23 high) remain in the vendored `cosign`
  binary** shipped inside `snapshot-oras` and `sandbox-materialize` (#486).
  They are **not** in KubeSwift code and not in the VM runtime: they are in a
  signing helper's frozen dependency tree, which nothing in our tooling can
  bump. v2.6.5 is the newest 2.x, and 3.x needs the offline signing contract
  redesigned.

  Measured: linking cosign as a Go library instead of vendoring the binary would
  resolve **30 of the 45, including the only critical**, because Go's minimal
  version selection would build those packages at the newer versions this
  repository already carries. The remaining 15 are cosign's own sigstore
  dependencies. That work is gated on proving signature interop with signatures
  already in users' registries, so it is tracked rather than rushed.

---

## [v0.13.7] — 2026-08-11

A supply-chain release. Not one line of Go or Rust changed between v0.13.6 and
v0.13.7 — the diff is workflows, the chart, docs and dependency lockfiles. What
changed is how much of the release you have to take on trust: every prior
version asked you to believe that the images in the registry are what this
repository built, and this one proves it in the same job that publishes them.
Building that proof immediately turned up an image that had never been signed
at all.

### Upgrade

**`ui.image.tag` must be `v0.12.3` or newer**, and the chart now ships pinned to
it rather than tracking `latest`. The UI Deployment sets
`readOnlyRootFilesystem: true`, which earlier UI images cannot survive: they
generate their runtime config inside the web root, a directory no volume can make
writable without hiding the application. An older tag fails closed and loudly:

```
30-kubeswift-config-js.sh: can't create /usr/share/nginx/html/config.js: Read-only file system
```

If you override `ui.image.tag`, raise it in the same step as the chart upgrade.
The tag is still not chart-derived — kubeswift-ui releases on its own cadence, so
a chart upgrade will not move it for you.

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.7 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml) \
  --set ui.image.tag=v0.12.3
kubectl apply -f charts/kubeswift/crds/    # helm does not upgrade CRDs
```

Otherwise this is a drop-in upgrade from v0.13.6: no CRD schema changes, no API
changes, and no KubeSwift source changes at all. The binaries are rebuilt, so
they do carry the dependency updates and toolchain pins below, but no KubeSwift
logic differs from v0.13.6.

### Fixed

- **`sandbox-materialize` was shipping unsigned, and had been since it entered
  the release set** (#497). The sign list in `release-stable.yaml` was written by
  hand and covered eight of the nine published images. Probing the registry
  directly at v0.12.0, v0.13.0, v0.13.2, v0.13.4 and v0.13.6 finds no signature
  at any of them, while every other image at those same tags verifies — so
  anyone running `cosign verify` across the full set has been getting a failure
  on that one image for five releases, and every release job reported success.

  The sign list is now derived from the same digest-pinned refs the build step
  emits, so an image cannot be published without also being signed; and the
  release **verifies its own signatures and attestations before finishing**, so a
  signing failure now fails the release instead of producing a quietly unsigned
  artifact. v0.13.7 is the first release in which all nine images are signed.

  Re-verify any deployment that pinned digests on the assumption the whole set
  was signed. The images themselves were never in question — only the signatures.

- **The image scan queued 117 jobs on a single push** (#489). `paths:` filtering
  was applied to the `pull_request` trigger but not to `push`, so every merge to
  `main` re-scanned all nine images regardless of whether anything in them
  changed. Both triggers now carry the same filter.

### Added

- **The CI security programme (#466) is complete — five phases, four of them
  gating.** Each answers a different question, and they are deliberately gated
  differently, because a scanner that cries wolf gets deleted rather than tuned:

  | Phase | Asks | Gate |
  |---|---|---|
  | 1 — dependencies + secrets (#467) | is something we depend on known-vulnerable? is a credential committed? | fails the build |
  | 2 — images (#480) | is something *in the published image* vulnerable? | fails on **fixable** findings only |
  | 3 — SAST (#487) | did we write a bug? | **report-only** |
  | 4 — manifests + chart (#494, #495, #496) | would this chart render something we would reject in review? | fails the build |
  | 5 — signing + attestation (#497) | is what we published what we built? | fails the release |

  Phase 3 is report-only on purpose: gosec reports 86 findings on the current
  tree, all triaged and recorded as a baseline in the workflow header. Turning
  that red on day one would mean 86 things to clear before anyone could merge.

  Phase 4's policy baseline lives as per-object
  `ignore-check.kube-linter.io/<check>` annotations rather than a config-level
  `exclude:`, so a suppression is visible next to the object it excuses and
  applies only there. Each was verified by removing it and confirming the check
  actually fires. It also asserts what it renders: a default `helm template`
  covers one of five workloads, so a coverage check fails if a new workload is
  added without being linted.

- **`Verify release` workflow** — re-verify any published tag's signatures and
  attestations on demand (Actions → Verify release). Deliberately not on a
  schedule yet; see the header for why, and for what has to be true first.

- **Toolchains are pinned** — Go `1.26.5` (#488) and Rust `1.97.1` (#490),
  matching the builder images. A floating toolchain changes scanner output
  without anyone touching the repository, which makes a recorded baseline
  meaningless.

### Changed

- **The web console runs with a read-only root filesystem** (#507), with
  `emptyDir` volumes at `/tmp` and `/etc/nginx/conf.d` — the only two paths the
  image writes. See Upgrade above for the tag floor this requires.

- **The web console has its own empty ServiceAccount**, with
  `automountServiceAccountToken: false` (#495). It serves static assets; the
  browser talks to the gateway, not this pod, so it needs no Kubernetes identity
  at all. Previously it fell back to the namespace `default` ServiceAccount and
  mounted that token — verified present in the running pod before the fix.

- **`gpu-discovery` asserts `runAsNonRoot` + `RuntimeDefault` seccomp** (#495).
  The image already ran as uid 65534; the pod spec now states it, so a future
  base-image change cannot silently regress it.

- **kube and k8s-openapi are grouped for Dependabot** (#498). They are
  version-locked (kube 0.92 → k8s-openapi ^0.22, kube 4.2 → ^0.28), so ungrouped
  updates arrived as two PRs that could not build individually *and* did not
  combine into a working pair. Both were closed; the pair now travels together,
  including across a major bump.

- **The orphaned `webhook-server` image was removed** (#492). Admission webhooks
  are served by the controller-manager; nothing referenced it.

- Roughly twenty dependency updates across base images, GitHub Actions, Go and
  Rust — the routine half of Phase 1 doing its job.

---

## [v0.13.6] — 2026-08-10

A correctness release. Every item is a case where the system did the wrong thing
quietly — a seed silently discarded, a safety interlock watching the wrong field,
a controller idling while reporting healthy, an RBAC subject that outlived its
reason to exist.

### Upgrade

**Upgrade the chart, not just the image tag.** v0.13.6's controller watches
ServiceAccounts (the launcher-SA work), and that rule ships in this version's
chart RBAC. A controller image newer than its chart's RBAC cannot sync its
informer cache — and as of this release it now **exits non-zero** rather than
running idle, so the mismatch presents as CrashLoopBackOff instead of a cluster
where nothing reconciles. That is the intended behaviour, but it means an
image-only bump fails loudly where it used to fail silently.

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.13.6 \
  -n kubeswift-system -f <(helm get values kubeswift -n kubeswift-system -o yaml)
kubectl apply -f charts/kubeswift/crds/    # helm does not upgrade CRDs
```

Also note **`gateway.authMode: insecure` now refuses to render alongside any
chart-managed exposure** — `gateway.ingress`, `ui.ingress`, or a
LoadBalancer/NodePort Service for either. If you are running that combination
today you are serving an unauthenticated control plane; switch to
`authMode: oidc`, drop the exposure and use port-forward, or set
`gateway.allowInsecureIngress=true` to accept the risk deliberately.


### Fixed

- **A drained namespace now retires the legacy `default` launcher subject**
  (#443). Subject convergence only ran from a SwiftGuest or SwiftSandbox
  reconcile, so once the last guest in a namespace was deleted nothing
  reconciled there again and `default` stayed bound to the reporter ClusterRole
  forever — the namespace-wide `pods: patch` grant left behind in a namespace
  with no launcher to justify it, which is when it is least defensible. A small
  reconciler now watches the launcher RoleBindings themselves, so the object
  carrying the stale subject drives its own cleanup after every CR is gone.

- **Helper Jobs no longer mount a ServiceAccount token** (#443). Thirteen
  pod specs across image import/validate, kernel pull, root/data disk
  provisioning, snapshot s3/oci/cold-migration and clone download now set
  `automountServiceAccountToken: false`. None of them talks to the API server.
  Launcher pods are deliberately untouched — swiftletd needs its token to
  report status.

- **A NoCloud seed with no `metaData` no longer discards the operator's
  cloud-config** (#457). NoCloud is only recognised as a datasource when the
  seed disk carries a `meta-data` file; the renderer omitted the key when the
  field was empty, so cloud-init fell back to `DataSourceNone` and threw away
  `userData` entirely — no user, no SSH key, no hostname — while the guest
  booted, took a DHCP lease and reported Running/Ready. Nothing in `kubectl`
  indicated anything had gone wrong.

  The controller now synthesizes `instance-id: <namespace>-<guest>` +
  `local-hostname: <guest>` when neither `metaData` nor `metaDataFrom` is set.
  `instance-id` must be stable per guest because cloud-init keys "have I already
  run for this instance?" off it. This is what the clone path already did, which
  is why clones worked and ordinary guests did not. An explicit value always
  wins; only NoCloud is affected.

- **The chart's insecure-ingress interlock now triggers on actual reachability**
  (#459). It only checked `gateway.ingress.enabled`, while the documented way to
  publish the UI is `ui.ingress` — and the UI's nginx proxies the Connect RPCs to
  the gateway at same origin, so it exposes the identical control plane. A hub
  went out unauthenticated on a public address with the guard silent and
  `allowInsecureIngress` still `false`. The check now covers `gateway.ingress`,
  `ui.ingress`, and a `LoadBalancer`/`NodePort` Service for either — the last of
  which needs no Ingress object at all. `authMode: insecure` remains usable with
  no exposure (port-forward), and the override is unchanged.

- **A cache that never syncs is now fatal instead of silent** (#460). When RBAC
  denies a watched type, the reflector retries forever: the manager stays up, the
  pod stays Ready, and *nothing* reconciles — no controller starts until every
  informer syncs — so fresh resources sat with a completely empty `.status`, no
  pod and no event. The manager now waits a bounded 3 minutes and exits non-zero
  with the likely cause, turning an idle-but-healthy process into a
  CrashLoopBackOff next to the reflector's own `is forbidden` lines. A cancelled
  context (SIGTERM, lost leader election) is still a clean shutdown.

  Note this differs from a *missing CRD*, which already failed at informer
  construction — only the RBAC-denied case was invisible.

### Added

- **Launcher pods run as dedicated ServiceAccounts** (#456). Until now they used
  the workload namespace's `default`, and the reporter ClusterRole was bound to
  it — so every Job, sidecar, CronJob and debug pod in that namespace silently
  inherited `pods: patch` on a **privileged** launcher. Guests and sandboxes now
  get separate SAs (`kubeswift-launcher`, `kubeswift-sandbox-launcher`), and a
  sandbox launcher no longer receives `swiftguests/status` at all — it never
  needed it, and granting it would let an escaped sandbox forge conditions on any
  guest in the namespace.

  The binding *converges* rather than being created once: the new SA is always
  bound, and `default` stays bound only while a launcher pod is still running as
  it, so an in-place upgrade does not break VMs that keep their original SA until
  they next stop or migrate.

  **If you attach `imagePullSecrets` to the namespace `default` ServiceAccount**
  for a private registry, launchers no longer inherit them. Set
  `swiftletd.imagePullSecrets` so the chart puts them on the pod, where they
  apply regardless of SA — otherwise the first guest after upgrade is
  `ImagePullBackOff`.

  Read the scope honestly: Kubernetes has no RBAC gate on which ServiceAccount a
  pod may reference, so this removes *incidental* inheritance and makes the grant
  auditable — it does not stop someone who can create pods from naming the SA.
  See `docs/security-audit.md` and #443.

- **`swiftctl image publish` survives a registry rate limit** (#455). A 429 —
  or a 403 whose body says rate limit, which is how GitHub signals its secondary
  limit — aborted the whole publish and threw away every chunk already uploaded.
  It now retries with jittered exponential backoff and scales chunk size with
  artifact size. A plain 403 is still permission-denied and still fails fast;
  retrying it would just be a slow way to reach the same error.

- **Operator guide for apiserver audit logging** (#454) —
  `docs/operator/audit-logging.md`, including why a VM platform needs it and the
  k0s file-permission trap that makes the apiserver refuse to start.

- **`spec.schedulerName` on SwiftGuest**, and therefore on every SwiftGuestPool
  replica (the pool copies its template spec wholesale). It sets
  `pod.spec.schedulerName` on the launcher, which lets a guest opt into a
  kube-scheduler profile configured with the `NodeResourcesFit`
  `LeastAllocated` scoring strategy.

  This closes the gap in #418. `spreadPolicy: Spread` counts pods, so it will
  place the next replica on a node that is already nearly full as long as that
  node holds no more pods of the pool than its neighbours. The launcher has
  always requested the guest's real footprint — cpu equal to the vCPU count,
  memory equal to guest RAM plus overhead, requests equal to limits — so the
  scheduler could already score on utilization; what an operator had no way to
  express was how heavily free capacity should weigh. Naming a profile is that
  expression. The two compose: spread keeps replicas off one node,
  `LeastAllocated` breaks the tie toward the emptiest.

  `nodeName` still wins — direct binding skips the scheduler entirely, so the
  field is left unset on the pod rather than written as configuration that
  never ran. An unknown profile name leaves the pod Pending indefinitely and
  KubeSwift cannot warn about it, because the set of scheduler profiles is not
  exposed through the API; the guide says so plainly.

---

## [v0.13.5] — 2026-08-09

An incident release. Two SwiftGuests were deleted from a lab cluster and the
cause could not be established — not because the evidence was ambiguous, but
because none existed. The gateway had served traffic for 42 hours and written
three lines, all from start-up.

### Added

- **The gateway records every RPC.** Mutations are logged unconditionally with
  the service, method, impersonated user, cluster, namespace, object name and
  outcome; reads sit at `-v=1` because list/watch/telemetry poll continuously.

  Classification is *default-mutating*: a procedure counts as a read only if its
  name begins with a known read verb, so an RPC added later that nobody
  classifies is logged loudly rather than skipped silently.

  This matters more than it looks for this object. A SwiftGuest carries no
  finalizer and no owner reference, so a delete takes the object, its launcher
  pod and its events together and leaves nothing behind to inspect. The gateway
  is the only place such a call can be observed at all.

  **This records calls that go through the gateway.** A `kubectl delete` still
  leaves no KubeSwift-side trace — for that, enable apiserver audit logging.

### Fixed

- **The fleet view reported an unreachable cluster as an empty one**
  (kubeswift-ui #42). `ListGuests` is partial-fleet: a member that fails yields
  an `errors` entry while the RPC still returns OK. The UI cleared its table and
  repopulated from an empty list, then set the state to "live" — so a failed
  query rendered as a confident **"No VMs across the fleet."**, and self-healed
  on the next 3-second poll, making it near-impossible to catch in the act.

  A cluster that errors now keeps its last-known rows flagged stale, "live"
  means the whole fleet answered (a partial answer is "degraded"), and the
  "No VMs" message is only shown when every member actually replied.

- Deleting a VM from the UI now requires typing its name. The previous yes/no
  confirm was not an adequate gate for an irreversible action that leaves
  nothing behind, and afterwards is indistinguishable from a delete nobody
  performed.

### Upgrade notes

No API, CRD or chart-value changes. **kubeswift-ui v0.12.2** carries the fleet
fix; the gateway change is independent and needs no UI update.

---

## [v0.13.4] — 2026-07-28

Second security pass. A UI-focused review found a class of defect in the v0.11.0
guided forms, and closing out every remaining finding from both reviews produced
six more fixes across the gateway, controller, chart and launcher. Validated on
the dev cluster — including the two items previous releases could only unit-test.

### Action required on upgrade

- **The rendered cloud-init seed is now a Secret, not a ConfigMap.** Anything
  reading `<guest>-seed` as a ConfigMap must read the Secret instead. A guest
  that is already running keeps its ConfigMap until it is next recreated (the
  controller will not delete it out from under a live pod); guests created after
  the upgrade never get one.
- **The console and sandbox WebSocket planes now expect the bearer as a
  `Sec-WebSocket-Protocol` subprotocol.** `?token=` still works and is logged as
  deprecated, so an older UI keeps functioning — but **kubeswift-ui must be at
  v0.12.0 or later** to use the new form. Upgrade both.
- **`auth-mode=insecure` now refuses cross-origin WebSocket upgrades** even with
  `gateway.corsAllowOrigin="*"`. Set an explicit origin, or use a real auth mode.
  No change for `oidc`/`token` deployments.
- **SwiftKernel is now validated at admission.** An `ociRef.image` containing
  shell metacharacters or `..`, or a `kernelCmdline` with a newline, is rejected.
- `make deploy` (kustomize) now installs 9 webhooks instead of 7 — SwiftSnapshot
  and SwiftRestore were silently missing.

### Fixed

- **Guided forms silently rewrote fields the operator never touched**
  (kubeswift-ui #37). `hydrate()` failed to recognise a variant and `build()`
  overwrote the subtree. The PodSpec editor rebuilt from scratch, dropping
  `automountServiceAccountToken`, `initContainers`, `envFrom`, `capabilities`,
  `seccompProfile`, `affinity`, probe tuning, extended resources like
  `nvidia.com/gpu`, env `valueFrom` and volumeMount `subPath`;
  `allowPrivilegeEscalation: false` was not even expressible; unmodelled volume
  types became `emptyDir` while keeping the name, so mounts still resolved onto
  an empty tmpdir. A SwiftSnapshotSchedule on the s3 or oci backend was rewritten
  to `local`, silently redirecting offsite backups to node disk; a SwiftImage on
  the oci source lost its cosign `verifyKeySecretRef`.
- **The WebSocket bearer travelled in the query string** (#445) and so was
  written to every access log on the path — the UI's nginx and any ingress in
  front of it — leaving replayable ID tokens readable by anyone with `pods/log`.
- **`CheckOrigin` accepted every origin** (#445). Harmless under a real auth mode,
  but under `auth-mode=insecure` any page the operator visited could open a
  console or a sandbox shell.
- **Cloud-init user-data sourced from a Secret was re-materialized into a
  plaintext ConfigMap** (#446) — SSH keys, passwords and join tokens readable
  with `get configmaps`.
- **controller-manager ran with no `securityContext`** (#447), alone among the
  components this project ships.
- **Release workflows ran Actions on floating tags** while holding
  `id-token: write` for cosign keyless signing (#447).
- **SwiftKernel had no validating webhook at all** (#448), and the snapshot
  host-path guard existed only in the webhook — which is disabled by default.
- **A SwiftGuest pinned to an unschedulable node stalled silently** (#449,
  closes #444). The taint check ran after the root-disk clone Job had already
  pinned to the same node, so reconcile never reached it.
- **Sandbox `restricted` egress was bypassable by source-address spoofing**
  (#449). The policy chain was entered on `-s <subnet>`; the guest configures its
  own NIC, so a forged source did not match the jump and skipped every DROP.
  IPv6 had no rules at all.

### Changed

- `migration.mtls` stays **off** by default; its documentation now states plainly
  that a cross-node live migration streams guest RAM in the clear when it is off,
  and that enabling it is a hard cert-manager dependency.

---

## [v0.13.3] — 2026-07-26

Security release. A five-domain review (gateway auth, RBAC/chart/containers, UI,
VM runtime, supply chain) produced eight fixes. Every one carries a regression
test that was verified to fail without its source change.

### Action required on upgrade

- **`gateway.authMode` now defaults to `oidc`** (was `insecure`, which performs no
  authentication at all). The gateway is not deployed unless `gateway.enabled=true`
  or `federation.role=hub`, so a bare install is unaffected — but if you enable the
  gateway you must now configure an IdP, or set `gateway.authMode=insecure`
  deliberately. The chart also refuses `authMode=insecure` together with
  `gateway.ingress.enabled`; override with `gateway.allowInsecureIngress=true`.
- **SwiftGuest host paths are confined to an allowlist that is empty by default.**
  `spec.filesystems[].source.hostPath` and vhost-user socket directories are
  rejected until an operator opts in with `swiftGuest.allowedHostPathPrefixes`.
  `pvcRef` shares are unaffected. See `docs/virtiofs.md`.
- **`spec.interfaces[].mac` must be a canonical MAC.** Previously unvalidated.
- **The `manage-rbac` capability no longer grants `escalate`/`bind`**, so the UI's
  Admin role can no longer grant permissions its holder does not already have.

### Fixed

- **Gateway `ListClusters`/`WatchClusters` were unauthenticated** — both returned
  every member cluster's apiserver URL, version and condition messages to any
  caller that could reach the port, and `WatchClusters` streamed it live. (#434)
- **Secret redaction was bypassed by the kubectl annotation.** Only `data` and
  `stringData` were stripped, so a Secret created with `kubectl apply` still
  carried its plaintext `stringData` in
  `kubectl.kubernetes.io/last-applied-configuration`. (#434)
- **OIDC claims could assert `system:masters`.** Claim values were copied verbatim
  into the impersonation headers with no reserved-prefix guard. (#434)
- **Command injection in the image-import and kernel-pull Jobs.** `fmt.Sprintf("%q")`
  is a Go quoter, not a shell quoter — it leaves `$` and backticks intact — and the
  result was placed inside shell double quotes. A SwiftImage URL or SwiftKernel OCI
  reference containing `$(...)` executed as root in a privileged container. Values
  now ride as environment variables. (#435)
- **The Cloud Hypervisor binary was downloaded without integrity verification**,
  while the firmware beside it was already checksum-pinned. Both architectures are
  now pinned. (#436)
- **Verify-before-boot had a TOCTOU in `sandbox-materialize`** — it verified one
  digest and then re-resolved the tag, so a tag swap between the two defeated
  `verifyKeySecretRef`. It now materializes the digest it verified. (#436)
- **Unconfined SwiftGuest host paths.** `hostPath: /` mounted the node root
  read-write into a tenant's VM. (#437)
- **`spec.interfaces[].mac` reached a sourced shell file**, so a MAC containing a
  command substitution executed in the privileged launcher. (#439)
- **`spec.nodeName` bypassed the scheduler's taint check.** Direct pod binding is
  deliberate (live migration depends on it), so the taint predicate is now
  reproduced in the controller instead of changing the binding. (#439)
- oras-go bumped for GO-2026-5880; both CI workflows declare least-privilege
  `permissions`. (#436)

### Changed

- The host-path allowlist is enforced in the controller as well as the validating
  webhook, so it holds on installs running with `webhook.enabled=false`. (#441)
- `docs/security-audit.md` SEC-01/02/03 move from RESOLVED to **ACCEPTED**: the
  capability-scoped launcher was implemented and then reverted because it broke
  QEMU boot, so every launcher is privileged by design. README and the GPU and
  operator docs said otherwise and are corrected. The consequence is recorded
  plainly — the launcher pod is a node-level trust boundary. (#438)

### Known

- The launcher pod runs as the namespace `default` ServiceAccount, which is bound
  `pods: get,patch`. Anyone able to create a pod in that namespace inherits it and
  can patch the privileged launcher's image. A dedicated ServiceAccount is planned
  but does not by itself close this — Kubernetes does not gate which ServiceAccount
  a pod may reference. Tracked.

---

## [v0.13.2] — 2026-07-24

Gateway enablement for permission-aware, general-purpose Kubernetes resource CRUD
in the kubeswift-ui Explorer. **No runtime, CRD, or migration change** — the
launcher and controller behave exactly as in v0.13.1; this release is the gateway
+ docs.

### Added
- **`ResourceService.CanI`** — a batch access-review RPC. The gateway runs a
  `SelfSubjectAccessReview` per (kind, verb, namespace) check through the user's
  own impersonated client, so the UI can hide create/update/delete actions the
  user can't perform instead of firing them and surfacing a denial. Fail-closed;
  needs no extra gateway RBAC (it is a self-review).
- **Broadened explorer catalog** — the ResourceService now surfaces the common
  native kinds for browse + (RBAC-gated) CRUD: Deployments, StatefulSets,
  DaemonSets, ReplicaSets, Jobs, CronJobs, Ingresses, and a new **Access**
  category (ServiceAccounts, Roles, RoleBindings, ClusterRoles,
  ClusterRoleBindings). Every call stays impersonated and gated by the user's
  Kubernetes RBAC; Secret values remain redacted on read.

Pairs with kubeswift-ui v0.10.0 (the proactive action gating + guided
Secret / ConfigMap / Service create forms).

## [v0.13.1] — 2026-07-23

Adds the sandbox-interactivity backend for kubeswift-ui and folds in the v0.13.0
doc sweeps. **No runtime, CRD, or migration change** — the launcher and
controller behave exactly as in v0.13.0; this release is the gateway + docs.

### Added

- **Gateway sandbox logs + interactive exec/attach** — two raw-WebSocket planes
  for kubeswift-ui: `/sandbox-logs` (read-only, `tail -F` the microVM console
  log) and `/sandbox-exec` (an interactive shell: pod-exec → the in-guest vsock
  agent → the `internal/guestagent` frame protocol). Both mirror the existing
  serial-console plane — a raw WebSocket (browsers can't do bidirectional
  Connect) with the bearer token on the query string, authorized by the
  impersonating client. **No proto, RBAC, or chart change**: new routes on the
  existing gateway listener, reusing `internal/guestagent`.

### Docs

- Install snippets across the docs now pin the current chart version (the
  `helm --version` lines had lagged at 0.11.0).

The matching **kubeswift-ui v0.8.0** ships the sandbox **Logs** + interactive
**Shell**, a guided sandbox-create wizard (model preload / scratch disk / GPU
profile / warm-pool checkout), a snapshot-schedule drawer with suspend/resume,
Fabric Manager partitions in the GPU node drawer, and a kernel drawer.

---

## [v0.13.0] — 2026-07-23

Moves the runtime to **Cloud Hypervisor v53.0** and adopts the safe v53 leverage wins.
The headline is a migration-internals rework forced by v53, done so live migration is
unchanged for operators: `vm.send-migration` became non-blocking in v53, so the
source-side completion gate now polls instead of relying on the call blocking. Two v53
opportunities were investigated and **deliberately not taken** because cluster
validation proved each would regress or was impossible — see below.

### Changed

- **Cloud Hypervisor v52.0 → v53.0** in the swiftletd image. The paired `CLOUDHV.fd`
  firmware is unchanged (validated on the v53 binary). Free fixes ride along: the guest
  clock now advances across snapshot/restore/migration, post-migration GARP speeds L2
  reconvergence, the memfd private-mapping memory regression is reverted, and
  guest-triggerable VMM panics are hardened (strengthening the SwiftSandbox boundary).
- **Source-side live-migration completion detection reworked for v53's non-blocking
  `vm.send-migration`** (cloud-hypervisor#8021). `send-migration` now returns the instant
  CH accepts the migration, so swiftletd polls `vm.info` until the source CH exits
  (success) or the deadline passes (failure, guest auto-resumed). Version-gated: on CH
  ≤ v52 the historic single-probe path is byte-identical. `receive-migration` remains
  blocking, so the destination handler is unchanged. Live migration behaviour and
  downtime are unchanged for operators.
- **Generic vhost-user CLI key `virtio_id` → `device_type`** (cloud-hypervisor#8564).
  Emitted to CH only; the `virtioId` CRD/RuntimeIntent field is unchanged.

### Added

- **`reserve=on` on guest memory** (cloud-hypervisor#8350): CH reserves guest-RAM commit
  at VM creation and fails fast with `ENOMEM` instead of a later out-of-memory kill.
- **Optional `--seccomp` mode via `KUBESWIFT_SECCOMP`** (`true|false|log|errno`;
  cloud-hypervisor#8578). Unset keeps CH's default (kill on violation); `errno` returns
  `EPERM` instead of killing the VMM — a debugging aid, off by default.
- **`swiftctl console` now replays the boot log** when attached after boot — CH v53
  buffers pre-connect serial-socket output (cloud-hypervisor#8322).

### Investigated, not adopted (validated regressions)

- **Native CH migration mTLS** (cloud-hypervisor#8053) was evaluated as a replacement for
  the stunnel sidecar and **rejected**: it connects to the destination by DNS name, and
  OVN-Kubernetes primary-UDN launchers cannot resolve cluster DNS (proven on-cluster), so
  it would break live migration for multi-node guests. The stunnel transport (already
  mutual TLS, pod-to-pod, IP-based) is retained.
- **Unprivileged GPU launchers via pre-opened VFIO/iommufd FDs** (cloud-hypervisor#8287)
  was spiked on real hardware and **rejected**: CH v53's iommufd DMA-map fails (`EFAULT`)
  for the test GPU regardless of memory backing, so the privileged legacy-VFIO GPU path
  is retained. May be revisited if a future CH release fixes iommufd.

---

## [v0.12.1] — 2026-07-19

A networking patch release: give a `SwiftGuest` a second, cross-node-routable NIC on
top of its node-local NAT primary, and fix an MTU blackhole on overlay CNIs. Together
these let a guest be a node in a multi-node cluster where the primary pod network is
not itself node-routable — validated on OVN-Kubernetes and on Calico VXLAN.

### Added

- **Secondary NAD interface IP hand-off.** A guest can attach a secondary
  NetworkAttachmentDefinition interface — an OVN-Kubernetes `layer2` secondary UDN, or
  a plain bridge / VXLAN-mesh NAD — that carries its own routable, cross-node IP into
  the guest, alongside a node-local NAT primary. The launcher reads the CNI-assigned
  address off the Multus interface and hands it to the guest by MAC (fixed-lease
  dnsmasq), and the controller stamps the guest MAC and a per-interface `IPAMClaim` on
  every OVN-Kubernetes NAD interface. The secondary hands **no default gateway**
  (`--dhcp-option=option:router`), so it does not shadow the primary's default route
  with a dead end on an isolated L2 — it keeps only its on-link subnet route for
  node-to-node reachability. (#419)

### Fixed

- **NAT guests now receive the pod's overlay MTU.** The primary NAT dnsmasq never
  handed the guest an MTU, so on an overlay CNI (Geneve/VXLAN, pod MTU < 1500) the
  guest kept 1500 and silently dropped every packet larger than the path MTU — no ICMP
  fragmentation-needed reaches the guest, PMTU discovery cannot recover, and TLS
  handshakes and package downloads hang. The guest is now handed the pod egress
  interface's MTU (`--dhcp-option=option:mtu`). No-op on a 1500-MTU cluster. (#419)

---

## [v0.12.0] — 2026-07-17

GPU sandboxes graduate from a DRA-only spike to a complete feature: `SwiftSandbox`/
`SwiftSandboxPool` gain a **native SwiftGPU allocation backend** (`spec.gpuProfileRef`,
alongside the existing DRA `gpuResourceClaim`), **warm GPU pools** that hold pre-booted
GPU slots for sub-second checkout, and **model preload** (`spec.model`) so an inference
checkout starts with weights already resident. Sandboxes also gain a
**scratch/persistent block disk** (`spec.scratchDisk`) and a working
`spec.imagePullSecret`. The QEMU **Tier-2/3 HGX topology runtime** ships — Tier 2
(`hgx-shared`, host Fabric Manager) is now allocatable and boots on the full generated
topology; Tier 3 (`hgx-full`, in-guest Fabric Manager) is rejected at allocation, since
the NVSwitch-into-guest passthrough isn't wired yet. The web console's Explorer now
lists sandboxes generically, retiring the dedicated gateway `SandboxService`.

### Added

**Tier-2/3 HGX QEMU topology runtime**
- The QEMU launch path now renders the full GPU topology the controller has always
  computed (previously dropped as a documented "when hardware is available" stub,
  leaving a flat PCI layout that CUDA rejects): each SXM device behind its own
  `pcie-root-port` (unique chassis/slot per QEMU docs/pcie.txt), `x-no-mmap=true`
  large-BAR handling, per-NUMA-node shared memory backends + `-numa` bindings,
  optional 1G/2M hugepage backing, SMP sockets matching the NUMA layout, and
  post-spawn vCPU→host-CPU pinning via QMP `query-cpus-fast` + `sched_setaffinity`
  — usable today for **Tier 2** (`hgx-shared`). The builder can also emit NVSwitch
  device-passthrough args for **Tier 3** (`hgx-full`), but Tier 3 allocation is
  rejected (see Fixed below), so that path isn't reachable through the controller
  yet. Grounded in the NVIDIA HGX Shared NVSwitch Passthrough Integration Guide
  (WP-12736-002). Validated without GPU hardware by `make verify-qemu-topology`,
  which boots the builder's exact args on the shipping QEMU with emulated PCIe
  endpoints substituted on the same root ports; VFIO/NVLink/Fabric-Manager runtime
  behaviour still requires real HGX hardware.

**GPU sandboxes — two allocation backends, warm GPU pools, model preload**
- **Pass a GPU into a `SwiftSandbox` via either backend.** `spec.gpuResourceClaim`
  (DRA — the scheduler + a DRA driver allocate at pod-schedule time) or
  `spec.gpuProfileRef` (native SwiftGPU — the SwiftGPU controller allocates at
  controller time), mutually exclusive. Either way a `gpu-init` container binds
  VFIO and the sandbox boots firmware-less (mode-3) with the GPU passed through —
  an ephemeral VM boundary around GPU inference / untrusted GPU code. The NVIDIA
  driver rides the guest OCI image and loads at start; a GPU sandbox boots the
  module-capable **`gpu-sandbox`** kernel profile (selected automatically). GPU
  nodes must also be kernel nodes. Single Tier-1 GPU per sandbox; multi-GPU /
  multi-node remain scoped follow-ups (#390). Cluster-validated on a GTX 1080
  (`nvidia-smi` in the guest). Runbook: `docs/sandbox/gpu-sandboxes.md`.
- **Native SwiftGPU allocation backend for sandboxes (`spec.gpuProfileRef`).** The
  SwiftGPU controller allocates the device(s) at controller time against a
  `SwiftGPUProfile` + `SwiftGPUNode` (no ResourceClaim), stamps `status.gpu`, pins
  the sandbox to the allocated node, and `gpu-init` binds the specific BDF(s) — the
  same allocation core the native SwiftGuest path uses. `tier: pcie` only;
  `hgx-shared`/`hgx-full` are rejected at allocation (`GPUAllocated=False` reason
  `UnsupportedTier` — use a SwiftGuest for the QEMU HGX path). (#410)
- **Warm GPU pools (`SwiftSandboxPool.spec.gpuProfileRef`).** Setting
  `gpuProfileRef` on a pool makes every warm slot a GPU sandbox holding a native
  SwiftGPU allocation, pre-booted so a checkout is sub-second instead of a cold GPU
  boot. Trades an idle GPU per warm slot for latency — size `minWarm` to the free
  GPU count. GPU pools warm only on nodes that are both `gpu-node` and
  `kernel-node`; `tier: pcie` only. A checking-out `SwiftSandbox` sets `poolRef`
  alone — never a GPU field — and inherits the slot's GPU implicitly (the webhook
  rejects `poolRef` + either GPU field on a user-authored sandbox: a GPU sandbox
  always boots cold, so the slot's GPU shape, not the claimant's request, is what
  matters). (#412)
- **Model preload (`spec.model`, on both `SwiftSandbox` and
  `SwiftSandboxPool`).** Mounts a read-only, node-shared model artifact — an OCI
  image whose filesystem holds the weights — into the sandbox over virtio-fs
  (default `/model`). Materialized once per node (digest-keyed cache,
  cosign-verifiable via `verifyKeySecretRef`) and shared read-only from the host
  page cache, so a warm GPU pool with a preloaded model needs neither a cold GPU
  boot nor a cold model load on checkout. (#414)

**Sandbox scratch disks + private-registry pulls**
- **Scratch / persistent block disk (`SwiftSandbox.spec.scratchDisk`).** Attaches
  one secondary block disk to the sandbox guest — for build caches, dataset
  staging, checkpoints, or a GPU model/weight cache — beyond the ephemeral
  RAM-backed rootfs overlay. `blank` provisions a new, sandbox-owned, sized Block
  PVC (GC'd with the sandbox); `pvcRef` attaches an existing Block PVC that
  persists beyond it. Reuses the v0.4.2 blank-data-disk runtime path (raw
  `--disk`, no filesystem mount) — the workload runs `mkfs`+`mount` itself. New
  `ScratchDiskReady` condition gates boot on the PVC being Bound. (#411)
- **`spec.imagePullSecret` is now wired through the sandbox materialize path**
  (on both `SwiftSandbox` and `SwiftSandboxPool`, and for `spec.model`'s image).
  Resolves the named docker-registry Secret and attaches it to the
  materialize/pool pods so a private-registry rootfs or model image pulls
  correctly; an invalid or missing secret surfaces as `ImagePullSecretInvalid`
  rather than a generic pull failure. (#402)

**Gateway Explorer sandbox catalog**
- **The web console's Explorer replaces the dedicated gateway `SandboxService`.**
  `SwiftSandbox`/`SwiftSandboxPool` are now surfaced through the generic
  ResourceService/Explorer resource catalog (phase/image/node/IP and
  phase/warm/claimed/min columns) instead of a bespoke Connect-RPC service — one
  fewer service to keep in sync as the sandbox CRD surface grows. (#387) The
  v0.11.0 `SandboxService` is removed. (#401)

### Fixed

- **Shared-NVSwitch GPU allocation now couples GPU selection to the Fabric
  Manager partition membership.** Previously the allocator picked GPUs by NUMA
  locality and an FM partition by count *independently* — but the NVSwitch
  fabric only allows NVLink among the GPUs **within** the activated partition
  (NVIDIA WP-12736-002), and FM physical IDs do not follow lspci order, so a
  Tier-2 `hgx-shared` guest could receive one partition's GPUs while a
  *different* partition was activated: no NVLink for that tenant and a fabric
  cross-wired against the next one. The partition is now the unit of
  allocation (`findAndAllocate`, the migration `ReserveOnNode` primitive, and
  the `GPUNodeHasCapacity` pre-flight all select a free partition whose member
  GPUs are all free and hand the guest exactly those members). Proven with a
  hardware-faithful HGX H100 fixture built from the NVIDIA guide's real BDFs,
  Module-ID mapping, and partition table — the mismatch reproduced verbatim on
  the old code.

- **GPU discovery now translates Fabric Manager partition membership from GPU
  Module IDs to device indices.** `gpu-discovery` wrote
  `SwiftGPUNode.status.fabricManager.partitions[].gpuIndices` straight from the
  parsed FM partition listing, treating the values as device indices — but FM
  expresses membership in GPU physical/Module IDs (`nvidia-smi -q` "GPU Module
  Id"), which do not follow lspci order (NVIDIA WP-12736-002). The membership
  the shared-NVSwitch allocator consumes was therefore wrong on real HGX
  baseboards. Discovery now joins the Module IDs from `nvidia-smi -q` with the
  lspci BDF→index order and translates each partition into device-Index space;
  an unmapped ID is dropped (safe under-count, never a wrong-NVLink one) and an
  empty map (no `nvidia-smi`) degrades to identity with a prominent warning.
  HGX deployments must provide `nvidia-smi` to the discovery pod. No effect on
  the PCIe path (no Fabric, identity pass-through).

- **Shared-NVSwitch allocation now enforces the Fabric Manager version match.**
  `SwiftGPUProfile.spec.fabricManager.requiredVersion` (the guest driver
  version) had no consumers; per NVIDIA WP-12736-002 the host Fabric Manager
  version must exactly match the guest driver version in shared mode or the
  partition attach fails — a broken fabric, not a boot failure. The allocator,
  the migration `ReserveOnNode` reserve, and the `GPUNodeHasCapacity` pre-flight
  now reject a version-mismatched node (shared mode only; full mode runs FM in
  the guest), and the `GPUAllocated=NoCapacity` condition names the mismatch
  rather than reporting a bare "no capacity".

- **SwiftGuest GPU allocation now rejects `tier: hgx-full` (Tier 3) instead of
  silently routing it through the QEMU path.** The native SwiftGPU backend
  previously treated `hgx-full` the same as `hgx-shared` (Tier 2, host Fabric
  Manager) and would allocate a Tier 3 guest onto the same QEMU runtime — but
  the in-guest Fabric Manager / NVSwitch-into-guest passthrough isn't wired, so
  an `hgx-full` guest would fail at boot/fabric time rather than at allocation
  (a silent-failure-shaped gap, not a clean rejection). `nativeBackend.Prepare`
  now returns an `UnsupportedTierError` for `hgx-full`, surfaced as
  `GPUAllocated=False` reason `UnsupportedTier` — mirroring the SwiftSandbox
  native GPU path's existing tier gate. `tier: hgx-shared` (Tier 2) is
  unaffected and allocatable on the topology runtime above. (#416)

- **Sandbox kernel build was silently re-using a stale `.config`, shipping a
  non-monolithic kernel.** Buildroot applies `configs/sandbox-linux.config` (plus
  any profile fragment) at its `linux-configure` step and then STAMPS it, so a
  later edit was ignored on incremental builds — the `sandbox` kernel had shipped
  `CONFIG_MODULES=y` even though the config had said `=n` for months. `make build`
  now re-applies the config whenever it changed and prints the built
  `CONFIG_MODULES`. Republished `kernels/sandbox:6.6.13` (now genuinely monolithic)
  and `kernels/gpu-sandbox:6.6.2`. (#415)

- **Sandboxes without `spec.workingDir` panicked on kernel 6.6.10** (regression
  from v0.11.0's cold-path workingDir change). The bridge-initramfs runs `set -u`
  but only assigned `$WORKDIR` when a workingDir was set, so the no-workingDir case
  — the common one — hit `WORKDIR: parameter not set`, killing PID 1 ("Attempted to
  kill init"). Fixed by initialising `WORKDIR=""`; ships as sandbox kernel
  **6.6.11** (bump the `SwiftKernel sandbox` OCI ref). Operators on 6.6.10 should
  move to 6.6.11.

### Removed

- **`SwiftSandboxPool.spec.idleTTL`** — the field was accepted but never honored
  (a no-op). Scale-to-zero on a quiet pool is done via the scale subresource + an
  HPA with `minReplicas: 0` on `minWarm`, which the pool already supports; the
  dead field is removed to stop implying behaviour that didn't exist. Existing
  pools that set it are unaffected (it was ignored before and is pruned now).

---

## [v0.11.0] — 2026-07-13

MicroVM platform expansion. Surfaces **SwiftSandbox / SwiftSandboxPool in the
kubeswift-ui** (a new gateway `SandboxService` read + write plane and live UI
views), adds **image signing** and a **virtio-fs rootfs** option to sandboxes,
honors `spec.workingDir` on cold boot, and dedups warm-pool image pulls. Builds
on the v0.10.0 warm-pool arc; all cluster-validated on the dev fleet.

### Added

**MicroVM UI surfacing (SwiftSandbox / SwiftSandboxPool in the web UI)**
- A new gateway **`SandboxService`** (Connect-RPC): a fleet-fan-out read plane
  (`ListSandboxes` + a `WatchSandboxes` server-stream, `ListSandboxPools`, detail
  RPCs — per-cluster error surfacing + impersonation) and a write plane
  (`CreateSandbox`/`DeleteSandbox`, `CreateSandboxPool`/`DeleteSandboxPool` as the
  signed-in user). The kubeswift-ui gains a live **Sandboxes** view (sandbox +
  warm-pool tables) and a create drawer. The member RBAC role gains
  `sandbox.kubeswift.io` get/list/watch/create/delete.

**Sandbox virtio-fs rootfs**
- **`SwiftSandbox.spec.rootfsMode: block|virtiofs`** (and the same on
  `SwiftSandboxPool`; default `block`). `virtiofs` shares the unpacked OCI rootfs
  tree over virtio-fs (tag `sandboxroot`) instead of a read-only ext4 disk —
  skipping `mkfs.ext4` and the ext4 size floor, and sharing the host page cache.
  Same RO-base + writable tmpfs-overlay semantics as block. swiftletd gains an
  `is_sandbox` discriminator (independent of the block rootfs) so a virtio-fs
  sandbox keeps the sandbox serial-to-file / config-disk behaviour; the config
  disk moves to `/dev/vda` (no block rootfs precedes it).

**Sandbox image signing (verify before boot)**
- **`SwiftSandbox.spec.verifyKeySecretRef`** (and `SwiftSandboxPool.spec.verifyKeySecretRef`)
  — a Secret holding a cosign public key (`cosign.pub`). When set,
  `sandbox-materialize` resolves the image digest and runs `cosign verify
  <repo>@<digest>` **before** materializing a single layer; a missing or invalid
  signature fails the init container, so the sandbox goes `Failed` and never boots
  an unverified rootfs. A pool verifies every warm slot. Mirrors `SwiftImage`'s
  `spec.source.oci.verifyKeySecretRef`, reusing the proven `internal/oci.Verify`.
  Immutable after create; requires a TLS registry (cosign speaks HTTPS only).

### Changed

**Sandbox `spec.workingDir` honored on cold boot**
- The bridge-initramfs ran the workload as `chroot /newroot argv`, which cannot
  set the working directory, so `spec.workingDir` was accepted-but-ignored on the
  cold-boot path (it already worked on warm-pool checkout via the vsock agent).
  The bridge now parses the config-disk CWD and, when set, runs the workload via
  the guest agent's new one-shot mode (`kubeswift-guest-agent --run` — chroot +
  chdir + exec in one process, distroless-safe, foreground so the bridge stays
  PID 1). Ships as sandbox kernel `6.6.10` (initramfs-only; bzImage unchanged).

**Sandbox warm-pool efficiency**
- `sandbox-materialize` now takes a **node-local per-digest lock** around the
  pull+extract step, so concurrent materializes of the same image on the same
  node (warm-pool slots when `minWarm` exceeds the node count, or co-located
  sandboxes) serialize — the first pulls, the rest cache-hit — instead of N
  redundant parallel pulls of the same layers. The digest cache was already
  correct (atomic rename); this removes the wasted bandwidth.

### Docs

- Clarified the networking operations guide. Community contribution — thanks
  [@evrardjp](https://github.com/evrardjp) (#377).

---

## [v0.10.0] — 2026-07-13

Adds **SwiftSandbox warm pools** — a `SwiftSandboxPool` keeps N pre-booted,
workload-less microVMs ready so a `SwiftSandbox` with `spec.poolRef` checks out
in sub-second time instead of paying the ~15s cold materialize + boot. Built for
bursty same-image workloads (CI fan-out, AI-agent step execution). Ships the
feature complete: checkout claims a warm slot and injects the workload over
vsock (consume-and-replenish), the pool scales like a `SwiftGuestPool`, and
warming is image-independent so distroless images can be pooled. All
cluster-validated on the dev cluster.

### Added

**Warm pools (`SwiftSandboxPool`)**
- `SwiftSandboxPool` CRD + controller — maintains a warm buffer of pre-booted
  slots for one image (`minWarm`/`maxWarm`), node-spread one-per-node across
  kernel-nodes. (#363, #364)
- **`SwiftSandbox.spec.poolRef`** — checkout claims a warm slot (CAS re-parent)
  and injects the sandbox's `command`/`args`/`env` over vsock, sub-second; the
  consumed slot is destroyed and the pool replenishes. Falls back to the cold
  path on a miss (no warm slot, or no command). (#365, #366)
- **Scale subresource** — `kubectl scale sboxpool <name> --replicas=N` sets the
  warm buffer and an HPA can target it; scale-down drains excess slots, so an
  HPA with `minReplicas: 0` on the checkout cold-rate drains a quiet pool and
  re-warms on demand. (#370)
- **Image-independent warming** — a warm slot idles in the initramfs
  (`kubeswift.idle=1`) instead of running the image's `sleep`, so distroless
  images (no shell/sleep — the untrusted-code case) can be pooled. Kernel
  `kernels/sandbox:6.6.9`. (#372)
- **Image-env merge** — a checkout's injected workload gets the pool image's
  config env merged with `spec.env` (parity with a cold sandbox), resolved once
  at materialize with no per-checkout pull. (#371)

**Observability**
- `kubeswift_sandbox_checkouts_total{result}` (hit/cold), `kubeswift_sandbox_total{result}`
  (completed/failed), and state gauges `kubeswift_sandboxes` / `kubeswift_sandbox_pools`
  by phase + per-pool `kubeswift_sandbox_pool_{warm,claimed}_replicas`. New
  **Sandboxes & Warm Pools** Grafana dashboard. (#367, #373)
- `swiftctl sandbox logs`/`exec`/`attach` now target a checked-out sandbox's
  claimed slot (via `status.podRef`). (#369)

Operator guide: [`docs/sandbox/warm-pool.md`](docs/sandbox/warm-pool.md). (#368)

---

## [v0.9.0] — 2026-07-12

Adds **SwiftSandbox** — a third boot mode alongside disk boot and kernel
boot: an ephemeral, strongly-isolated microVM that runs an **OCI image as
its root filesystem** (the Firecracker/Kata model: direct-kernel boot, a
read-only ext4 built from the image, a tmpfs overlay, no PVC). Built for CI
runners, AI-agent/code-interpreter execution, serverless compute, and
untrusted code. Cluster-validated end to end — an alpine microVM boots, runs
its workload to a terminal phase, and supports interactive exec/attach over
vsock on both network modes.

### Added

**SwiftSandbox (`sandbox.kubeswift.io`)**
- `SwiftSandbox` CRD + controller — resolves an OCI image, materializes it to
  a node-local ext4 via an init container, and boots it as a direct-kernel
  microVM with a tmpfs overlay root. New `sandbox` SwiftKernel profile
  (Linux 6.6.8 + bridge-initramfs; not bootable as a plain `kernelRef`
  SwiftGuest kernel — it needs the OCI rootfs disk the controller supplies).
  `status.phase` runs `Pending → Materializing → Running →
  Completed`/`Failed`; `kubectl get sbox` short name. (#349)
- **Batch lifecycle** — the workload runs as a supervised child, so its real
  exit code surfaces as `status.exitCode` (`0` → `Completed`, non-zero →
  `Failed`); `spec.timeout` force-terminates a runaway run; `spec.ttl`
  deletes a finished sandbox's record and frees the node's rootfs-cache
  reference. (#353)
- **`spec.command`/`args`/`env`/`workingDir`** delivered to the guest over a
  per-sandbox read-only config disk — never the kernel cmdline, so env stays
  out of `/proc/cmdline` and the host's `ps`/logs. (`workingDir` is accepted
  but not honored in v1 — the workload always starts in `/`.) (#352)
- **Network modes** — `spec.network.mode: restricted` (default: deny-ingress
  plus hardened egress — DNS and the public internet are reachable, but the
  cloud metadata endpoint and RFC1918 cluster-internal ranges are blocked via
  in-pod iptables), `open` (deny-ingress, unrestricted egress), or `none` (no
  network at all). A networked sandbox resolves cluster service names and
  external names alike via injected namespace search domains. (#351, #355)
- **`swiftctl sandbox logs` / `exec` / `attach`** — `logs [-f]` streams the
  workload console; `exec <name> -- cmd [args...]` runs a command inside the
  sandbox's OCI rootfs over a host↔guest vsock channel, with stdout/stderr
  streamed back live, the exit code propagated, `-e KEY=VALUE`/`-w DIR` for
  env/workdir, and `-i`/`-t` for stdin forwarding and an interactive TTY;
  `attach` is shorthand for `exec -it -- /bin/sh` with window-resize
  propagation. (#356, #357, #358, #359, #360)

### Fixed

- **SwiftSandbox status was silently empty.** The sandbox launcher wasn't
  injecting `POD_NAME`/`POD_NAMESPACE`, so swiftletd skipped all status
  reporting; `status.runtime` and `status.network` now surface correctly.
  (#347, #350)
- Broken Cloud Hypervisor upstream link in `README.md`. (#346)

---

## [v0.8.0] — 2026-07-08

Makes multi-cluster **federation near-zero-config**. Registering the hub and its
members, and wiring per-VM
telemetry, now come from Helm values and auto-discovery instead of hand-written
`Cluster` objects and credential Secrets: a `federation.role` (hub / edge) preset,
hub **self-registration**, **edge onboarding** that mints its own join credential,
gateway **Prometheus auto-discovery**, and cert-manager **`ingress.tlsAuto`**.

### Added
- **`federation.role: standalone | hub | edge`** — `hub` presets `gateway.enabled` +
  `ui.enabled` and **self-registers** this cluster as a local fleet member (a
  `Cluster` with `spec.local: true` using the gateway's own in-cluster
  ServiceAccount, no credential Secret). `edge` mints a least-privilege member
  ServiceAccount + a long-lived token Secret + the member-RBAC, and its Helm NOTES
  print the ready-to-apply hub-side `Cluster` + `Secret` (the token is never printed
  by Helm). `standalone` (the default) is unchanged. (#335)
- **Gateway Prometheus auto-discovery** — when `Cluster.spec.prometheusEndpoint` is
  empty, the gateway discovers an in-cluster Prometheus on the member (the
  kube-prometheus-stack `prometheus-operated` Service, or any Service labeled
  `app.kubernetes.io/name=prometheus`, scanned in
  `gateway.prometheusDiscovery.namespaces`) and publishes it to
  `status.prometheusEndpoint` + a `PrometheusEndpointResolved` condition. An explicit
  endpoint always wins. (#335)
- **`ingress.tlsAuto`** — cert-manager TLS on the UI / gateway ingress from a single
  issuer value (`clusterIssuer` or `issuer`), without hand-writing the `tls[]` block
  and the cert-manager annotation. (#332)
- **Chart values reference** — `charts/kubeswift/README.md` documents every Helm
  value (and renders on the OCI registry / Artifact Hub page). (#337)

### Changed
- **`fleet.kubeswift.io` Cluster CRD**: `spec.credentialSecretRef` is now optional,
  and a new `spec.local` field boots the gateway's client from its own in-cluster
  ServiceAccount (the hub self entry). (#335)

### Fixed
- **Hub self-registration telemetry** — the gateway ServiceAccount now has
  `services get,list`, so a self-registered (`spec.local`) hub can discover its own
  in-cluster Prometheus. Previously the self entry reported
  `PrometheusEndpointResolved=DiscoveryError`. (#336)

---

## [v0.7.0] — 2026-07-03

Brings **OCI-registry-native VM artifacts** to KubeSwift. VM snapshots, golden
images, and cold / suspended-state migration can now live in any OCI registry
(Harbor / Zot / a cloud registry) — content-addressed, deduplicated,
cosign-signable, and portable across clusters and out to the edge. KubeSwift is a
registry **client**, never a registry. Headliners: a new `oci` snapshot backend,
golden-image `SwiftImage.spec.source.oci` with a first-party `swiftctl image
publish` producer and cosign **verify-on-pull**, full-state (disk + memory) cold
migration including a **source-independent cross-cluster** path, and
secondary-data-disk snapshots.

### Added

**OCI at-rest VM-disk artifacts (ORAS)**
- **`SwiftSnapshot.spec.backend.type: oci`** — capture / restore / cloneFromSnapshot
  against any OCI registry, alongside the existing `local` / `s3` backends, via a
  new `snapshot-oras` transfer image (embeds `oras-go/v2`). (#295–#299)
- **Provenance signing** — `spec.backend.oci.signingKeySecretRef` cosign-signs the
  pushed artifact; surfaced as `status.oci.signed`. (#300)
- **Golden-image `SwiftImage.spec.source.oci`** — pull a golden VM disk from an OCI
  registry, stored as sparse, zero-skipping, content-addressed **chunks** that
  dedup zero regions and unchanged cross-version blocks. (#301–#303)
- **`swiftctl image publish`** — first-party, client-side producer: chunk a local
  raw/qcow2 golden disk and push it (qcow2 auto-converted via qemu-img; optional
  `--sign-key`). (#324)
- **cosign verify-on-pull** — `SwiftImage.spec.source.oci.verifyKeySecretRef` fails
  the import if the signature is missing/invalid, so no unsigned/tampered disk is
  ever materialized. (#325)
- **Cold / suspended-state migration** — full-state (disk + memory)
  capture-then-terminate plus `cloneFromSnapshot` import; `swiftctl guest
  export/import`. (#304–#309)
- **Source-independent (cross-cluster) full-state clones** — resume from the
  captured launcher-sufficient surface even when the source guest / image / seed
  are gone. (#310–#315)
- **Secondary data-disk snapshots (SI v1.1)** — full-state capture + import of a
  guest's `blank` / `pvcRef` data disks; `swiftctl snapshot export-manifest /
  import-manifest` for cross-cluster object transfer. (#317–#320)
- **Edge registry profile** — per-site Zot mirroring of VM artifacts from a hub via
  `zot sync` (docs + samples). (#316)

### Fixed
- **cloneFromSnapshot reboot ("firmware hang")** — a memory-clone's root disk is now
  a CSI clone of the SOURCE guest's disk (matching grown geometry + real data),
  not the pristine image. The previous filesystem-larger-than-partition mismatch
  dropped the clone into the initramfs on its next reboot. (#323)
- **SwiftRestore** — the clone target no longer inherits a stopped source's
  `runPolicy` (restoring from a stopped source is the natural DR flow); resolves
  the in-guest `RemoveIPC` probe finding. (#315)
- **Nightly cluster E2E** — the snapshot scenario's guest PVC now provisions on a
  CSI (snapshot-capable) StorageClass; the hosted-runner nightly runs the disk-boot
  smoke, with the CSI-snapshot and local memory-snapshot scenarios available via
  `workflow_dispatch` and validated on the dev cluster. (#326)

### Changed
- The `snapshot-oras` transfer core (chunk / push / registry / cosign) is now a
  shared `internal/oci` package used by both the in-cluster `snapshot-oras` Job and
  the client-side `swiftctl image publish`.

---

## [v0.6.1] — 2026-06-30

Project rehomed to the **kubeswift-io** GitHub organization. **No functional
changes** — this release re-points the Go module path, container images, and the
Helm OCI chart to the new org. Old `projectbeskar` URLs and images keep working
(GitHub redirects + the old packages remain), so existing installs are unaffected
until they upgrade.

### Changed
- **Go module**: `github.com/projectbeskar/kubeswift` → `github.com/kubeswift-io/kubeswift`.
- **Images**: `ghcr.io/kubeswift-io/kubeswift/*` (controller-manager, swiftletd,
  kubeswift-gateway, gpu-discovery, snapshot-s3, migration-stunnel,
  kubeswift-dra-driver); the web console at `ghcr.io/kubeswift-io/kubeswift-ui`.
- **Helm chart**: `oci://ghcr.io/kubeswift-io/charts/kubeswift`.
- **Repos**: `github.com/kubeswift-io/{kubeswift,kubeswift-ui}`.

---

## [v0.6.0] — 2026-06-25

Adds the **KubeSwift web console** — a multi-cluster operator UI for the fleet,
served by a new in-cluster **gateway** (`kubeswift-gateway`) that federates the
registered `fleet.kubeswift.io` Cluster members and fans a read / write / telemetry
/ console plane across them **as the signed-in user**. The browser app ships from
the companion **kubeswift-io/kubeswift-ui** repo; this release adds the gateway,
the `kubeswift.v1` proto contract, and the `gateway.enabled` / `ui.enabled` Helm
surface — including end-to-end **OIDC login** (e.g. Keycloak) with per-user
Kubernetes **RBAC impersonation**, exposed behind one ingress. Cluster-validated
end-to-end on the dev hub (login → impersonation → RBAC denial surfaces).

### Added
- **Multi-cluster gateway (`kubeswift-gateway`, `gateway.enabled`)** — a
  Connect / gRPC-Web hub that watches `fleet.kubeswift.io/v1alpha1` Cluster
  objects and serves the UI: ClusterService (fleet inventory), GuestService
  (list / watch / detail / create / start / stop / delete / clone / migrate +
  events), MigrationService (list + watch), TelemetryService (per-VM CPU/mem/net
  from each member's Prometheus, + node metrics), ResourceService (an RBAC-scoped
  generic resource explorer with read + apply / delete), AccessService (an RBAC
  editor — predefined + capability ClusterRoles bound to OIDC users/groups), and a
  WebSocket serial console (exec-pipe). (#258–#288)
- **`fleet.kubeswift.io/v1alpha1` Cluster CRD** — registers a member cluster and
  its credential for the hub to federate. (#259)
- **Gateway OIDC auth (`gateway.authMode=oidc`)** — verifies the browser's
  IdP-issued ID token and impersonates the claim-derived user + groups onto each
  member, so every action authorizes against that member's own Kubernetes RBAC.
  `gateway.oidc.*` Helm values; **`--oidc-ca-file`** (`gateway.oidc.caSecret`)
  trusts a private / self-signed IdP CA for the JWKS fetch. (#278, #289)
- **Web-console Helm surface (`ui.enabled`)** — deploys the kubeswift-ui app
  (nginx); the default `ui.gateway.mode=proxy` reverse-proxies the gateway so the
  browser is single-origin (no CORS, one ingress host, one OIDC redirect).
  `ui.oidc.*` turns on the browser Authorization-Code + PKCE login. (#290)
- **`GetGuestDetail` structured spec + networking view** — Clone reads the
  structured boot source; the drawer's Networking section shows binding / exposed
  ports / Service / egress (`GuestNetwork`). (#286, #288)

### Fixed
- Gateway maps a member RBAC **403 → `PermissionDenied`** (was `Internal`) on the
  no-policy-webhook paths, so the UI can show a clean permission error. (#291)
- `StopGuest` deletes the launcher pod (not just patches `runPolicy`), matching
  `swiftctl` via the shared `internal/actions` primitives. (#267, #275)
- Node-metrics PromQL 400 (`regexp.QuoteMeta` in `=~` matchers). (#283)
- Console exec must name the launcher container. (#270)

### Operator notes
- The console requires **HTTPS** for the OIDC login (PKCE `crypto.subtle` is
  disabled on non-secure remote origins). Expose the UI and the IdP over TLS; for a
  private CA, set `gateway.oidc.caSecret`. Full Keycloak + Kubernetes-RBAC runbook:
  `docs/ui/auth.md`.
- The gateway and UI run on a **hub** cluster only; members just need KubeSwift
  installed (CRDs + controller) and to be registered as `fleet.kubeswift.io`
  Cluster objects.

---

## [v0.5.0] — 2026-06-19

Adds **Model A — namespace-native VM tenancy**: a SwiftGuest in a namespace whose
**primary** network is an OVN-Kubernetes UserDefinedNetwork rides that UDN directly —
holding a **native UDN IP**, cross-node reachable and tenant-isolated. Every pod *and*
VM in the namespace shares one tenant network, with no per-guest `networkRef`.
Cluster-validated end-to-end on a kubeadm OVN-K-primary cluster (boot → native UDN IP
`10.50.0.x` → cross-node ping). Complements the v0.4.6 **Model B** (per-guest
secondary-UDN) path.

### Added
- **Model A — guest on the namespace primary OVN-K UDN (namespace-native tenancy).**
  Detected from the `k8s.ovn.org/primary-user-defined-network` namespace label — no
  SwiftGuest spec change. The launcher datapath (`setup_primary_udn_nic`) bridge-binds
  the pod's `ovn-udn1` interface to the VM's tap so the VM adopts OVN's IP-derived MAC +
  IP (the standard bridge-binding pattern; OVN `port_security` pins them). Because the
  primary UDN is bridged to the guest and the pod's `eth0` is infrastructure-locked,
  swiftletd cannot reach the apiserver — so the **controller derives status**:
  `status.network.primaryIP` from the pod's `k8s.ovn.org/pod-networks` annotation,
  `GuestRunning` from a launcher CH-API-socket readiness probe; swiftletd skips all
  apiserver calls for these guests. Operator guide:
  [`docs/networking/udn-primary-tenancy.md`](docs/networking/udn-primary-tenancy.md);
  sample: [`config/samples/model-a/`](config/samples/model-a/).
  (PRs #252–#256.)

### Limitations
- **Model A guests are offline-only in v1.** Live migration of a primary-UDN guest is
  rejected at admission (the webhook parallels the VFIO gate; `mode: auto` resolves to
  **offline**) because the primary UDN withholds the swiftletd↔swiftletd migration
  channel from the pod (the destination pod's `eth0` is infrastructure-locked, dropping
  pod-to-pod). `kubectl drain` still evacuates Model A guests **offline** (the target
  acquires a fresh UDN IP). **Live migration + snapshot of Model A guests are a v2
  track.** For IP-preserving **live** migration today, use **Model B** (a per-guest
  secondary UDN — [`docs/networking/udn-multi-tenancy.md`](docs/networking/udn-multi-tenancy.md)).

---

## [v0.4.6] — 2026-06-18

Adds **OVN-Kubernetes as a second OVN CNI backend**: a guest's **primary** NIC on an
OVN-Kubernetes `layer2` NAD gets a portable IP with **IP-preserving live migration** —
the same capability the kube-ovn backend already delivers, now on clusters where
OVN-Kubernetes is the primary CNI. Cluster-validated end-to-end on a kubeadm
OVN-K-primary cluster (boot + `mode: live` migration, no `allowIPChange`, IP
preserved). Also hardens controller startup on clusters that lack the CSI
VolumeSnapshot CRDs, and ships a full operator-doc set (cluster setup + per-tenant
multi-tenancy recipes for both OVN substrates).

### Added
- **OVN-Kubernetes as a second OVN CNI backend (OVN-K arc P2).** `ovnKubernetesBackend`
  implements the `ovnBackend` seam for OVN-Kubernetes **layer2 primary-on-NAD** guests:
  it detects `type: ovn-k8s-cni-overlay` + `topology: layer2`, injects the guest MAC
  (the logical-switch-port identity) + an `ipam-claim-reference` into the Multus
  network-selection element, and creates+owns a per-guest `IPAMClaim` to pin the
  primary IP (OVN-K does not auto-create it). The identity mechanism — the `mac`
  field of the selection element — was confirmed on a real OVN-K cluster (a foreign
  MAC requested there comes up reachable cross-node). Live-migration IP preservation
  rides the already-carried-over Multus annotation: OVN-K allows the cross-node claim
  overlap by **default**, so no `migrationJobName`-style marker is needed (simpler
  than kube-ovn). Adds `ipamclaims` RBAC (`get,list,watch,create`; GC via owner-ref
  cascade). The `PrimaryIPPreservedCrossNode()` live-eligibility gate already covers
  it (CNI-agnostic). **Cluster-validated end-to-end** on a real OVN-Kubernetes-primary
  cluster: a RWX+Block disk-boot guest on a `layer2 allowPersistentIPs` NAD booted
  reachable cross-node, then `mode: live`-migrated with **no `allowIPChange`** in
  **2.8 s** with the IP preserved and reachable from a third node. Operator guide:
  [`docs/networking/ovn-kubernetes-install.md`](docs/networking/ovn-kubernetes-install.md).
  UDN-*secondary* networks ride this backend transparently (a generated `layer2` NAD);
  UDN-*primary* multi-tenancy is a separate later phase.

### Changed
- **Internal: pluggable OVN CNI backend seam (OVN-K arc P1).** Lifted the kube-ovn
  primary-on-NAD identity logic behind an internal `ovnBackend` interface
  (`internal/controller/swiftguest/ovn_backend.go`) — `Detect`/`Identity` per
  backend, first-match-wins, "two implementations, not a framework". The two call
  sites (the launcher-pod stamp and the live-migration dst-pod annotations) now
  dispatch through the seam, so additional OVN-based CNIs (OVN-Kubernetes next)
  plug in without touching the controller. **Behavior-preserving** — no
  user-facing change; the shipped kube-ovn IP-preserving live-migration path is
  identical and its tests pass unchanged. Foundation for OVN-Kubernetes support.

### Fixed
- **Controller no longer crash-loops on a cluster without the CSI VolumeSnapshot
  CRDs.** The snapshot controllers `Owns(VolumeSnapshot)`; when the
  external-snapshotter CRDs (`snapshot.storage.k8s.io/v1`) are absent, that watch's
  cache could never sync and the manager exited fatally — so on a bare cluster
  (e.g. one whose CSI driver doesn't bundle them) KubeSwift never started. The
  manager now does a one-time discovery check and **gates those watches** on it,
  logging a clear warning and degrading gracefully: the **core VM runtime**, the
  **local/s3 snapshot backends**, and `cloneStrategy=copy` all keep working; only
  the **CSI VolumeSnapshot backend** and `cloneStrategy=snapshot` are disabled
  until the CRDs are installed. Surfaced by the OVN-K P3 cluster validation (the
  W5 pattern — a bare cluster exposes a hard dependency unit tests can't see).

### Docs
- **OVN-Kubernetes + UDN operator guides.** A kubeadm + OVN-Kubernetes-primary
  cluster setup guide ([`kubeadm-ovn-kubernetes-setup.md`](docs/networking/kubeadm-ovn-kubernetes-setup.md)),
  the OVN-Kubernetes-primary install guide
  ([`ovn-kubernetes-install.md`](docs/networking/ovn-kubernetes-install.md)), and
  per-tenant multi-tenancy recipes for both OVN substrates
  ([`udn-multi-tenancy.md`](docs/networking/udn-multi-tenancy.md) for OVN-K,
  [`kubeovn-multi-tenancy.md`](docs/networking/kubeovn-multi-tenancy.md) for kube-ovn).

---

## [v0.4.5] — 2026-06-17

Closes the **multi-node L2 / kube-ovn primary-on-NAD** arc: a guest whose
**primary** NIC rides a kube-ovn NAD is reachable cross-node on its own IP and
**preserves that IP across a `mode: live` migration with no `allowIPChange`** —
zero-touch, no manual `ovn-nbctl`. Cluster-validated end-to-end on image
`sha-e403f4c`.

### Added

- **kube-ovn primary-on-NAD integration (IP-preserving live migration on a real
  Tier-C OVN L2).** When a SwiftGuest's **primary** interface rides a
  kube-ovn-class NAD (`config.type: kube-ovn`), KubeSwift now programs the guest's
  identity onto the OVN logical-switch port so the guest is reachable on the
  segment, and preserves its IP across a live migration — with **no manual
  `ovn-nbctl`**. Two coupled pieces:
  - **Controller (#239):** stamps `<provider>.kubernetes.io/mac_address: <guest MAC>`
    on the launcher pod (so OVN's per-port ARP responder / L2 delivery target the
    guest's bridged MAC, not the pod NIC's) and, once known,
    `<provider>.kubernetes.io/ip_address: <guest IP>` (a stable static IP across
    pod recreate). The live-migration **destination** pod additionally gets
    the migration-job annotation kube-ovn's IPAM recognises
    (`swiftguest.MigrationJobNameAnnotation`), which makes it skip the conflict
    check so the dst acquires the **same** static IP the source still holds through
    cutover. Reads NADs read-only (new
    `k8s.cni.cncf.io/network-attachment-definitions` get RBAC). No-op for every
    other networking mode (node-local bridge, non-kube-ovn NAD, SR-IOV). Composes
    with `#235`/`#236` (the NAD live-migration carry-through).
  - **Launcher datapath fix (this change):** stamping the guest MAC onto the pod
    NIC means the NIC and the guest's tap share a MAC; enslaving the NIC to `br0`
    makes the kernel add a permanent fdb entry `<guest-mac> -> NIC` that **shadows
    the tap** (the bridge sends the guest's return traffic to the NIC, not the
    guest). `network-init` now re-MACs the NIC to a dummy **before** enslaving it
    (the standard bridge-binding pattern); the OVN port keeps the guest MAC, so OVN
    still delivers the guest's frames `NIC -> br0 -> tap`. A no-op for any NAD whose
    IPAM gives the NIC its own distinct MAC.
  - **Validation.** Cluster-validated **zero-touch end-to-end** on image
    `sha-e403f4c`: a fresh kube-ovn-primary guest auto-stamps its OVN port
    identity and `network-init` auto-re-MACs the pod NIC, so the guest comes up
    reachable cross-node with its IP in `status.network.primaryIP` (no manual
    `ovn-nbctl`); a cross-node `mode: live` migration with **no `allowIPChange`**
    Completed in **~3.2 s** downtime with the IP **preserved and reachable from a
    third node**. The post-merge cluster validation of the **automated** path
    surfaced the NIC-MAC-shadow gap above (the W5 pattern: unit tests verify the
    annotation, only a cluster exercises the bridge fdb) — fixed here. **Both**
    the controller and the launcher image must carry the integration.

### Changed

- **De-experimentalized the multi-node L2 guide (#238).**
  [`docs/networking/multi-node-l2.md`](docs/networking/multi-node-l2.md) now
  carries an honest validation matrix: the primary-on-NAD datapath, offline
  migration, and **live** IP-preservation are cluster-validated on a kube-ovn
  Tier-C L2; the hand-rolled bridge/macvlan mesh validates the datapath and
  offline moves but not live migration.

### Fixed

- **Live migration of a guest on a Multus NAD no longer fails `DstNeverReady`
  (#235).** The migration **destination** pod now inherits the source's
  `k8s.v1.cni.cncf.io/networks` request annotation, so its secondary interface
  (`net1`) is plumbed by Multus before the receiver starts — previously the dst
  pod came up without the NAD attachment and the migration timed out waiting for
  a receiver that could never bind.
- **Bounded send-retry when the destination CH receiver is not yet listening on
  the migration channel (#236).** The source launcher now retries the
  live-migration send for a short bounded window instead of failing the first
  time the dst Cloud Hypervisor receiver has not finished binding the channel —
  closing a cutover race that intermittently failed otherwise-healthy
  migrations.
- **Hardened the primary-on-NAD per-pod dnsmasq for a shared flat multi-node L2
  (#237).** On a NAD that places every guest on one flat L2 segment, the per-pod
  dnsmasq now answers **only its own guest's MAC** (it will not reply to a peer
  guest's DHCP on the shared wire), hands out an **infinite lease**, and carries
  the **overlay MTU** so the guest matches the segment.

---

## [v0.4.4] — 2026-06-16

Feature release. The **in-guest vsock identity agent** — a `cloneFromSnapshot`
clone now regenerates its identity (machine-id / SSH host keys / hostname / MAC)
and re-DHCPs **in place, with no reboot**, sidestepping the Cloud-Hypervisor-v52
clone-reboot firmware hang documented in v0.4.3. Cluster-validated end-to-end.

### Added

- **In-guest identity agent over vsock.** Opt a snapshot **source** in with
  `spec.guestAgent.enabled: true`. The controller attaches a Cloud Hypervisor
  `--vsock` device and (with the `guest-agent` SwiftSeedProfile) delivers a tiny
  static agent binary onto the source's NoCloud seed disk, installed on first
  boot. The agent is captured in the memory snapshot and resumes — alive — in
  every clone. When a clone reaches `GuestRunning`, the SwiftGuest controller
  drives a one-shot regeneration over the host↔guest vsock channel: it
  regenerates the items in `cloneFromSnapshot.regenerate`, sets the per-clone
  guest-visible MAC, and re-DHCPs — all without a reboot. The result is reported
  on the new **`CloneIdentityRegenerated`** status condition (`True`, or `False`
  with reason `GuestAgentUnreachable` when the agent is absent — a loud,
  never-silent fallback), and each clone's own IP lands in
  `status.network.primaryIP` via the restore lease-poller.
  - New `cmd/kubeswift-guest-agent` (static Go AF_VSOCK listener; one
    `regenerate-identity` op; validated argv inputs; primary-interface
    detection), embedded in the swiftletd image and delivered via the seed disk.
  - New `rust/swift-vsock-client` crate (the host-side CONNECT-handshake client)
    + a swiftletd `identity` action namespace driving it.
  - `SwiftGuest.spec.guestAgent.enabled` (opt-in on the source; Linux only).
  - Operator guide:
    [`docs/snapshots/identity-regeneration.md`](docs/snapshots/identity-regeneration.md)
    + [`docs/snapshots/clone-from-snapshot.md`](docs/snapshots/clone-from-snapshot.md).
  - Cluster-validated on **Tier B** (local) memory snapshots — a 2-clone fan-out
    came up with distinct machine-id / hostname / MAC / IP per clone, no reboot.
    **Tier C** (S3) clones inherit the identical agent flow (the snapshot carries
    the agent + vsock device regardless of backend); validate in your environment.

### Notes

- No new container image or chart toggle: the agent ships inside the `swiftletd`
  image and the opt-in is a SwiftGuest field. The `swift.kubeswift.io` CRD gains
  `spec.guestAgent`; `kubectl apply`/`helm upgrade` updates it.
- Windows guests (`osType: windows`) are out of scope for the agent in this
  release (the regeneration mechanics differ); the controller skips the device.

---

## [v0.4.3] — 2026-06-15

Patch release. swiftletd lease-poller fix for cloneFromSnapshot guests, and a
documented Cloud-Hypervisor-v52 limitation surfaced validating the instant-clone
flow.

### Fixed

- **swiftletd: the lease poller no longer gives up on restore/clone guests.** A
  `cloneFromSnapshot` guest boots via CH `--restore` — it RESUMES the source's
  captured RAM (including its already-configured `eth0` lease) and does not
  re-run DHCP on start, so a fresh clone has no lease to discover. The guest only
  re-DHCPs on a later reboot, but the lease poller capped at ~4 min and then
  exited permanently (`lease_poll_timeout`) — so any post-reboot lease landed
  into a dead poller and the IP never reached `status.network.primaryIP`. The
  poll cap is now parameterized: fresh boots keep the ~4 min cap, CH `--restore`
  receivers (`intent.is_restore()`) keep polling for the pod's lifetime so the
  eventual lease is discovered. The poller still terminates immediately on the
  first successful patch. swiftletd-only; no controller/CRD change.

### Known issues

- **A cloneFromSnapshot (memory-snapshot) guest cannot reboot on Cloud
  Hypervisor v52** — rebooting a `--restore`d guest hangs in UEFI firmware (the
  EDK2 S3-resume / AP-init path freezes after `MpInitChangeApLoopCallback`),
  while a *normal* guest reboots through the same point and re-DHCPs in seconds.
  Because the documented clone-identity remedy is "reboot once to regenerate
  identity + re-DHCP", this means memory-snapshot clones currently keep the
  source's guest-visible identity and do not surface their own IP — treat them as
  warm, read-mostly replicas (collision-safe via per-pod network namespaces). It
  is a CH-`--restore`+reboot firmware interaction, not a KubeSwift defect; the
  in-guest vsock identity agent (regenerate without a reboot) is the planned real
  fix. Operator note in
  [`docs/snapshots/clone-from-snapshot.md`](docs/snapshots/clone-from-snapshot.md).

---

## [v0.4.2] — 2026-06-14

Code + chart release. Headline: **blank / raw VM data disks** — the
"give me a sized empty volume for my database" case that previously had no
first-class option — plus five fixes surfaced dogfooding the demo flows.

### Added

- **Blank / raw VM data disks (`spec.dataDiskRefs[]`).** A guest can now attach
  sized, image-less block disks that the guest formats itself — the previously
  missing empty-volume primitive.
  - `dataDiskRefs[].blank: {size, storageClassName, volumeMode}` provisions a
    guest-owned PVC (Block by default), attached to the VM as a raw `--disk`
    (CH `--disk path=/dev/...`); the guest formats and mounts it.
  - `dataDiskRefs[].attachAsDisk` attaches an **existing** Block PVC as a raw VM
    disk (vs the default filesystem-directory mount).
  - The plural `dataDiskRefs[]` is now fully real for VM disks across all three
    kinds — blank, image-backed (`imageRef`), and attached-PVC — and the
    previously dead-code `dataDiskRefs[].imageRef` now works.
  - New `status.dataDisks[]` echo (PVC / volumeMode / devicePath / bound) and a
    **`DataDisksReady`** condition: a guest never boots with a missing data disk
    — it holds in `Scheduling` and names the blocker (Principle #6).
  - Admission validation for `dataDiskRefs[]`: exactly one of
    `imageRef`/`pvcRef`/`blank` per entry, `blank.size > 0`, unique DNS-label
    names, max 8 disks, `attachAsDisk` only with `pvcRef`. Data disks compose
    with GPU.
  - Cluster-validated end to end (blank Block 20Gi: controller → Block PVC → pod
    `volumeDevice` → CH `--disk path=/dev/...` → guest `vdc 20G`; PVC GC'd with
    the guest).
  - Operator guide `docs/api/data-disks.md` + blank-disk sample. **Mount data
    disks by UUID/LABEL** — the in-guest `/dev/vdX` letter is not stable.

### Fixed

- **Snapshot Tier A could capture an empty disk (unbootable restore).** The
  CSI VolumeSnapshot (Tier A) path snapshotted the source guest's root PVC as
  soon as it was `Bound`, but the per-guest rootclone Job writes `image.raw`
  into the PVC *after* it binds — a snapshot taken alongside a fresh source
  guest captured an empty disk, and the restore cloned that empty snapshot.
  The snapshot now gates on the source guest being `Running` or `Stopped`
  (disk populated); Tier B/S3 were unaffected.
- **`swiftctl ssh <guest> -- <command>` now runs a remote command
  non-interactively.** `ssh` was interactive-only and rejected extra args
  (`accepts 1 arg(s)`); it now mirrors `ssh host <cmd>` / `kubectl exec pod --
  <cmd>` — no TTY required, streams stdout/stderr, propagates the remote exit
  code. The bare interactive form is unchanged.
- **The standalone `config/dra-driver/` manifest pinned `:latest`** (a tag the
  registry never publishes), so `kubectl apply` always `ImagePullBackOff`'d —
  and applying it over a Helm install with `dra.enabled=true` clobbered the
  chart's working version-managed image, leaving the DRA driver down and GPU
  ResourceClaims unallocatable. The manifest image is now release-pinned
  (`:v0.4.2`); prefer the chart's `dra.enabled`, which manages the version.

### Changed

- **Performance: the rootclone Job is co-located with the launcher's node.** For
  a node-pinned guest (cloneFromSnapshot with a node-local snapshot,
  `spec.nodeName`, GPU, or migration), an unpinned rootclone Job could populate
  the RWO root PVC on a different node, forcing a ~26s Longhorn detach/reattach
  bounce onto the launcher's node. The Job now carries the launcher's
  `kubernetes.io/hostname` nodeSelector when the target node is known; unpinned
  guests are unchanged.
- Docs: warned against pointing an image-backed data disk at a bootable OS image
  (its partition/FS UUIDs collide with the root disk and corrupt the boot) — now
  superseded by blank disks for the empty-volume case — and corrected the
  data-disk device letter (`/dev/vdc` on disk boot, `/dev/vdb` only on kernel
  boot).

---

## [v0.4.1] — 2026-06-13

Chart-only patch. Identical code and images to v0.4.0 (rebuilt as `v0.4.1`); the
fix is in the Helm chart.

### Fixed
- **The DRA GPU driver is now packaged in the Helm chart.** v0.4.0 shipped the
  DRA GPU allocation backend (`SwiftGuest.spec.gpuResourceClaim`) but its
  reference driver (`kubeswift-dra-driver`) was only available as standalone
  manifests under `config/dra-driver/` — a Helm install could not deploy it, so
  the DRA backend was unreachable on chart-based installs. The chart now ships
  the DRA driver behind a new **`dra.enabled`** toggle (default `false`): the
  DaemonSet on `kubeswift.io/gpu-node=true` nodes, its RBAC, and the
  `kubeswift-vfio-gpu` DeviceClass (`dra.deviceClass.create`, default `true`).
  `dra` is **independent of `gpuDiscovery`** — the DRA driver does its own GPU
  discovery (it publishes ResourceSlices), so a DRA-only cluster runs
  `dra.enabled=true` with `gpuDiscovery.enabled=false`. Enable with
  `--set dra.enabled=true`. The standalone `config/dra-driver/` manifests remain
  for kustomize installs.

---

## [v0.4.0] — 2026-06-13

Everything since v0.3.1 (PRs #198–#223): three feature arcs — **service
exposure**, **DRA GPU allocation**, and an **observability program** — plus the
in-cluster validation of Windows guests and the project's relicensing for
open source. All additive and backward-compatible (`v1alpha1`); no breaking
changes. Each arc shipped with on-cluster validation walkthroughs.

### Highlights

- **Service exposure — VMs as first-class Kubernetes Services (S0–S4).** One
  CNI-agnostic primitive (an in-pod DNAT `podIP:port → vmIP:targetPort`) makes a
  guest port a normal Kubernetes Endpoint, so the whole north-south ecosystem
  composes on top.
  - `SwiftGuest.spec.network.ports[]` exposes guest ports; `expose:
    ClusterIP|NodePort|LoadBalancer` mints one Service per guest with honest
    readiness (the endpoint joins only once the in-guest service answers).
  - `SwiftGuestPool.spec.service` fronts all replicas with **one load-balanced
    Service**; endpoints follow readiness and scale churn (the pool's `scale`
    subresource is the HPA seam). Optional headless variant for sharded serving.
  - A **VM→cluster egress reachability probe** surfaces as `status.network.egress`
    + the `EgressReady` condition — a notorious silent failure made
    `kubectl`-visible.
  - `serviceAnnotations` + `loadBalancerClass` passthrough drive the ecosystem
    (MetalLB, Gateway API, Tailscale, Istio, Linkerd) — recipes in
    `docs/networking/ecosystem-integrations.md`.
- **DRA GPU allocation (Phases 1–2).** GPU passthrough now has **two allocation
  backends** behind one runtime: the native SwiftGPU model (default) and
  Kubernetes **Dynamic Resource Allocation** via `spec.gpuResourceClaim`
  (XOR `gpuProfileRef`). Ships a pluggable two-phase `gpualloc.Backend`, a
  KubeSwift reference DRA driver (`gpu.kubeswift.io`), and the full
  ResourceSlice → scheduler → CDI-env hand-off → CH `--device` chain —
  cluster-validated end-to-end on a real GPU. NVIDIA `k8s-dra-driver-gpu` adapter
  and IMEX/MIG remain hardware-gated.
- **Observability program (O1–O4).** A complete operator observability surface:
  provisioning-native **Grafana dashboards** (six-dashboard taxonomy) gated behind
  a Helm `monitoring.*` block (ServiceMonitor + dashboards), a cache-backed
  **CR-state metrics collector** (11 gauge families), gap-fill counters
  (GPU alloc/release, drain evacuation, image-import outcome, migration
  failure-reason), and a warning-biased **PrometheusRule** starter pack + alerts
  runbook. Fixed two latent metric defects (restart-drift gauge, unbounded label).
- **Windows guests — cluster-validated.** `osType: windows` (introduced in
  v0.3.0) is now validated in-cluster end-to-end: import → CH v52 disk boot
  (`kvm_hyperv=on`, `image_type=raw`) → cloudbase-init over the NoCloud seed →
  Running with DHCP + RDP. Version-aware image-prep tooling for Windows Server
  2022/2025.

### Added

- `SwiftGuest.spec.network` (binding `nat`/`bridge`, `ports[]`,
  `serviceAnnotations`, `loadBalancerClass`); `SwiftGuestPool.spec.service`;
  `status.network.{egress,exposedPorts,serviceRef}` + `PortsProgrammed` /
  `ServiceReady` / `EgressReady` conditions; `SERVICE` / `EGRESS` printcolumns.
- `SwiftGuest.spec.gpuResourceClaim` (DRA backend) + the `kubeswift-dra-driver`
  image.
- Rich `kubectl get` printcolumns for SwiftGuest / SwiftImage / SwiftSeedProfile.
- Helm `monitoring.*` values (ServiceMonitor + dashboard ConfigMaps), shipped
  Grafana dashboards under `config/grafana/`, and a starter `PrometheusRule` pack.
- Operator docs: service exposure + ecosystem integrations, DRA GPU guide,
  fast-VM snapshots/clones guide, live-migratable guests guide, a visual
  architecture reference (Mermaid), and apply-ready samples for all of the above.

### Changed

- **Relicensed to AGPL-3.0** and prepared the repository for open source
  (README rewrite, CRD hygiene, internal development-process docs pruned, dead
  links scrubbed, AI-dev tooling kept local).
- Documentation accuracy pass: corrected the Cloud-Hypervisor-vs-QEMU framing
  (CH is the hypervisor for nearly every workload — Linux **and Windows**, disk
  and kernel boot, PCIe GPU, snapshots, live migration; QEMU is the secondary
  runtime only for HGX SXM multi-GPU topologies), resolved the "Linux only"
  self-contradiction (host x86_64+KVM vs the Linux+Windows guest matrix), and
  brought the CRD reference to all 12 CRDs incl. `spec.osType`. The shipped
  Cloud Hypervisor is **v52.0**.

### Fixed

- DRA cluster-e2e: intent `devices: null` serde, the kubeletplugin socket
  directory, and a hotfix-removal regression.
- Observability: `kubeswift_guest_running_total` restart-drift (now emitted from
  cluster state) and the `vm_failures_total` free-text label (now a bounded
  reason).

---

## [v0.3.1] — 2026-06-10

Patch release. Content is identical to v0.3.0 (images rebuilt as `v0.3.1`);
the fix is in the Helm chart.

### Fixed
- **Helm chart: `migration.mtls.enabled=true` installs were broken** — the
  chart never set `KUBESWIFT_MIGRATION_STUNNEL_IMAGE`, so the controller's
  mTLS sidecar injection fell back to the code default `:latest`, a tag that
  does not exist in the registry. Every launcher pod stuck `1/2
  ImagePullBackOff` (the VM ran, but the guest never reached
  `phase=Running`). The chart now sets the env via a new
  `migrationStunnel.image` values block, mirroring the `snapshotS3` pattern
  (tag defaults to `v<appVersion>`). Found dogfooding the released 0.3.0
  chart — the first helm-path install with mTLS enabled; the kustomize dev
  deploy sets the env via the Makefile, which masked the gap. Installs with
  `migration.mtls.enabled=false` (the default) were unaffected.

---

## [v0.3.0] — 2026-06-09

Consolidates everything since v0.1.0 (the v0.2.0-rc.1 tag from April was never
promoted and is superseded by this release). Roughly 500 commits across six
major feature arcs, each shipped with on-cluster validation walkthroughs.

### Highlights

- **VM snapshots, end to end (Phases 0–6)** — disk-only CSI snapshots,
  local memory snapshots, S3/object-storage export with zstd compression,
  boot-as-clone (`spec.cloneFromSnapshot`), cron scheduling with keep-N
  retention, Prometheus metrics + Grafana dashboard.
- **Live migration, end to end (Phases 1–5)** — offline migration for any
  guest, live migration with sub-3s observed downtime, mTLS-secured
  migration transport, `kubectl drain` integration, offline GPU
  evacuation, metrics + retention.
- **Windows guest support v1** — `osType: windows` on Cloud Hypervisor.
- **vhost-user devices** — virtiofs shared filesystems, vhost-user-net /
  -blk / generic devices (operator-provided backends).
- **Multi-node L2 networking foundation** — primary-on-NAD (experimental)
  and a corrected migration IP-preservation gate; multi-NIC support
  actually works now (three latent bugs fixed; smoke test passes 5/5).
- **Cloud Hypervisor v51.1 → v52.0** — fixes the Windows viostor bugcheck,
  resets guests in place on reboot, unlocks core-scheduling and
  restore/snapshot improvements.

### Added

**Snapshots & restore (`snapshot.kubeswift.io`)**
- SwiftSnapshot + SwiftRestore CRDs and controllers: Tier A CSI
  VolumeSnapshot (disk-only), Tier B local hostPath memory snapshots
  (CH pause/snapshot/resume), Tier C S3-compatible export/import
  (MinIO/AWS/RGW) with checksummed manifests and zstd-compressed memory
  ranges (`spec.backend.s3.compression`).
- `SwiftGuest.spec.cloneFromSnapshot`: boot N guests as clones of one
  memory snapshot (pool-templatable; per-clone hypervisor MAC; CH v52
  auto-resume + on-demand/userfaultfd memory restore).
- `SwiftImage.spec.cloneStrategy: copy|snapshot` for ≥3× faster pool scaling.
- SwiftSnapshotSchedule CRD: cron-created snapshots with `keepLast` pruning,
  reference-aware GC, `spec.ttl`, `spec.deletionPolicy: Delete|Retain` with
  prefix-scoped S3 purge.
- `swiftctl snapshot|restore|schedule` command groups; snapshot metrics,
  byte gauges, Grafana dashboard (`config/grafana/kubeswift-snapshots.json`).

**Live migration (`migration.kubeswift.io`)**
- SwiftMigration CRD + controller: offline mode (direct PVC reuse,
  ~25–70s downtime depending on CSI driver) and live mode (CH pre-copy,
  ~2–3s observed downtime, kernel-boot and RWX+Block disk-boot).
- mTLS migration transport: per-node cert-manager-issued identities,
  SAN-pinned stunnel sidecars (~1% overhead); plaintext path requires an
  explicit unsafe acknowledgement.
- `kubectl drain` integration: eviction webhook + drain controller
  auto-migrate guests per `spec.migration.drainPolicy`
  (Migrate|LiveMigrate|Block); universal per-guest `maxUnavailable: 0`
  PDB as the hard floor; VFIO/GPU guests evacuate offline via the GPU
  release-and-reallocate primitive (reserve-before-stop atomicity).
- Auto mode resolution (live when eligible, else offline), per-guest
  `spec.migration.enabled` pinning, `spec.allowIPChange` opt-in,
  `spec.timeout` (default 30m) and `spec.ttl` retention,
  `status.observedDowntime` / `status.transferProgress` /
  `status.observedTransferDuration`, typed `FailureReasonCode` taxonomy,
  migration metrics + Grafana dashboard.
- `swiftctl migrate` (with `--check` read-only preflight: target
  readiness/capacity, IP preservation, mode resolution, NFD-based CPU
  feature comparison) and `swiftctl migration list|describe|cancel`.

**Windows guests**
- `osType: windows` on SwiftImage/SwiftGuest: CH disk boot with
  `kvm_hyperv=on`, unprivileged import path, cloudbase-init provisioning
  over the existing NoCloud seed, image-prep tooling
  (`tools/windows-image-prep/`) producing virtio-ready images with
  headless BCD (EMS/SAC serial console).

**vhost-user devices**
- `SwiftGuest.spec.filesystems[]`: virtiofs shared filesystems (hostPath
  or PVC source, readOnly enforcement); swiftletd spawns virtiofsd
  (`--sandbox none`, no added capabilities) — full datapath
  cluster-validated.
- `GuestInterface.type: vhost-user` (+ `socket`, `mac`): virtio-net via an
  operator-provided DPDK/OVS backend.
- `SwiftGuest.spec.vhostUserDevices[]`: vhost-user-blk (SPDK-style) and
  generic vhost-user devices (`--generic-vhost-user`).
- Migration gate: guests with node-local virtio backends are offline-only
  (mirrors VFIO; auto resolves to offline).

**Networking**
- Multi-NIC: secondary interfaces via Multus NADs, SR-IOV VFIO NIC
  passthrough, mixed bridge+sriov guests.
- Multi-node L2 foundation: `GuestInterface.primary` lets the primary NIC
  ride a multi-node NAD (IP-preserving migration); corrected
  IP-preservation gate keyed on the primary interface; primary-on-NAD
  launcher runtime (EXPERIMENTAL — datapath pending validation on a
  multi-node L2 cluster); operator docs
  (`docs/networking/multi-node-l2.md`).
- Networking operations guide, OVN-Kubernetes integration guide, ESXi/
  Proxmox concept mapping.

**Storage**
- `spec.storage` on SwiftGuestClass/SwiftGuest: accessMode / volumeMode /
  storageClassName selection with per-field merge; RWX+Block is the
  live-migration-capable combination (RWX+Filesystem rejected at
  admission); full Block-mode runtime path (volumeDevices end to end);
  `cloneStrategy: snapshot` works across volume modes
  (allow-volume-mode-change on the clone seed).

**GPU**
- SwiftGPU Phases 1–3: SwiftGPUProfile/SwiftGPUNode CRDs, allocation
  controller (NUMA-aware, FM partitions, finalizer dealloc), QEMU path
  (Q35/OVMF/QMP) for HGX tiers, GPU discovery DaemonSet (multi-vendor,
  60s cycle), Tier 1 PCIe passthrough validated on hardware (GTX 1080),
  IOMMU-group peer auto-binding, `vfioReady` + capacity pre-flights.

**Fleet & lifecycle**
- SwiftGuestPool: ReplicaSet-style fleets with rolling updates
  (maxUnavailable/maxSurge), topology spread, PVC-per-replica, scale
  subresource, node pre-assignment for snapshot clones.
- `dataDiskRef`/`dataDiskRefs` secondary disks (/dev/vdb) on all boot paths.
- Per-class vCPU core-scheduling (`SwiftGuestClass.spec.coreScheduling:
  off|vm|vcpu`) — SMT side-channel mitigation without disabling SMT.

**Operability & docs**
- GitOps: FluxCD reference repository (`examples/gitops-flux/`, three-layer
  model) + `docs/gitops/` operator docs.
- `swiftctl` grew `ssh`, `describe`, `logs`, robust pod resolution across
  migrations, and the command groups above.
- Controller-driven per-namespace swiftletd RBAC (no manual RoleBinding);
  `make deploy-with-webhook` / `deploy-with-webhook-and-mtls` targets;
  e2e suites wired into CI on path-touch triggers; THREAT-MODEL.md.

### Changed
- **Cloud Hypervisor v51.1 → v52.0** (platform-wide; Linux regression
  passed): fixes Windows viostor `0xD1` bugcheck; guests now **reset in
  place on reboot** (the launcher pod and CH survive — reboots no longer
  churn pods or trigger runPolicy); `CLOUDHV.fd` firmware unchanged
  (`ch-13b4963ec4`). CLOUDHV.fd replaced rust-hypervisor-firmware
  earlier in the cycle (all modern distros bootable; Ubuntu Noble is the
  primary guest OS).
- Guest RAM is now mapped `shared=on` (memfd MAP_SHARED): halves the
  launcher's guest-memory footprint and fixes memory-snapshot OOMs; the
  standard backing for snapshot/migration-capable guests.
- SwiftGuestClass default memory raised to 4Gi; launcher memory limits
  include a 512MiB overhead allowance.
- Root-disk import pipeline: qemu-img resize + sgdisk -e (GPT fix);
  cloud-init growpart expands on first boot.
- `status.observedPauseWindow` renamed to `status.observedTransferDuration`
  (it measures the full transfer RPC, not the vCPU pause).

### Fixed
- **Multi-NIC was silently broken end to end** (three stacked latent bugs):
  the network-init container had no runtime-intent mount (its multi-NIC
  path was unreachable), the launcher image lacked python3 (the intent
  parser), and vhost-user NICs tripped the NIC loop. All fixed; the
  long-flaky multi-nic smoke scenario now passes (smoke suite 5/5).
- **Tier A restore data loss** (silent fresh-boot instead of restored
  disk): `EnsureRootDiskClone` ordering fixed; regression-tested.
- Migration terminal-state handling: per-operation webhook discipline
  (finalizer traps, reconcile storms), chain-migration source-pod
  identity (`status.sourcePodRef`), offline-after-live pod-name trap,
  false-success on destination boot failure, downtime metrics anchored
  on real cutover timestamps.
- vswiftimage webhook: finalizer-removal trap on deletion AND
  pointer-identity spec comparison (metadata-only edits on Ready images
  were falsely rejected) — both fixed with content equality.
- swiftletd: lease poller survives transient RBAC/API failures; stale
  socket cleanup before CH spawn; receiver-mode GuestRunning reporting.
- S3 snapshot upload resume verifies sha256 (not just size); upload Job
  runs with the permissions the root-owned capture artifacts require.
- GPU walkthrough fixes: allocation re-stamp race, premature
  scheduling-atomicity check, reservation leak on guest-delete-mid-migration.
- `swiftctl debug` /proc scan anchors on argv[0] (no self-match);
  numerous gpu-init/container hardening fixes (sysfs shadowing, explicit
  interpreters, ASCII-only scripts).

### Known limitations (v0.3.0-rc.1)
- **Primary-on-NAD runtime is EXPERIMENTAL**: the launcher datapath is
  implemented but unvalidated (the dev cluster has no working multi-node
  L2); validate on an OVN-Kubernetes cluster before relying on
  IP-preserving migration.
- **vhost-user-net/-blk/generic datapaths are asset-gated**: CH wiring is
  cluster-validated, but line-rate operation needs operator-provided
  DPDK/SPDK backends (none on dev infra). virtiofs is fully validated.
- **SR-IOV NIC passthrough and Tier 2/3 HGX GPU support** are code-complete
  but hardware-unvalidated (no SR-IOV NICs / HGX systems available).
- **Cross-node GPU migration destination boot** is not hardware-validated
  (single GPU node); the release/reserve choreography is.
- **Windows in-cluster cloudbase-init provisioning** is untested (no
  Windows license on the dev cluster); every other Windows layer is
  validated.
- All API groups remain **v1alpha1**.

---

## [v0.1.0] — SwiftKernel + Networking (March 2026)

### Added

- SwiftImage import: HTTP source, qcow2-to-raw conversion, GRUB serial console patching
- SwiftGuest lifecycle: launcher pod creation, VM boot, status reporting via pod annotations
- Networking: tap+bridge+dnsmasq, guest IP discovery, status.network.primaryIP
- swiftctl CLI: console, start, stop, restart, debug, ssh, describe, logs
- SwiftSeedProfile: NoCloud cloud-init for user-data, SSH keys, network config
- RunPolicy: Running, Stopped, RestartOnFailure, Always with exponential backoff
- Observability: Prometheus metrics (boot time, running count, failure count, import time)
- SwiftKernel: per-node OCI artifact pull, kernel boot path (bzImage + initramfs)
- faas-minimal kernel profile: Linux 6.6.44 + BusyBox musl via buildroot
- SwiftKernel networking: DHCP IP via virtio-net on kernel boot guests
- Smoke test: end-to-end boot verification

### Known Issues

See the per-release notes below for known issues and their resolutions.
