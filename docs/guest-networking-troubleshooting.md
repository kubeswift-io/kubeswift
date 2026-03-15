# Guest Networking Troubleshooting

When `status.network.primaryIP` is empty, use these steps to diagnose.

## Root cause (verified): Interface name mismatch

**Ubuntu cloud images use predictable interface naming** (e.g. `ens3`, `enp0s3`) for virtio-net, not legacy `eth0`. If the default network-config targets only `eth0`, cloud-init creates netplan for a non-existent interface and the VM never sends DHCP. The lease file stays empty.

**Fix:** Default network-config uses `match: name: en*` (predictable) and `match: name: eth*` (legacy) to support Ubuntu, Debian, Rocky, Fedora. See `internal/controller/swiftguest/controller.go` `defaultNetworkConfig`.

## Data flow

1. **dnsmasq** (in launcher) assigns DHCP lease → writes to `/var/lib/kubeswift/run/<guest-id>/dnsmasq.leases`
2. **swiftletd** lease poller reads the file → patches pod annotation `kubeswift.io/guest-ip`
3. **Controller** reads annotation in `MapPodToStatus` → sets `status.Network.PrimaryIP`

## Diagnostic commands

Run these with your SwiftGuest name and namespace (e.g. `sample`, `default`):

```bash
# 1. Check SwiftGuest status
kubectl get swiftguest sample -n default -o yaml | grep -A5 "status:"

# 2. Check pod annotation (set by swiftletd)
kubectl get pod -n default -l swift.kubeswift.io/guest=sample -o jsonpath='{.items[0].metadata.annotations}' | jq .

# 3. Check launcher logs for lease discovery messages
POD=$(kubectl get pod -n default -l swift.kubeswift.io/guest=sample -o jsonpath='{.items[0].metadata.name}')
kubectl logs "$POD" -n default -c launcher 2>&1 | grep -E "lease|guest IP|discovered|patched"

# 4. Exec into launcher and check lease file
kubectl exec -n default "$POD" -c launcher -- cat /var/lib/kubeswift/run/default-sample/dnsmasq.leases 2>/dev/null || echo "Lease file missing or path wrong"

# 5. Check if dnsmasq is running
kubectl exec -n default "$POD" -c launcher -- ps aux | grep dnsmasq

# 6. Check network setup (br0, tap0)
kubectl exec -n default "$POD" -c launcher -- ip addr show br0 2>/dev/null || echo "br0 not found"
kubectl exec -n default "$POD" -c launcher -- ip link show tap0 2>/dev/null || echo "tap0 not found"
```

## Common issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| **Lease file empty, "lease poll timeout"** | **Interface name mismatch: network-config targets eth0 but Ubuntu uses ens3** | Use `match: name: en*` in default network-config (fixed in controller) |
| Lease file missing | dnsmasq not started or wrong path | Check launcher logs; verify `guest_id` matches (default-sample) |
| Annotation set but status empty | Controller not reconciling after pod patch | Controller watches Pod; should reconcile. Check controller logs. |
| RBAC denied | swiftletd cannot patch pods | `kubectl apply -k config/rbac/ -n <namespace>` |
| No network-init | SwiftGuest has no seedProfileRef | Add seedProfileRef; networking requires seed |

## Guest ID and paths

- **Guest ID**: `namespace/name` (e.g. `default/sample`)
- **Safe ID** (path): `namespace-name` (e.g. `default-sample`)
- **Lease file**: `/var/lib/kubeswift/run/default-sample/dnsmasq.leases`
- **Runtime dir**: Same path; must match between launcher-entrypoint (dnsmasq) and swiftletd (poller)
