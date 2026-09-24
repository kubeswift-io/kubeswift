//! vCPU→host-CPU pinning via sched_setaffinity.
//!
//! QEMU has no command-line option for vCPU thread affinity — libvirt applies
//! it post-spawn, and so do we: QMP query-cpus-fast maps each vCPU index to
//! its host thread id, then sched_setaffinity pins that thread to the host CPU
//! the controller chose (NUMA-local to the passed GPUs). Best-effort by the
//! caller: a failed pin degrades performance, never correctness.

use crate::config::QemuVCPUPin;

/// Pin each vCPU thread to its host CPU. `cpu_threads` is the
/// (cpu-index, thread-id) map from query-cpus-fast. Every pin is attempted:
/// stopping at the first failure left some vCPUs pinned and the rest not.
/// Returns the number pinned, or an error naming every pin that failed.
pub fn apply_pins(pins: &[QemuVCPUPin], cpu_threads: &[(u32, i32)]) -> Result<usize, String> {
    let mut applied = 0;
    let mut failed = Vec::new();
    for pin in pins {
        let Some(tid) = cpu_threads
            .iter()
            .find(|(idx, _)| *idx == pin.vcpu)
            .map(|(_, tid)| *tid)
        else {
            failed.push(format!(
                "vcpu {} not in query-cpus-fast map ({} vCPUs reported)",
                pin.vcpu,
                cpu_threads.len()
            ));
            continue;
        };
        match set_thread_affinity(tid, pin.host_cpu) {
            Ok(()) => applied += 1,
            Err(e) => failed.push(format!(
                "pin vcpu {} (tid {}) -> cpu {}: {}",
                pin.vcpu, tid, pin.host_cpu, e
            )),
        }
    }
    if failed.is_empty() {
        Ok(applied)
    } else {
        Err(format!(
            "pinned {} of {} vCPUs; {}",
            applied,
            pins.len(),
            failed.join("; ")
        ))
    }
}

/// Fit the controller's pins to the CPUs this process may actually use.
///
/// The controller picks host CPUs NUMA-local to the guest's GPUs, but under
/// the kubelet's static CPU Manager the pod gets its exclusive CPUs at
/// admission, after the controller wrote the intent: a chosen CPU outside the
/// pod's cpuset is rejected by the kernel (EINVAL). Pins already inside
/// `allowed` are kept. Each other one moves to an unused allowed CPU, on the
/// same NUMA node as the CPU it was meant for when there is one. A pin with no
/// CPU left is dropped. Returns the pins to apply and a note per change.
pub fn fit_to_cpuset(
    pins: &[QemuVCPUPin],
    allowed: &[u32],
    node_of: &dyn Fn(u32) -> Option<u32>,
) -> (Vec<QemuVCPUPin>, Vec<String>) {
    use std::collections::BTreeSet;
    let allowed: BTreeSet<u32> = allowed.iter().copied().collect();
    let mut used: BTreeSet<u32> = pins
        .iter()
        .map(|p| p.host_cpu)
        .filter(|c| allowed.contains(c))
        .collect();
    let mut out = Vec::with_capacity(pins.len());
    let mut notes = Vec::new();
    for pin in pins {
        if allowed.contains(&pin.host_cpu) {
            out.push(*pin);
            continue;
        }
        let want = node_of(pin.host_cpu);
        let pick = allowed
            .iter()
            .copied()
            .filter(|c| !used.contains(c))
            .find(|c| want.is_some() && node_of(*c) == want)
            .or_else(|| allowed.iter().copied().find(|c| !used.contains(c)));
        match pick {
            Some(cpu) => {
                used.insert(cpu);
                notes.push(format!(
                    "vcpu {}: cpu {} is outside the pod's cpuset, using cpu {}",
                    pin.vcpu, pin.host_cpu, cpu
                ));
                out.push(QemuVCPUPin {
                    vcpu: pin.vcpu,
                    host_cpu: cpu,
                });
            }
            None => notes.push(format!(
                "vcpu {}: cpu {} is outside the pod's cpuset and no allowed cpu is free; left unpinned",
                pin.vcpu, pin.host_cpu
            )),
        }
    }
    (out, notes)
}

/// The NUMA node a host CPU sits on, from sysfs (`cpuN/nodeM`).
pub fn cpu_numa_node(cpu: u32) -> Option<u32> {
    let dir = std::fs::read_dir(format!("/sys/devices/system/cpu/cpu{}", cpu)).ok()?;
    dir.filter_map(|e| e.ok())
        .filter_map(|e| e.file_name().into_string().ok())
        .find_map(|n| n.strip_prefix("node").and_then(|m| m.parse().ok()))
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

    fn pin(vcpu: u32, host_cpu: u32) -> QemuVCPUPin {
        QemuVCPUPin { vcpu, host_cpu }
    }

    // Under the static CPU Manager the pod owns CPUs the controller did not
    // pick. Pins outside the pod's cpuset move to free allowed CPUs, on the
    // intended CPU's NUMA node when possible, instead of failing with EINVAL.
    #[test]
    fn fit_to_cpuset_moves_disallowed_pins_numa_locally() {
        // cpus 0-7 on node 0, 8-15 on node 1; the pod owns 4,5 and 12,13.
        let node = |c: u32| Some(if c < 8 { 0 } else { 1 });
        let (out, notes) =
            fit_to_cpuset(&[pin(0, 12), pin(1, 9), pin(2, 1)], &[4, 5, 12, 13], &node);
        assert_eq!(out, vec![pin(0, 12), pin(1, 13), pin(2, 4)]);
        assert_eq!(notes.len(), 2, "{:?}", notes);
    }

    #[test]
    fn fit_to_cpuset_drops_a_pin_with_no_cpu_left() {
        let (out, notes) = fit_to_cpuset(&[pin(0, 4), pin(1, 9)], &[4], &|_| None);
        assert_eq!(out, vec![pin(0, 4)]);
        assert!(notes[0].contains("left unpinned"), "{:?}", notes);
    }

    // One bad pin no longer stops the rest.
    #[test]
    fn apply_pins_attempts_every_pin() {
        let tid = gettid();
        let err = apply_pins(&[pin(7, 0), pin(0, 0)], &[(0, tid)]).unwrap_err();
        assert!(err.contains("pinned 1 of 2"), "{}", err);
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
