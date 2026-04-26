//! Cloud Hypervisor client (Unix socket).
//!
//! Spawns the cloud-hypervisor process and talks to its REST API via a
//! Unix domain socket. No TCP, no TLS — local IPC only. The transport
//! lives in the [`api`] module; higher-level methods (pause, snapshot,
//! resume, vm.info, version) are layered on top.

mod api;
mod config;
mod spawn;

pub use api::{await_api_client, ApiClient, ApiError, ApiResponse};
pub use config::{NICConfig, VFIODeviceConfig, VmConfig};
pub use spawn::{connect_socket, spawn_ch, wait_for_socket};
