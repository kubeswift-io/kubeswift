//! vCPU→host-CPU pinning via sched_setaffinity.
//!
//! QEMU has no command-line option for vCPU thread affinity — libvirt applies
//! it post-spawn, and so do we: QMP query-cpus-fast maps each vCPU index to
//! its host thread id, then sched_setaffinity pins that thread to the host CPU
//! the controller chose (NUMA-local to the passed GPUs). Best-effort by the
//! caller: a failed pin degrades performance, never correctness.

use crate::config::QemuVCPUPin;

/// Pin each vCPU thread to its host CPU. `cpu_threads` is the
/// (cpu-index, thread-id) map from query-cpus-fast. Returns the number of
/// vCPUs pinned; errors on the first pin that cannot be applied (unknown vCPU
/// index or a rejected sched_setaffinity).
pub fn apply_pins(pins: &[QemuVCPUPin], cpu_threads: &[(u32, i32)]) -> Result<usize, String> {
    let mut applied = 0;
    for pin in pins {
        let tid = cpu_threads
            .iter()
            .find(|(idx, _)| *idx == pin.vcpu)
            .map(|(_, tid)| *tid)
            .ok_or_else(|| {
                format!(
                    "vcpu {} not in query-cpus-fast map ({} vCPUs reported)",
                    pin.vcpu,
                    cpu_threads.len()
                )
            })?;
        set_thread_affinity(tid, pin.host_cpu).map_err(|e| {
            format!(
                "pin vcpu {} (tid {}) -> cpu {}: {}",
                pin.vcpu, tid, pin.host_cpu, e
            )
        })?;
        applied += 1;
    }
    Ok(applied)
}

/// sched_setaffinity(tid, {cpu}) — pin one thread to one host CPU.
fn set_thread_affinity(tid: i32, cpu: u32) -> Result<(), String> {
    if cpu as usize >= libc::CPU_SETSIZE as usize {
        return Err(format!("host cpu {} exceeds CPU_SETSIZE", cpu));
    }
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_ZERO(&mut set);
        libc::CPU_SET(cpu as usize, &mut set);
        if libc::sched_setaffinity(tid, std::mem::size_of::<libc::cpu_set_t>(), &set) != 0 {
            return Err(std::io::Error::last_os_error().to_string());
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn gettid() -> i32 {
        unsafe { libc::syscall(libc::SYS_gettid) as i32 }
    }

    #[test]
    fn set_affinity_on_self_round_trips() {
        // A real sched_setaffinity on the test's own thread: pin to CPU 0 and
        // read the mask back. CPU 0 always exists.
        let tid = gettid();
        set_thread_affinity(tid, 0).expect("pin self to cpu 0");
        unsafe {
            let mut set: libc::cpu_set_t = std::mem::zeroed();
            assert_eq!(
                libc::sched_getaffinity(tid, std::mem::size_of::<libc::cpu_set_t>(), &mut set),
                0
            );
            assert!(libc::CPU_ISSET(0, &set), "cpu 0 must be in the mask");
            assert_eq!(libc::CPU_COUNT(&set), 1, "mask must be exactly {{0}}");
        }
    }

    #[test]
    fn apply_pins_maps_vcpu_to_thread() {
        // vcpu 1 maps to OUR tid so the pin lands on the test thread; vcpu 0
        // maps to a bogus map entry we don't pin.
        let tid = gettid();
        let pins = [QemuVCPUPin {
            vcpu: 1,
            host_cpu: 0,
        }];
        let map = [(0u32, 1i32), (1u32, tid)];
        assert_eq!(apply_pins(&pins, &map).unwrap(), 1);
    }

    #[test]
    fn apply_pins_unknown_vcpu_errors() {
        let pins = [QemuVCPUPin {
            vcpu: 7,
            host_cpu: 0,
        }];
        let err = apply_pins(&pins, &[(0, 1234)]).unwrap_err();
        assert!(err.contains("vcpu 7"), "{}", err);
    }
}
