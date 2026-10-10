//! What the sandbox kernel's bridge-initramfs understands beyond argv/env/cwd.
//!
//! A bridge ignores config it does not know, so an older one would run the
//! workload without its secret files or mounts. The kernel artifact carries a
//! `bridge-features` file next to `bzImage` (written by the kernel build from
//! the bridge's own list); swiftletd reads it once at start and refuses to ask
//! a bridge for a feature it does not list. No file means an older bridge
//! with none of them.

use std::collections::BTreeSet;
use std::path::Path;
use std::sync::OnceLock;

static FEATURES: OnceLock<BTreeSet<String>> = OnceLock::new();

/// Reads `bridge-features` from the kernel's directory. Call once at start.
pub fn load(kernel_path: &str) {
    let dir = Path::new(kernel_path).parent().unwrap_or(Path::new("/"));
    let features = parse(&std::fs::read_to_string(dir.join("bridge-features")).unwrap_or_default());
    log::info!("bridge_features {:?}", features);
    let _ = FEATURES.set(features);
}

fn parse(s: &str) -> BTreeSet<String> {
    s.split_whitespace().map(str::to_string).collect()
}

/// Ok when the bridge supports `feature`; otherwise the message the sandbox
/// fails with.
pub fn require(feature: &str, what: &str) -> Result<(), String> {
    let has = FEATURES.get().map(|f| f.contains(feature)).unwrap_or(false);
    if has {
        return Ok(());
    }
    let (sandbox, gpu) = minimum_kernels(feature);
    Err(format!(
        "the sandbox kernel's bridge does not support {} (feature '{}'); \
         point the SwiftKernel at kernels/sandbox {} or kernels/gpu-sandbox {} or newer",
        what, feature, sandbox, gpu
    ))
}

/// The first sandbox and gpu-sandbox kernels whose bridge has `feature`.
fn minimum_kernels(feature: &str) -> (&'static str, &'static str) {
    match feature {
        "warm-mounts" => ("6.6.15", "6.6.4"),
        _ => ("6.6.14", "6.6.3"),
    }
}

/// The features to advertise on the pod: the bridge's, less `warm-mounts`
/// when this launcher has no staging share to project artifacts into.
pub fn advertised(has_stage: bool) -> String {
    FEATURES
        .get()
        .map(|f| advertise(f, has_stage))
        .unwrap_or_default()
}

fn advertise(features: &BTreeSet<String>, has_stage: bool) -> String {
    features
        .iter()
        .filter(|x| has_stage || x.as_str() != "warm-mounts")
        .cloned()
        .collect::<Vec<_>>()
        .join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_the_feature_list() {
        let f = parse("files mounts\n");
        assert!(f.contains("files") && f.contains("mounts") && f.len() == 2);
        assert!(parse("").is_empty());
    }

    #[test]
    fn advertises_warm_mounts_only_with_a_staging_share() {
        let f = parse("files mounts warm-mounts\n");
        assert_eq!(advertise(&f, true), "files mounts warm-mounts");
        assert_eq!(advertise(&f, false), "files mounts");
        assert_eq!(advertise(&parse("files mounts"), true), "files mounts");
    }

    #[test]
    fn names_the_kernels_that_have_a_feature() {
        assert_eq!(minimum_kernels("warm-mounts"), ("6.6.15", "6.6.4"));
        assert_eq!(minimum_kernels("files"), ("6.6.14", "6.6.3"));
    }
}
