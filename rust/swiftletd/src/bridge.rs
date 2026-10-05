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
    Err(format!(
        "the sandbox kernel's bridge does not support {} (feature '{}'); \
         point the SwiftKernel at kernels/sandbox 6.6.14 or kernels/gpu-sandbox 6.6.3 or newer",
        what, feature
    ))
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
}
