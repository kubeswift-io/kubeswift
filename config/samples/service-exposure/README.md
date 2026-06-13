# Service Exposure samples

A KubeSwift VM port becomes a normal Kubernetes Endpoint (in-pod DNAT), so a VM
Service composes with the whole ecosystem. Docs:
[service exposure](../../../docs/networking/service-exposure.md) ·
[ecosystem integrations](../../../docs/networking/ecosystem-integrations.md).

Prerequisites for the SwiftGuest in each sample: a Ready `SwiftImage`
`ubuntu-noble`, a `SwiftGuestClass` `default`, and a `SwiftSeedProfile` `minimal`
(see [`config/samples/disk-boot`](../disk-boot/)).

| Sample | Shows | Needs installed |
|---|---|---|
| `01-ingress-clusterip.yaml` | declare ports → one ClusterIP Service | — |
| `02-metallb-loadbalancer.yaml` | a real bare-metal LoadBalancer IP | MetalLB |
| `03-gateway-httproute.yaml` | L7 host/path routing to a VM | a Gateway API controller |
| `04-gateway-tcproute.yaml` | raw L4 (SSH) routing to a VM | a Gateway API controller (TCPRoute) |
| `05-tailscale.yaml` | reach a VM over your tailnet | Tailscale K8s operator |
| `06-mesh-istio.yaml` | VM as an Istio mesh backend | Istio |
| `07-mesh-linkerd.yaml` | VM as a Linkerd mesh backend | Linkerd |

The integration objects (MetalLB pools, Gateways, mesh CRs) assume the
third-party controller is installed — follow each project's docs. The KubeSwift
side (ports/expose/annotations) is the only KubeSwift-specific surface.
