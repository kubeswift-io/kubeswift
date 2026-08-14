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

#[cfg(test)]
mod tests {
    use super::*;

    // Guards the one failure mode a kube-rs bump can introduce that the compiler
    // cannot see (#499, kube 0.92 -> 4.2).
    //
    // kube 4.x builds its TLS stack on rustls 0.23, which panics at RUNTIME —
    // "no process-level CryptoProvider available" — if the dependency graph
    // resolves to zero or ambiguous crypto providers. Nothing about that is a
    // compile error, so swiftletd would build clean, ship, boot the VM, and then
    // die the first time it tried to patch its pod annotations. The operator
    // would see a guest that runs but never reports an IP.
    //
    // Constructing a Client is where the provider is demanded, so this test is
    // the cheapest place to find out. It deliberately does NOT call
    // create_client(): that falls back to Client::try_default(), which reads the
    // developer's kubeconfig and would make the test pass for the wrong reason.
    //
    // #[tokio::test], not #[test]: under kube 4.x, Client::try_from spawns a
    // tower Buffer worker and panics with "there is no reactor running" outside a
    // runtime. Harmless for swiftletd — create_client is async, so it always has
    // one — but it means a Client cannot be built from sync code, which is worth
    // knowing before someone tries.
    #[tokio::test]
    async fn client_construction_has_a_crypto_provider() {
        let config = kube::Config::new("https://kubernetes.default.svc".parse().unwrap());
        // root_cert stays None here on purpose: that is the branch kube 4.x
        // rerouted to rustls-platform-verifier, so it exercises both the shared
        // ClientConfig::builder() (the panic site) and the newly added fallback.
        Client::try_from(config)
            .expect("kube Client must build; a panic here is the rustls provider");
    }

    // create_client() only reaches Client::try_default() because incluster_dns()
    // returns Err off-cluster. If a future kube-rs made that a panic instead, the
    // fallback would never run and swiftletd would abort on a cluster whose
    // service-account dir is laid out unusually.
    #[test]
    fn incluster_dns_fails_cleanly_outside_a_cluster() {
        // In CI and on a workstation /var/run/secrets/kubernetes.io/serviceaccount
        // does not exist, so this must be an Err, not an unwind.
        if std::path::Path::new("/var/run/secrets/kubernetes.io/serviceaccount/token").exists() {
            return; // running inside a pod; the premise does not hold
        }
        assert!(
            kube::Config::incluster_dns().is_err(),
            "off-cluster incluster_dns must return Err so create_client falls back"
        );
    }
}
