# Service Exposure

Expose services **to** and **from** a SwiftGuest VM using normal Kubernetes
primitives. The design rests on a single CNI-agnostic datapath rule: an in-pod
DNAT `podIP:port → vmIP:targetPort` makes a VM port a normal Kubernetes
**Endpoint**, so a VM Service is just a Service — and the whole north-south
ecosystem (ClusterIP/NodePort/LoadBalancer, Gateway API, NetworkPolicy, service
mesh, Tailscale) composes on top of it.

This is the KubeVirt masquerade + `virtctl expose` model. For third-party
integrations see [Ecosystem integrations](ecosystem-integrations.md).

---

## 1. Binding model

`spec.network.binding` selects how the primary NIC relates to the pod network:

| Binding | IP the VM gets | Ports | Use when |
|---|---|---|---|
| `nat` (default) | behind the pod IP (DNAT) | Service-exposable | the common case — Services, ingress, mesh |
| `bridge` | a portable NAD IP (multi-node L2) | reach the NAD IP directly (no DNAT, not Service-exposable) | you need a routable VM IP (telco/NFV, external L2) |

A guest with no `spec.network` block behaves exactly as before (nat).

---

## 2. Ingress — declare ports, get a Service

Add ports under `spec.network`. On a **nat** guest each port installs the in-pod
DNAT and a launcher containerPort; set `expose` on a port to additionally mint a
Service.

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: web
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: default }
  seedProfileRef: { name: minimal }
  network:
    binding: nat
    ports:
    - name: http
      port: 80          # reachable on the pod IP, and the Service port
      targetPort: 8080  # the in-guest listening port (defaults to port)
      protocol: TCP     # TCP (default) | UDP | SCTP
      expose: ClusterIP # ClusterIP | NodePort | LoadBalancer — omit for DNAT-only
```

- **One Service per guest** carries all exposed ports (`<guest>-svc`), a plain
  **selector Service** on `swift.kubeswift.io/guest`. The endpoint controller
  populates it; kube-proxy load-balances `serviceIP → podIP:port → (DNAT) → vmIP`.
- **Honest readiness**: the launcher gets a readiness probe on the first TCP
  port, so the Service endpoint is Ready only once the in-guest service answers.
- The Service is **owner-ref'd** by the SwiftGuest → garbage-collected on delete.
- `status.network.exposedPorts`, `status.network.serviceRef`, and the
  `PortsProgrammed` / `ServiceReady` conditions report exactly what was programmed.

`expose` is rejected for a `bridge` guest (no DNAT exists); declaring ports
without `expose` on a bridge guest is allowed (e.g. for NetworkPolicy targeting).

---

## 3. Egress — VM → cluster services, with a reachability check

- **VM → internet** works today (a MASQUERADE rule on the pod's egress).
- **VM → cluster DNS / ClusterIPs** is CNI-dependent:
  - **kube-proxy** clusters (iptables/IPVS): works — the ClusterIP DNAT lives in
    the host netfilter path the VM traffic transits.
  - **eBPF kube-proxy-replacement** (Cilium / Calico-eBPF): the ClusterIP hook is
    on the pod's `eth0`; VM traffic forwarded out `eth0` bypasses it. This is a
    known upstream gap ([KubeVirt #10388](https://github.com/kubevirt/kubevirt/issues/10388),
    [Cilium #37669](https://github.com/cilium/cilium/issues/37669)) and is a
    KubeSwift **roadmap** item (`egressMode: clusterServices`, needs a Cilium
    cluster to spike).

**No silent failure.** At startup the launcher probes the cluster DNS ClusterIP
(TCP 53) from the pod context and reports the result; the controller maps it to
`status.network.egress` (`ClusterServices` | `DirectOnly`) and an `EgressReady`
condition (visible in `kubectl get swiftguest -o wide`'s `EGRESS` column). On an
eBPF cluster this surfaces `EgressReady=False,
reason=ClusterIPUnreachableInPodNetns` — a notorious silent failure turned into a
documented, `kubectl`-visible state.

Short cluster-name resolution works: dnsmasq hands the guest the pod's DNS
search list, so `curl http://my-svc` resolves `my-svc.<ns>.svc.cluster.local`.

