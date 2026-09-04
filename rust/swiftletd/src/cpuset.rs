//! vCPU placement: choose which host CPUs a guest's vCPUs are pinned to.
//!
//! The candidate CPUs are the launcher pod's OWN effective cpuset, never the
//! node's full CPU list. That distinction is the whole point of computing the
//! plan here rather than in the controller:
//!
//!   * Under the kubelet CPU Manager `static` policy the pod's exclusive CPUs
//!     are assigned at admission — after the controller has written the intent.
//!     A controller-chosen CPU outside that set is not honoured: the kernel
//!     clamps or rejects it, and the guest silently runs unpinned.
//!   * With the policy at `none` the effective cpuset is simply every host CPU,
//!     so the same code produces the same result the naive approach would.
//!
//! Reading the cpuset at launch time is therefore correct under both policies,
//! and is the only form that stays correct if the node's policy changes later.
//!
//! The planner is pure: topology is passed in, so the placement rules are unit
//! tested without sysfs and without a tuned node.

use std::collections::BTreeMap;
use std::path::Path;

/// Sibling-placement policy (SwiftGuestClass.spec.smtPolicy).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SmtPolicy {
    /// One thread per physical core before any core's second thread.
    Spread,
    /// Both siblings of a core before moving to the next core.
    Pack,
}

impl SmtPolicy {
    /// Parse the intent string. Anything unrecognised (including absent) is
    /// Spread — the documented default.
    pub fn from_intent(s: Option<&str>) -> Self {
        match s {
            Some("pack") => Self::Pack,
            _ => Self::Spread,
        }
    }
}

/// One vCPU's host-CPU assignment. `host_cpus` is a set because Cloud
/// Hypervisor's `affinity=` accepts one, though static pinning always yields
/// exactly one entry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VcpuAffinity {
    pub vcpu: u32,
    pub host_cpus: Vec<u32>,
}

/// Parse a Linux CPU list ("0-3", "0,2,4", "0-1,4-5") into sorted CPU ids.
///
/// This format is shared by cpuset.cpus.effective, Cpus_allowed_list and the
/// sysfs topology files, so one parser serves all three.
pub fn parse_cpu_list(s: &str) -> Result<Vec<u32>, String> {
    let mut out = Vec::new();
    for part in s.trim().split(',').filter(|p| !p.is_empty()) {
        match part.split_once('-') {
            Some((lo, hi)) => {
                let lo: u32 = lo
                    .trim()
                    .parse()
                    .map_err(|_| format!("bad cpu range low {lo:?}"))?;
                let hi: u32 = hi
                    .trim()
                    .parse()
                    .map_err(|_| format!("bad cpu range high {hi:?}"))?;
                if hi < lo {
                    return Err(format!("inverted cpu range {part:?}"));
                }
                out.extend(lo..=hi);
            }
            None => out.push(
                part.trim()
                    .parse()
                    .map_err(|_| format!("bad cpu id {part:?}"))?,
            ),
        }
    }
    out.sort_unstable();
    out.dedup();
    Ok(out)
}

/// The launcher pod's effective cpuset.
///
/// cgroup v2 exposes it directly; the fallback covers cgroup v1 and any
/// environment where the cgroup file is absent. `Cpus_allowed_list` is the
/// authoritative last word either way — it is what the kernel will actually
/// permit — so it is a safe backstop rather than a guess.
pub fn effective_cpuset() -> Result<Vec<u32>, String> {
    for p in [
        "/sys/fs/cgroup/cpuset.cpus.effective",
        "/sys/fs/cgroup/cpuset/cpuset.cpus",
    ] {
        if let Ok(s) = std::fs::read_to_string(p) {
            if !s.trim().is_empty() {
                return parse_cpu_list(&s);
            }
        }
    }
    let status = std::fs::read_to_string("/proc/self/status")
        .map_err(|e| format!("read /proc/self/status: {e}"))?;
    for line in status.lines() {
        if let Some(rest) = line.strip_prefix("Cpus_allowed_list:") {
            return parse_cpu_list(rest);
        }
    }
    Err("could not determine the launcher pod's effective cpuset".to_string())
}

