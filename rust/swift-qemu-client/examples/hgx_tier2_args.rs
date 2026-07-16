//! Print the QEMU args the builder emits for a canned Tier-2 HGX topology.
//!
//! The drift-proof half of test/gpu/verify-qemu-topology.sh: the script feeds
//! THESE args (the real `QemuConfig::to_args()` output, never a hand-copied
//! command line) to a real qemu-system-x86_64, so builder changes are always
//! what gets smoke-booted.
//!
//! The canned shape mirrors the NVIDIA HGX H100/H200 layout (integration
//! guide WP-12736-002): 4 SXM GPUs (real guide BDFs), each behind its own
//! pcie-root-port, 2 virtual NUMA nodes, optional 1G hugepages. Sizes are
//! smoke-sized (1 GiB RAM total) — the arg SHAPE is what's under test.
//!
//! Flags:
//!   --dummy           substitute each vfio-pci device with an emulated PCIe
//!                     endpoint (e1000e) on the SAME root port — boots with no
//!                     GPU/VFIO on the host, validating that QEMU accepts and
//!                     realizes the generated hierarchy.
//!   --run-dir <dir>   place serial/QMP sockets + OVMF_VARS under <dir>
//!                     (default /tmp/hgx-topology-smoke).
//!   --hugepages       back RAM with 1G hugepages (needs a host pool);
//!                     default memfd so the smoke runs anywhere.
//!
//! One arg per output line (safe for shell array consumption).

use std::path::PathBuf;

use swift_qemu_client::{QemuConfig, QemuNUMANode, QemuVCPUPin, QemuVFIODevice};

fn main() {
    let argv: Vec<String> = std::env::args().collect();
    let dummy = argv.iter().any(|a| a == "--dummy");
    let hugepages = argv.iter().any(|a| a == "--hugepages");
    let run_dir = argv
        .iter()
        .position(|a| a == "--run-dir")
        .and_then(|i| argv.get(i + 1))
        .cloned()
        .unwrap_or_else(|| "/tmp/hgx-topology-smoke".to_string());

    let rp = |addr: &str| QemuVFIODevice {
        host_address: addr.to_string(),
        pcie_root_port: true,
        no_mmap: true,
    };

    let config = QemuConfig {
        guest_id: "smoke/hgx-tier2".to_string(),
        cpus: 4,
        memory_mib: 0, // NUMA node sizes are authoritative
        ovmf_code: PathBuf::from("/usr/share/OVMF/OVMF_CODE.fd"),
        ovmf_vars: PathBuf::from(format!("{}/OVMF_VARS.fd", run_dir)),
        disk_path: String::new(),
        seed_path: String::new(),
        tap_name: None,
        mac: String::new(),
        nics: vec![],
        serial_socket: PathBuf::from(format!("{}/serial.sock", run_dir)),
        qmp_socket: PathBuf::from(format!("{}/qmp.sock", run_dir)),
        data_disk_paths: vec![],
        // Real HGX H100/H200 GPU BDFs from the NVIDIA integration guide.
        vfio_devices: vec![
            rp("0000:0f:00.0"),
            rp("0000:10:00.0"),
            rp("0000:41:00.0"),
            rp("0000:44:00.0"),
        ],
        numa_nodes: vec![
            QemuNUMANode {
                id: 0,
                cpus: "0-1".to_string(),
                memory_mib: 512,
            },
            QemuNUMANode {
                id: 1,
                cpus: "2-3".to_string(),
                memory_mib: 512,
            },
        ],
        hugepages: if hugepages {
            "1G".to_string()
        } else {
            String::new()
        },
        vcpu_pinning: vec![
            QemuVCPUPin {
                vcpu: 0,
                host_cpu: 0,
            },
            QemuVCPUPin {
                vcpu: 1,
                host_cpu: 1,
            },
        ],
    };

    for arg in config.to_args() {
        if dummy && arg.starts_with("vfio-pci,host=") {
            // Keep the bus=rpN placement (the topology under test); swap the
            // VFIO endpoint for an emulated PCIe NIC and drop VFIO-only props.
            let bus = arg
                .split(',')
                .find(|p| p.starts_with("bus="))
                .unwrap_or("bus=pcie.0");
            println!("e1000e,{}", bus);
        } else {
            println!("{}", arg);
        }
    }
}
