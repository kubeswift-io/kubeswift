//! Kubernetes client with fallback for clusters where KUBERNETES_SERVICE_HOST
//! is unreachable but kubernetes.default.svc DNS works.

use kube::Client;

/// Creates a kube Client, trying Config::infer first, then Config::incluster_dns
/// as fallback. Some clusters (e.g. external API server, custom networking) have
/// KUBERNETES_SERVICE_HOST set but the cluster IP unreachable; DNS resolution
/// of kubernetes.default.svc may work.
pub async fn create_client() -> Result<Client, kube::Error> {
    let first_err = match kube::Client::try_default().await {
        Ok(client) => return Ok(client),
        Err(e) => e,
    };
    eprintln!(
        "swiftletd: kube Client::try_default failed ({}), trying incluster_dns",
        first_err
    );
    match kube::Config::incluster_dns() {
        Ok(config) => Client::try_from(config),
        Err(e2) => {
            eprintln!("swiftletd: incluster_dns also failed: {}", e2);
            Err(first_err)
        }
    }
}
