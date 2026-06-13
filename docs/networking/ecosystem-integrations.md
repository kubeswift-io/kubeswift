# Ecosystem Integrations

A KubeSwift VM Service is a **standard Kubernetes Service** — the in-pod DNAT
makes the VM a normal Endpoint (see [Service Exposure](service-exposure.md)). So
the KubeSwift-specific surface is tiny: declare `ports` with `expose`, and pass
through `serviceAnnotations` / `loadBalancerClass`. Everything below is the rest
of the ecosystem composing on top of that.

> **Scope note.** These are integration *patterns*, validated against the fact
> that the VM Service is a normal Service. The third-party install steps are
> summarized — follow each project's own docs for production setup. The dev
> cluster (kube-proxy + Calico) has none of these installed, so the end-to-end
> recipes are documented, not KubeSwift-cluster-validated; the KubeSwift side
> (the Service + annotations the recipes depend on) **is** validated.

Sample manifests: [`config/samples/service-exposure/`](../../config/samples/service-exposure/).

| Integration | What it gives a VM | KubeSwift surface used |
|---|---|---|
| [MetalLB](#metallb) | a real LoadBalancer IP on bare metal | `expose: LoadBalancer` + address-pool annotation |
| [Gateway API](#gateway-api) | L7 routing / TLS / host+path to the VM | the VM ClusterIP Service as a `backendRef` |
| [Tailscale](#tailscale) | reach the VM from anywhere on your tailnet | `loadBalancerClass: tailscale` / expose annotation |
| [Istio](#istio) | mesh routing, mTLS, policy to the VM | the VM Service as a mesh destination |
| [Linkerd](#linkerd) | mesh routing, mTLS to the VM | the VM Service as a mesh destination |

---

## MetalLB

Give a VM (or pool) a routable LoadBalancer IP on bare metal.

1. Install MetalLB and an address pool:

```yaml
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: { name: production, namespace: metallb-system }
spec: { addresses: ["192.168.1.240-192.168.1.250"] }
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: { name: production, namespace: metallb-system }
spec: { ipAddressPools: [production] }
```

2. Expose the guest as a LoadBalancer, selecting the pool by annotation:

```yaml
spec:
  network:
    serviceAnnotations:
      metallb.universe.tf/address-pool: production
    loadBalancerClass: metallb.universe.tf/metallb   # optional; pins MetalLB if you run several LB controllers
    ports:
    - { name: web, port: 80, targetPort: 8080, expose: LoadBalancer }
```

MetalLB assigns an external IP to `<guest>-svc`; `kubectl get svc <guest>-svc`
shows it under `EXTERNAL-IP`. For a **pool**, use `spec.service.{type:
LoadBalancer, annotations, loadBalancerClass}` — one VIP load-balanced across all
replica VMs.

---

## Gateway API

Route L7 traffic (host/path, TLS termination, header rules) to a VM. Works with
any Gateway controller — Envoy Gateway, Cilium, Istio, NGINX Gateway Fabric.

The VM's **ClusterIP** Service (`expose: ClusterIP`) is a normal `backendRef`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: { name: web-vm }
spec:
  parentRefs: [{ name: my-gateway }]
  hostnames: ["app.example.com"]
  rules:
  - matches: [{ path: { type: PathPrefix, value: / } }]
    backendRefs:
    - name: web-svc       # the SwiftGuest's <guest>-svc
      port: 80
```

For non-HTTP L4 (e.g. SSH, a game server, a database), use a `TCPRoute`/
`UDPRoute` (Gateway API experimental channel) pointing at the same Service. For a
**pool**, the `backendRef` is `<pool>-svc` — the Gateway load-balances across all
replica VMs, and the pool's autoscaling changes the backend set transparently.

---

## Tailscale

Reach a VM from anywhere on your **tailnet** — no public IP, no VPN gateway. This
is the easiest way to (for example) RDP into a Windows guest or SSH a Linux guest
from a laptop that isn't on the cluster network.

1. Install the [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator).
2. Expose the guest Service to the tailnet — either way works:

```yaml
# (a) loadBalancerClass — the operator provisions a tailnet proxy for the Service
spec:
  network:
    serviceAnnotations:
      tailscale.com/hostname: web-vm        # the MagicDNS name on your tailnet
    loadBalancerClass: tailscale
    ports:
    - { name: web, port: 80, targetPort: 8080, expose: LoadBalancer }
```

```yaml
# (b) expose annotation on a ClusterIP Service (no LoadBalancer needed)
spec:
  network:
    serviceAnnotations:
      tailscale.com/expose: "true"
      tailscale.com/hostname: win-vm
    ports:
    - { name: rdp, port: 3389, targetPort: 3389, expose: ClusterIP }
```

The VM is then reachable at `web-vm.<your-tailnet>.ts.net` from any device on the
tailnet. For a Windows guest, `rdp://win-vm.<tailnet>.ts.net` works from a Mac/PC
with no cluster routing. Pools work the same way via `spec.service`.

---

## Service mesh (Istio / Linkerd)

Two distinct questions — keep them separate:

### A. VM as a mesh *backend* (supported, works today)

A meshed pod calling the VM's Service routes through the mesh normally: the VM
Service is a standard mesh destination, traffic is load-balanced over its
endpoints, and mTLS terminates at the VM pod boundary. This is the common case —
mesh your microservices, and one of them talks to a VM-hosted database, legacy
app, or model server.

### Istio

```yaml
# Route + policy to the VM Service like any other host
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata: { name: legacy-vm }
spec:
  hosts: [legacy-svc.default.svc.cluster.local]   # the <guest>-svc / <pool>-svc
  http:
  - route:
    - destination: { host: legacy-svc.default.svc.cluster.local, port: { number: 8080 } }
    timeout: 5s
```

A `DestinationRule` with `trafficPolicy.tls.mode: ISTIO_MUTUAL` applies mesh mTLS
on the client side. Ambient mode (sidecar-less) routes to the VM Service the same
way.

### Linkerd

Linkerd discovers the VM Service automatically; a meshed client gets mTLS +
load-balancing to the VM endpoints with no extra config. Add an
[`HTTPRoute`/`ServiceProfile`](https://linkerd.io/2/features/) for retries,
timeouts, and per-route metrics against the VM Service.

### B. The VM *pod itself* in the mesh (advanced — validate first)

Injecting a mesh sidecar into the **launcher pod** (`istio.io/rev` /
`linkerd.io/inject` on the namespace or pod) is **not a turnkey path**. The
launcher pod already runs a `network-init` init container that programs custom
iptables (the in-pod DNAT) and holds `NET_ADMIN`; the mesh's `istio-init` /
`linkerd-init` also rewrites the pod's iptables to redirect traffic to its proxy.
The two iptables regimes (the DNAT `PREROUTING` chain vs the mesh `REDIRECT`
chains) interact, and the VM's traffic originates on `tap0`/`br0` and is
*forwarded* — so it may bypass the proxy's capture the same way it bypasses the
eBPF ClusterIP hook (the egress caveat in [Service Exposure §3](service-exposure.md#3-egress--vm--cluster-services-with-a-reachability-check)).

Recommendation: use pattern **A** (VM as a mesh backend) — it covers inbound
service traffic, which is what most VM workloads need. If you require the VM's
*outbound* calls to be transparently meshed, treat it as a spike on your specific
mesh + CNI before relying on it; KubeSwift does not claim this works out of the
box.
