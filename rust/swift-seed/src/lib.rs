//! NoCloud seed media generation.
//!
//! Builds NoCloud datasource directory from ConfigMap mount for cloud-init.

pub mod nocloud;

pub use nocloud::build_nocloud_dir;
