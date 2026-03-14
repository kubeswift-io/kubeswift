## 1. Script Implementation

- [x] 1.1 Create `scripts/kubeswift-preflight.sh` with shebang, header comment, and exit code policy
- [x] 1.2 Implement architecture check (x86_64 PASS, other FAIL)
- [x] 1.3 Implement kernel version check (>= 5.6 PASS, 4.11–5.5 WARN, < 4.11 FAIL)
- [x] 1.4 Implement hardware virtualization check (vmx/svm in /proc/cpuinfo)
- [x] 1.5 Implement KVM modules check (kvm + kvm_intel or kvm_amd loaded)
- [x] 1.6 Implement /dev/kvm check (exists and readable)
- [x] 1.7 Implement KVM package check (kvm or qemu-kvm; WARN if missing)
- [x] 1.8 Implement container runtime check (containerd or cri-o; WARN if missing)
- [x] 1.9 Implement cgroup v2 check (unified PASS, v1/hybrid WARN)
- [x] 1.10 Implement swap status check (disabled/minimal PASS, enabled WARN)
- [x] 1.11 Add non-Ubuntu detection and emit WARN, continue with best-effort checks
- [x] 1.12 Add summary section (PASS/WARN/FAIL counts, overall result)
- [x] 1.13 Implement exact exit code logic (0=all pass, 1=any fail, 2=warn only, 3=script error)

## 2. Documentation

- [x] 2.1 Create `docs/worker-node-preflight.md` with how to download and run the script
- [x] 2.2 Document PASS/WARN/FAIL interpretation (hard failures vs warnings) and mapping to operator checklist
- [x] 2.3 Document exact exit code behavior for automation
- [x] 2.4 Add sample output (PASS example, FAIL example) to `docs/worker-node-preflight.md`
- [x] 2.5 Add reference to preflight in `docs/operator-checklist-ubuntu-x86_64.md`

## 3. Integration

- [x] 3.1 Add `make preflight` target to Makefile (runs `scripts/kubeswift-preflight.sh`)
