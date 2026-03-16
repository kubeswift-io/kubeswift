//! Kubernetes client with fallback for clusters where KUBERNETES_SERVICE_HOST
//! is unreachable but kubernetes.default.svc DNS works.

use kube::Client;

/// Creates a kube Client. Tries Config::incluster_dns first (https://kubernetes.default.svc),
/// then Client::try_default as fallback. Some clusters (e.g. external API server at
/// frida.labk8s.io:6443) have KUBERNETES_SERVICE_HOST set but the cluster IP unreachable
/// from pods; kubernetes.default.svc DNS often resolves and routes correctly.
pub async fn create_client() -> Result<Client, kube::Error> {
    // Prefer incluster_dns: uses kubernetes.default.svc, more reliable when cluster IP is unreachable
    if let Ok(config) = kube::Config::incluster_dns() {
        if let Ok(client) = Client::try_from(config) {
            return Ok(client);
        }
    }
    // Fallback: try_default (kubeconfig or incluster_env with KUBERNETES_SERVICE_HOST)
    kube::Client::try_default().await
}
