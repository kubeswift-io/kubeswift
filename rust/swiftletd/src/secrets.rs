//! Secret-backed workload variables (spec.env[].valueFrom.secretKeyRef).
//!
//! The controller hands swiftletd references only, and grants this launcher's
//! own ServiceAccount `get` on exactly the referenced Secrets. swiftletd reads
//! them here and returns `KEY=VALUE` strings for the guest. A value is never
//! logged, never written to an annotation, and on a cold boot only reaches the
//! config disk in a memory-backed volume (KUBESWIFT_SECRET_RUN_DIR). Errors
//! name the variable, Secret and key, never a value.

use k8s_openapi::api::core::v1::Secret;
use kube::api::Api;
use kube::Client;

use crate::intent::SecretEnvRef;

/// Reads each referenced key and returns `NAME=VALUE`, in order. An optional
/// reference to a missing Secret or key is skipped (the variable stays unset,
/// as on a pod); any other failure is an error naming what could not be read.
pub async fn resolve(
    client: &Client,
    namespace: &str,
    refs: &[SecretEnvRef],
) -> Result<Vec<String>, String> {
    let api: Api<Secret> = Api::namespaced(client.clone(), namespace);
    let mut out = Vec::with_capacity(refs.len());
    for r in refs {
        let secret = match api.get_opt(&r.secret).await {
            Ok(s) => s,
            Err(e) => {
                return Err(format!(
                    "secret env {}: cannot read Secret {}: {}",
                    r.name, r.secret, e
                ))
            }
        };
        match value_of(secret.as_ref(), r)? {
            Some(v) => out.push(format!("{}={}", r.name, v)),
            None => log::info!(
                "secret_env_skipped name={} secret={} key={} reason=optional_and_missing",
                r.name,
                r.secret,
                r.key
            ),
        }
    }
    Ok(out)
}

/// The value of r's key in secret, None when it is missing and optional.
fn value_of(secret: Option<&Secret>, r: &SecretEnvRef) -> Result<Option<String>, String> {
    let Some(secret) = secret else {
        return if r.optional {
            Ok(None)
        } else {
            Err(format!(
                "secret env {}: Secret {} not found",
                r.name, r.secret
            ))
        };
    };
    let bytes = secret
        .data
        .as_ref()
        .and_then(|d| d.get(&r.key))
        .map(|b| b.0.clone())
        .or_else(|| {
            secret
                .string_data
                .as_ref()
                .and_then(|d| d.get(&r.key))
                .map(|s| s.as_bytes().to_vec())
        });
    let Some(bytes) = bytes else {
        return if r.optional {
            Ok(None)
        } else {
            Err(format!(
                "secret env {}: Secret {} has no key {}",
                r.name, r.secret, r.key
            ))
        };
    };
    String::from_utf8(bytes).map(Some).map_err(|_| {
        format!(
            "secret env {}: Secret {} key {} is not UTF-8 text",
            r.name, r.secret, r.key
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use k8s_openapi::ByteString;
    use std::collections::BTreeMap;

    fn r(name: &str, key: &str, optional: bool) -> SecretEnvRef {
        SecretEnvRef {
            name: name.into(),
            secret: "db".into(),
            key: key.into(),
            optional,
        }
    }

    fn secret(key: &str, val: &[u8]) -> Secret {
        let mut data = BTreeMap::new();
        data.insert(key.to_string(), ByteString(val.to_vec()));
        Secret {
            data: Some(data),
            ..Default::default()
        }
    }

    #[test]
    fn reads_a_key() {
        let s = secret("url", b"postgres://x");
        assert_eq!(
            value_of(Some(&s), &r("DATABASE_URL", "url", false)).unwrap(),
            Some("postgres://x".to_string())
        );
    }

    #[test]
    fn missing_is_an_error_unless_optional() {
        let s = secret("url", b"v");
        let e = value_of(Some(&s), &r("X", "nope", false)).unwrap_err();
        assert!(e.contains("Secret db has no key nope"), "{}", e);
        assert_eq!(value_of(Some(&s), &r("X", "nope", true)).unwrap(), None);
        assert!(value_of(None, &r("X", "url", false))
            .unwrap_err()
            .contains("not found"));
        assert_eq!(value_of(None, &r("X", "url", true)).unwrap(), None);
    }

    #[test]
    fn errors_never_carry_the_value() {
        let s = secret("bin", &[0xff, 0xfe, b's', b'3', b'c', b'r', b'3', b't']);
        let e = value_of(Some(&s), &r("X", "bin", false)).unwrap_err();
        assert!(e.contains("not UTF-8") && !e.contains("s3cr3t"), "{}", e);
    }
}
