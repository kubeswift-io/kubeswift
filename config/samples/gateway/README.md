# kubeswift-gateway samples

Register member clusters with the [kubeswift-gateway](../../../docs/ui/gateway.md)
hub.

1. **Enable the gateway on the hub** (Helm):

   ```bash
   helm upgrade --install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
     -n kubeswift-system --create-namespace \
     --set gateway.enabled=true --set gateway.authMode=token
   ```

2. **On each member cluster**, apply [`member-rbac.yaml`](member-rbac.yaml) and
   mint a credential (e.g. a ServiceAccount token) for the gateway.
   - **Sections 1 to 3** let the gateway's member credential impersonate end
     users and read VMs as them.
   - **Section 4** gives the credential the console, sandbox-shell and log
     bridge. That needs ValidatingAdmissionPolicy (Kubernetes 1.30+). On an older
     member, remove section 4 before applying; the console then refuses.

3. **On the hub**, apply [`cluster.yaml`](cluster.yaml) — a credential Secret + a
   `fleet.kubeswift.io/v1alpha1` Cluster — once per member (edit names/endpoints
   and paste the member credential).

4. **Verify**:

   ```bash
   kubectl -n kubeswift-system get clusters
   ```

   Each member should reach `READY=True` with its Kubernetes version + guest
   count. See the [operator guide](../../../docs/ui/gateway.md) for the full flow
   and the auth/security notes.
