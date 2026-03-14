# Seed Rendering for NoCloud

KubeSwift delivers cloud-init-compatible NoCloud datasource media without reimplementing cloud-init. This document describes the control-plane vs node flow and the ConfigMap contract.

## Overview

1. **Control plane (Go)**: Resolves user-data, meta-data, network-config from SwiftSeedProfile (inline or Secret/ConfigMap refs), creates a ConfigMap with NoCloud-standard keys, mounts it into the guest pod.
2. **Node runtime (Rust, swiftletd)**: Reads ConfigMap contents from the mounted path, builds NoCloud directory layout via swift-seed, passes path to Cloud Hypervisor.

## Control Plane Flow

1. SwiftGuest controller reconciles SwiftGuest.
2. Resolver produces ResolvedGuest (includes Seed from SwiftSeedProfile).
3. Seed renderer (`internal/seed.Render`) resolves Secret/ConfigMap refs, produces final strings.
4. Controller creates ConfigMap with keys: `user-data`, `meta-data`, `network-config`.
5. Controller adds ConfigMap volume to pod spec, mounts at `/var/lib/kubeswift/seed`.
6. ConfigMap name: `<guest-name>-seed`.

## ConfigMap Contract

| ConfigMap key | NoCloud file |
|---------------|--------------|
| user-data | openstack/latest/user_data |
| meta-data | openstack/latest/meta_data.json |
| network-config | openstack/latest/network_config.json |

Empty values are omitted. The node runtime (swift-seed) copies from ConfigMap mount to NoCloud layout.

## Node Runtime Flow

1. swiftletd loads runtime intent from `/var/lib/kubeswift/intent/runtime-intent.json`.
2. If `seed_path` is non-empty, swiftletd calls swift-seed `build_nocloud_dir(configmap_path, output_path)`.
3. swift-seed reads user-data, meta-data, network-config from ConfigMap mount and writes NoCloud v2 layout to output.
4. swiftletd passes path to Cloud Hypervisor for VM launch.

## When No Seed

If SwiftGuest does not reference SwiftSeedProfile, no seed ConfigMap is created. No seed volume is mounted. `seed_path` in runtime intent is empty. VM boots without cloud-init.
