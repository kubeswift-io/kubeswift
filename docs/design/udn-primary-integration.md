# Design: Model A — guest on the pod's primary OVN-Kubernetes UserDefinedNetwork

> Staff-architect design doc. Status: **APPROVED — implementation arc, target v0.5.0.**
> Builds on the spike `docs/design/udn-primary-spike.md` (gitignored) and the shipped
> OVN-Kubernetes backend (v0.4.6, PRs #242–#249). Cluster: ntx (kubeadm + OVN-K
> primary, identity webhook off), staged namespace `model-a`.

## 1. Goal

Let a SwiftGuest ride its namespace's **primary** OVN-Kubernetes UserDefinedNetwork
(UDN) — interface `ovn-udn1`, the pod's primary network — so a **tenant namespace**
(all its pods *and* VMs) shares one isolated, IP-preserving-migratable network. This
is **Model A** (namespace-native tenancy); **Model B** (per-tenant *secondary* UDN,
shipped v0.4.6 zero-code) already covers per-tenant *guest* isolation.

**Why Model A now (v0.5.0):** Model B isolates the guest's L2 but leaves the launcher
pod's eth0 on the shared cluster network. Model A puts the whole namespace —
KubeSwift VMs **and** ordinary pods — on the tenant's primary network with native UDN
network-policy. The use case is **namespace-as-tenant with mixed pod+VM workloads**.

## 2. What the cluster told us (de-risks the design)

Validated on ntx (model-a, primary UDN `10.50.0.0/16`):
- A pod in a primary-UDN namespace gets **`ovn-udn1`** as primary (default route via
  the UDN gateway); **`eth0` stays on the cluster default** (10.244.x).
- **Full egress** from the UDN: internet (0% loss to 8.8.8.8) **and** cluster DNS both
  work; a SwiftImage import ran end-to-end in the namespace. → **Model A needs no
  special egress handling**; image imports / Jobs / the swiftletd→apiserver control
  path all work, the last over `eth0`.
- Primary UDN requires OVN-K **`enableOvnKubeIdentity=false`** on release-1.2 (its
  pod-admission webhook rejects the primary-UDN pod annotation). This is a **cluster
  prerequisite**, documented for operators — not KubeSwift's concern.

## 3. The datapath

Today (Model B / primary-on-NAD), the guest's L2 rides a **secondary** Multus NIC
(`net1`); `network-init::setup_primary_nad_nic` flushes net1's IP, re-MACs net1, and
bridges `net1 + tap0 → br0`; eth0 is never enslaved. (See
`docs/networking/ovn-kubernetes-install.md`.)