---

## 4. Pool-balanced Service — one Service across N replicas

A [`SwiftGuestPool`](../../config/samples/pool/) fronts all its replicas with one
load-balanced Service. Scaling the pool grows/shrinks the backend set
automatically (the HPA seam — the pool's `scale` subresource).

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: inference
spec:
  replicas: 3
  service:
    type: ClusterIP        # or NodePort / LoadBalancer
    # headless: true       # clusterIP: None — one A record per ready replica
    #                      # (client-side LB / sharded inference)
    ports:
    - name: http
      port: 8080
      targetPort: 8000
  template:
    metadata: { labels: { app: inference } }
    spec:
      imageRef: { name: ubuntu-noble }
      guestClassRef: { name: gpu-large }
      seedProfileRef: { name: inference-seed }
      runPolicy: Running
```

- The pool injects `service.ports` into each replica (DNAT + readiness probe, no
  per-replica Service) and mints **one selector Service** `<pool>-svc` on the
  `swift.kubeswift.io/pool` label.
- Endpoints follow readiness and scale churn via Kubernetes' own endpoint
  controller (a replica enters the Service only once its in-guest service
  answers; the Service survives a replica's live migration).
- `status.serviceRef` + the `ServiceReady` condition report it.
- A `bridge`-bound template is rejected for a pool Service (no DNAT):
  `ServiceReady=False, reason=BridgeBindingUnsupported`.

---

## 5. Decorating the Service (annotations + loadBalancerClass)

The minted Service is a normal Service, but it's controller-owned — so to drive
ecosystem controllers that key off Service annotations or a load-balancer class,
pass them through:

| Field | On | Purpose |
|---|---|---|
| `spec.network.serviceAnnotations` | SwiftGuest | annotations copied onto `<guest>-svc` |
| `spec.network.loadBalancerClass` | SwiftGuest | LB implementation for `expose: LoadBalancer` |
| `spec.service.annotations` | SwiftGuestPool | annotations copied onto `<pool>-svc` |
| `spec.service.loadBalancerClass` | SwiftGuestPool | LB implementation for `type: LoadBalancer` |

```yaml
  network:
    serviceAnnotations:
      metallb.universe.tf/address-pool: production
      external-dns.alpha.kubernetes.io/hostname: web.example.com
    loadBalancerClass: metallb.universe.tf/metallb   # only with expose: LoadBalancer
    ports:
    - { name: http, port: 80, targetPort: 8080, expose: LoadBalancer }
```

Notes:
- Annotations are **overlaid**, not replaced — annotations written by other
  controllers (MetalLB/cloud-LB status) are preserved. Removing a key from spec
  needs a manual cleanup (or recreate the Service).
- `loadBalancerClass` is **immutable** after the Service is created and is only
  valid with a `LoadBalancer` type; changing it requires recreating the Service.

These two fields are what make the [ecosystem recipes](ecosystem-integrations.md)
work without hand-authoring a parallel Service.

---

## 6. Quick reference

```bash
# What ports/Service/egress did a guest get?
kubectl get swiftguest web -o wide            # SERVICE + EGRESS columns
kubectl get swiftguest web -o jsonpath='{.status.network}{"\n"}'

# Reach a guest Service from another pod
kubectl run probe --rm -it --image=nicolaka/netshoot -- \
  curl http://web-svc.default.svc.cluster.local

# Pool: watch endpoints follow readiness across replicas
kubectl get endpointslices -l kubernetes.io/service-name=inference-svc
kubectl scale sgpool inference --replicas=5     # endpoints follow automatically
```
