//! Cloud Hypervisor API client (Unix socket).
//!
//! Spawns cloud-hypervisor process and connects to its API socket.
//! Uses local Unix sockets only; no TCP.

mod config;
mod spawn;

pub use config::VmConfig;
pub use spawn::{connect_socket, spawn_ch, wait_for_socket};