/// Read a CPU's SMT sibling list from sysfs. A CPU with no topology entry is
/// treated as its own sole sibling (no SMT).
pub fn thread_siblings(cpu: u32) -> Vec<u32> {
    let p = format!("/sys/devices/system/cpu/cpu{cpu}/topology/thread_siblings_list");
    if let Ok(s) = std::fs::read_to_string(Path::new(&p)) {
        if let Ok(v) = parse_cpu_list(&s) {
            if !v.is_empty() {
                return v;
            }
        }
    }
    vec![cpu]
}

/// Group the available CPUs into physical cores, preserving ascending order.
///
/// A core is keyed by the lowest sibling id that is actually in `available`, so
/// a partial cpuset (CPU Manager handing us one thread of a core) still yields
/// a coherent grouping rather than dropping the CPU.
fn cores(available: &[u32], siblings: &dyn Fn(u32) -> Vec<u32>) -> Vec<Vec<u32>> {
    let set: std::collections::BTreeSet<u32> = available.iter().copied().collect();
    let mut by_core: BTreeMap<u32, Vec<u32>> = BTreeMap::new();
    for &cpu in available {
        let mut sibs: Vec<u32> = siblings(cpu)
            .into_iter()
            .filter(|c| set.contains(c))
            .collect();
        sibs.sort_unstable();
        let key = *sibs.first().unwrap_or(&cpu);
        by_core.entry(key).or_default().push(cpu);
    }
    let mut out: Vec<Vec<u32>> = by_core
        .into_values()
        .map(|mut v| {
            v.sort_unstable();
            v.dedup();
            v
        })
        .collect();
    out.sort_by_key(|c| c[0]);
    out
}

/// Order the available CPUs according to `policy`, then take the first `vcpus`.
///
/// Spread walks thread 0 of every core, then thread 1 of every core, and so on,
/// so a guest smaller than the core count never doubles up on a core. Pack
/// walks each core's threads together.
pub fn plan_with_topology(
    vcpus: u32,
    available: &[u32],
    policy: SmtPolicy,
    siblings: &dyn Fn(u32) -> Vec<u32>,
) -> Result<Vec<VcpuAffinity>, String> {
    if vcpus == 0 {
        return Err("cannot pin a guest with 0 vCPUs".to_string());
    }
    if (available.len() as u32) < vcpus {
        // Refuse rather than oversubscribe. Pinning two vCPUs to one host CPU
        // would report as "pinned" while delivering worse latency than not
        // pinning at all, which is exactly the silent-degradation this feature
        // exists to remove.
        return Err(format!(
            "cpuPinning=static needs at least {vcpus} CPUs in the launcher pod's cpuset, found {} ({available:?}). \
             Reduce the class's cpu, or give the pod more CPUs.",
            available.len()
        ));
    }

    let cores = cores(available, siblings);
    let mut ordered: Vec<u32> = Vec::with_capacity(available.len());
    match policy {
        SmtPolicy::Spread => {
            let depth = cores.iter().map(|c| c.len()).max().unwrap_or(0);
            for t in 0..depth {
                for core in &cores {
                    if let Some(&cpu) = core.get(t) {
                        ordered.push(cpu);
                    }
                }
            }
        }
        SmtPolicy::Pack => {
            for core in &cores {
                ordered.extend(core.iter().copied());
            }
        }
    }

    Ok(ordered
        .into_iter()
        .take(vcpus as usize)
        .enumerate()
        .map(|(i, cpu)| VcpuAffinity {
            vcpu: i as u32,
            host_cpus: vec![cpu],
        })
        .collect())
}