Model A is the **same datapath with a different source NIC**: the guest rides
**`ovn-udn1`** (the pod's primary, created by OVN-K from the namespace label) instead
of `net1`. The pod has **both** `eth0` (cluster default, control path) and `ovn-udn1`
(the tenant UDN); the guest bridges `ovn-udn1 → br0 → tap0`:

```
pod NICs:  eth0 (10.244.x, cluster default — swiftletd→apiserver, image pulls)
           ovn-udn1 (10.50.x, tenant UDN)  ──flush IP, re-MAC, enslave──┐
                                                                         ▼
                                              br0 ── tap0 ── guest eth0 (10.50.x via dnsmasq)
```

The load-bearing invariants carry over unchanged: **eth0 not enslaved**, the **0a:
re-MAC** before enslaving (OVN-K stamps the guest MAC on the LSP just as kube-ovn
does — verify in PR 2, harmless if not), the **per-pod dnsmasq** fixed-lease, the
lease-poller IP discovery.

## 4. Design decisions

**D1 — Detection is namespace-label-driven (implicit), controller-side.** A guest
whose launcher pod lands in a namespace labeled
`k8s.ovn.org/primary-user-defined-network` **and** whose primary interface has **no
`networkRef`** rides the primary UDN. This matches the OVN-K platform model (every
pod in such a namespace rides the UDN; the namespace *is* the opt-in) and the spike's
"no new spec field" recommendation. The controller must **Get the Namespace** to read
the label (new — today it reads no Namespace object; needs `namespaces get,list,watch`
RBAC). **No new SwiftGuest API field.** (Alternative considered: an explicit
`spec.interfaces[].primaryUDN` bool — rejected as redundant with the namespace label
and out of step with how OVN-K models primary UDN; revisit only if operators need a
guest to *opt out* of an enclosing primary UDN, which OVN-K itself does not allow.)

**D2 — The UDN interface is `ovn-udn1`.** OVN-K's deterministic name for the
namespace primary UDN (cluster-confirmed). The launcher **detects it by name** with a
fallback: the primary-UDN NIC is "the non-`eth0`, non-`net*` interface carrying the
UDN subnet." PR 2 hardcodes `ovn-udn1` and logs if absent; the detectable fallback is
a hardening follow-up.

**D3 — A new launcher path `setup_primary_udn_nic`** (clone of `setup_primary_nad_nic`,
target `ovn-udn1` not `net1`). Dispatched by a new `network-init.sh` branch
(`primary=1 && primaryUDNInterface set`). Keeps flush-IP + re-MAC + eth0-not-enslaved
+ helper-IP + `primary-nad.env` (so the existing dnsmasq/lease path is reused
verbatim — `launcher-entrypoint.sh` is interface-agnostic, keys on `NAD_BRIDGE`).

**D4 — Migration eligibility extends the gate caller-side.** Today
`SwiftGuest.PrimaryIPPreservedCrossNode()` is pure spec (`p.NetworkRef != nil`). For
Model A the primary is portable but has no `networkRef`. The migration webhook
(`ValidateCreate`) and controller already hold a client, so they compute
`portable := g.PrimaryIPPreservedCrossNode() || (primaryHasNoNetworkRef(g) &&
namespaceHasPrimaryUDN(ctx, c, g.Namespace))`. The pure method stays for the
secondary-NAD case; Model A is added at the (client-bearing) call sites. (No status
field — the webhook runs at CREATE, before status.)

**D5 — IP preservation rides OVN-K's default cross-node claim overlap.** The
live-migration **dst** pod is created in the **same namespace** → OVN-K gives it the
same primary UDN automatically, and (as for the secondary-UDN path) OVN-K allows the
cross-node IP overlap by default — **no `migrationJobName` marker, no IPAMClaim we
own** (the primary UDN's IPAM is OVN-K-managed). PR 3 verifies the IP follows; if
OVN-K does *not* overlap primary-UDN IPs by default, fall back to an `IPAMClaim` on
the UDN (the spike's persistent-IPAM path) — a contained PR-3 contingency.

## 5. The six seams (grounded in current code)

| # | Seam | File | Change |
|---|---|---|---|
| 1 | Detect | `internal/controller/swiftguest/` (new helper + `controller.go`) | Get the Namespace; `namespaceHasPrimaryUDN(label) && primaryHasNoNetworkRef` → Model A. RBAC `namespaces`. |
| 2 | Intent | `internal/runtimeintent/types.go` + Rust `swiftletd` intent | `NICIntent.PrimaryUDNInterface string` (omitempty); Rust deserialize tolerant (serde default — the null-vs-missing lesson). |
| 3 | Resolver | `internal/resolved/types.go::GetNICs` | when Model A, set `PrimaryUDNInterface="ovn-udn1"` on the primary NIC (no `MultusInterface`). |
| 4 | Pod builder | `internal/controller/swiftguest/multus.go`, `pod.go` | do **not** add the primary to `k8s.v1.cni.cncf.io/networks` (it's namespace-bound, not annotation-bound). `buildMultusEntries` already skips no-`networkRef` ifaces — assert + document. |
| 5 | Launcher | `rust/swiftletd/scripts/network-init.sh` | new branch (line ~313) + `setup_primary_udn_nic`. |
| 6 | Migration | `api/swift/v1alpha1/swiftguest_types.go` callers; `internal/webhook/swiftmigration/`, `internal/controller/swiftmigration/` | D4 eligibility; D5 dst inherits the namespace. |

## 6. Phasing (each PR independently testable in `model-a`)

**PR 1 — Detection + intent plumbing (controller-side, no datapath yet).** Seams
1–4: Namespace-label detect, `NICIntent.PrimaryUDNInterface` (Go + Rust-tolerant),
resolver sets it, pod builder skips the primary in the Multus annotation; `namespaces`
RBAC. **Test:** a SwiftGuest in `model-a` → its `*-runtime-intent` ConfigMap carries
`primaryUDNInterface: ovn-udn1` and the pod has **no** primary entry in the Multus
annotation; the guest still boots (on the node-local path — datapath is PR 2). Unit +
envtest for the namespace-label detection.

**PR 2 — Launcher datapath (`setup_primary_udn_nic`) — THE MARQUEE.** Seam 5. **Test
in `model-a`:** a guest boots with `status.network.primaryIP` on `10.50.x`, reachable
cross-node from another `model-a` pod (probe → guest, 0% loss). Verify the re-MAC
(`bridge fdb show` has no permanent `<guest-mac> → ovn-udn1`) and eth0 untouched.

**PR 3 — Live-migration eligibility + IP preservation.** Seam 6 (D4 + D5). **Test in
`model-a`:** `swiftctl migrate <guest> --to <node>` accepted as `mode: live` **without
`allowIPChange`**; Completed, IP preserved, reachable from a third node. Verify D5
(default overlap) vs the IPAMClaim contingency.

**PR 4 — Webhook validation + operator docs + sample + CHANGELOG.** Admission:
reject a Model A guest whose namespace primary UDN is unsupported (non-layer2 /
non-OVN-K); accept valid. `docs/networking/udn-primary-tenancy.md` recipe +
`config/samples/multi-node-l2/` sample + the `enableOvnKubeIdentity` operator note.
**Test:** webhook accept/reject; docs render. Cut **v0.5.0** after PR 4.

Each PR: `go build/test`, `make generate` + chart sync if CRD/RBAC touched, `gofmt -s`
+ `cargo fmt`, then the cluster test above on `model-a` (mini-walkthrough per phase —
the project's lesson that only the cluster catches the W5-class gaps).

## 7. Risks / open questions

1. **OVN-K stamps the guest MAC on the primary-UDN LSP?** (drives the re-MAC need) —
   PR 2 checks; harmless if not (re-MAC is a no-op when the NIC MAC ≠ guest MAC).
2. **`ovn-udn1` name stability** (D2) — PR 2 hardcodes + logs; detectable fallback is
   a follow-up.
3. **Primary-UDN cross-node IP overlap by default?** (D5) — PR 3 verifies; IPAMClaim
   contingency contained.
4. **`namespaces get` RBAC** broadens the controller's reads — minimal, read-only,
   standard.
5. **Model B coexistence:** a guest with a *secondary* `networkRef` in a primary-UDN
   namespace has eth0=UDN-primary + net1=secondary NAD. The primary (UDN) rides
   `setup_primary_udn_nic`; the secondary rides the existing path. Out of PR-1 scope
   (primary-only); documented as a later combination.
6. **`OfflineGPUMigratable()` / GPU:** unchanged — Model A is networking-only; GPU
   guests in a primary-UDN namespace get the UDN like any guest, orthogonally.

## 8. Out of scope (v0.5.0)

ClusterUserDefinedNetwork (CUDN, cluster-spanning tenant) — the namespaced UDN is the
v0.5.0 target; CUDN rides the same seams later. Multiple primary UDNs per namespace
(OVN-K allows one). The `enableOvnKubeIdentity` upstream fix (operator/OVN-K concern).
