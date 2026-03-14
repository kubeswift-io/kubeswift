//! Injects version/build metadata at compile time.
//! Set env vars KUBESWIFT_VERSION, KUBESWIFT_GIT_COMMIT, KUBESWIFT_BUILD_DATE
//! when building (e.g. from Makefile or CI).

fn main() {
    let version = std::env::var("KUBESWIFT_VERSION").unwrap_or_else(|_| {
        format!("{}-dev", env!("CARGO_PKG_VERSION"))
    });
    let git_commit = std::env::var("KUBESWIFT_GIT_COMMIT").unwrap_or_else(|_| "unknown".to_string());
    let build_date = std::env::var("KUBESWIFT_BUILD_DATE").unwrap_or_else(|_| "unknown".to_string());

    println!("cargo:rustc-env=KUBESWIFT_VERSION={}", version);
    println!("cargo:rustc-env=KUBESWIFT_GIT_COMMIT={}", git_commit);
    println!("cargo:rustc-env=KUBESWIFT_BUILD_DATE={}", build_date);
}
