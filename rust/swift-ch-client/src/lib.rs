//! Cloud Hypervisor client (Unix socket).
//!
//! Spawns the cloud-hypervisor process and talks to its REST API via a
//! Unix domain socket. No TCP, no TLS — local IPC only. The transport
//! lives in the [`api`] module; lifecycle/snapshot methods are layered
//! in [`methods`] on top of [`api::ApiClient`].

mod api;
mod config;
mod methods;
mod spawn;

pub use api::{await_api_client, ApiClient, ApiError, ApiResponse};
pub use config::{NICConfig, VFIODeviceConfig, VmConfig};
pub use methods::{VmInfo, VmmVersion};
pub use spawn::{connect_socket, spawn_ch, spawn_ch_restore, wait_for_socket};