/// Build the plan from the live host: the pod's own cpuset plus sysfs topology.
pub fn plan(vcpus: u32, policy: SmtPolicy) -> Result<Vec<VcpuAffinity>, String> {
    let available = effective_cpuset()
        .map_err(|e| format!("determine launcher cpuset for cpuPinning=static: {e}"))?;
    plan_with_topology(vcpus, &available, policy, &thread_siblings)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// An 8-CPU SMT host: cores (0,4) (1,5) (2,6) (3,7) — the enumeration used
    /// by the dev nodes this was validated on.
    fn smt8(cpu: u32) -> Vec<u32> {
        vec![cpu % 4, cpu % 4 + 4]
    }
    fn no_smt(cpu: u32) -> Vec<u32> {
        vec![cpu]
    }
    fn cpus(p: &[VcpuAffinity]) -> Vec<u32> {
        p.iter().map(|a| a.host_cpus[0]).collect()
    }

    #[test]
    fn parse_cpu_list_forms() {
        assert_eq!(parse_cpu_list("0-3").unwrap(), vec![0, 1, 2, 3]);
        assert_eq!(parse_cpu_list("0,2,4").unwrap(), vec![0, 2, 4]);
        assert_eq!(parse_cpu_list("0-1,4-5").unwrap(), vec![0, 1, 4, 5]);
        assert_eq!(parse_cpu_list(" 2 \n").unwrap(), vec![2]);
        assert!(parse_cpu_list("3-1").is_err());
        assert!(parse_cpu_list("abc").is_err());
    }

    /// Spread must not put two vCPUs on one physical core while another core is
    /// still free — that is the entire behavioural difference from pack.
    #[test]
    fn spread_uses_a_distinct_core_per_vcpu_first() {
        let p = plan_with_topology(4, &[0, 1, 2, 3, 4, 5, 6, 7], SmtPolicy::Spread, &smt8).unwrap();
        assert_eq!(
            cpus(&p),
            vec![0, 1, 2, 3],
            "spread should take one thread of each core"
        );
    }

    #[test]
    fn spread_falls_back_to_second_siblings_when_it_must() {
        let p = plan_with_topology(6, &[0, 1, 2, 3, 4, 5, 6, 7], SmtPolicy::Spread, &smt8).unwrap();
        assert_eq!(cpus(&p), vec![0, 1, 2, 3, 4, 5]);
    }

    #[test]
    fn pack_fills_both_siblings_of_a_core_first() {
        let p = plan_with_topology(4, &[0, 1, 2, 3, 4, 5, 6, 7], SmtPolicy::Pack, &smt8).unwrap();
        assert_eq!(
            cpus(&p),
            vec![0, 4, 1, 5],
            "pack should consume core (0,4) before core (1,5)"
        );
    }

    /// The CPU Manager case: the pod owns an arbitrary subset, and the plan must
    /// stay inside it.
    #[test]
    fn plan_never_leaves_the_pods_cpuset() {
        let avail = vec![2, 3, 6, 7];
        for policy in [SmtPolicy::Spread, SmtPolicy::Pack] {
            let p = plan_with_topology(4, &avail, policy, &smt8).unwrap();
            let got = cpus(&p);
            assert_eq!(got.len(), 4);
            for c in &got {
                assert!(
                    avail.contains(c),
                    "{policy:?} planned cpu {c} outside the cpuset {avail:?}"
                );
            }
        }
    }

    /// A cpuset holding only one thread of each core must still be usable.
    #[test]
    fn partial_cores_are_not_dropped() {
        let avail = vec![0, 1, 2, 3];
        let p = plan_with_topology(4, &avail, SmtPolicy::Spread, &smt8).unwrap();
        assert_eq!(cpus(&p), vec![0, 1, 2, 3]);
    }

    #[test]
    fn no_smt_host_is_sequential_under_both_policies() {
        for policy in [SmtPolicy::Spread, SmtPolicy::Pack] {
            let p = plan_with_topology(3, &[0, 1, 2, 3], policy, &no_smt).unwrap();
            assert_eq!(cpus(&p), vec![0, 1, 2]);
        }
    }

    /// Oversubscription is refused, not silently accepted — a guest reported as
    /// pinned must actually be 1:1.
    #[test]
    fn too_few_cpus_is_an_error_not_a_double_pin() {
        let err = plan_with_topology(4, &[0, 1], SmtPolicy::Spread, &smt8).unwrap_err();
        let msg = err;
        assert!(
            msg.contains("at least 4"),
            "error should name the requirement: {msg}"
        );
    }

    #[test]
    fn vcpu_ids_are_dense_and_ordered() {
        let p = plan_with_topology(4, &[0, 1, 2, 3, 4, 5, 6, 7], SmtPolicy::Spread, &smt8).unwrap();
        assert_eq!(
            p.iter().map(|a| a.vcpu).collect::<Vec<_>>(),
            vec![0, 1, 2, 3]
        );
    }

    #[test]
    fn smt_policy_parses_with_spread_as_default() {
        assert_eq!(SmtPolicy::from_intent(Some("pack")), SmtPolicy::Pack);
        assert_eq!(SmtPolicy::from_intent(Some("spread")), SmtPolicy::Spread);
        assert_eq!(SmtPolicy::from_intent(None), SmtPolicy::Spread);
        assert_eq!(SmtPolicy::from_intent(Some("nonsense")), SmtPolicy::Spread);
    }
}
