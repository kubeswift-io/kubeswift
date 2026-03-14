## 1. API extensions for Secret/ConfigMap refs

**Prerequisite:** add-core-kubeswift-api-types, add-swiftguest-resolver.

- [x] 1.1 Extend SwiftSeedProfile (or ResolvedGuest.Seed) to support SecretKeyRef and ConfigMapKeyRef for userData, metaData, networkData
- [x] 1.2 Add SecretKeyRef and ConfigMapKeyRef structs if not present (or use corev1 types)
- [x] 1.3 Update resolver to pass through refs in ResolvedGuest.Seed
- [x] 1.4 Regenerate CRDs if API changed

## 2. Control-plane seed renderer

- [x] 2.1 Add internal/seed/render.go with Render(ResolvedGuest.Seed) -> (userData, metaData, networkData strings, error)
- [x] 2.2 Add internal/seed/resolve_refs.go to fetch Secret/ConfigMap and extract value by key
- [x] 2.3 Implement inline string pass-through (no ref)
- [x] 2.4 Implement SecretKeyRef resolution (fetch Secret, get data[key])
- [x] 2.5 Implement ConfigMapKeyRef resolution (fetch ConfigMap, get data[key])
- [x] 2.6 Return error when ref not found or key missing

## 3. ConfigMap creation

- [x] 3.1 Add internal/seed/configmap.go with BuildConfigMap(userData, metaData, networkData) -> *corev1.ConfigMap
- [x] 3.2 Use keys user-data, meta-data, network-config (NoCloud standard)
- [x] 3.3 Omit keys for empty values
- [x] 3.4 Set ConfigMap name and namespace (e.g., <guest-name>-seed, same namespace as guest)
- [x] 3.5 Set ownerReference to SwiftGuest for garbage collection

## 4. SwiftGuest controller integration

- [x] 4.1 When ResolvedGuest has Seed, call internal/seed.Render
- [x] 4.2 On success, call internal/seed.BuildConfigMap
- [x] 4.3 Create or update ConfigMap
- [x] 4.4 Add ConfigMap volume to pod spec
- [x] 4.5 Mount at well-known path (e.g., /var/lib/kubeswift/seed/<guest-name>/)
- [x] 4.6 When ResolvedGuest has no Seed, skip ConfigMap creation and volume

## 5. Rust swift-seed NoCloud builder

- [x] 5.1 Add rust/swift-seed/src/nocloud.rs with NoCloud builder
- [x] 5.2 Implement read from mounted ConfigMap path (user-data, meta-data, network-config files)
- [x] 5.3 Implement NoCloud directory layout (flat or openstack/latest/ per design choice)
- [x] 5.4 Implement build_nocloud_dir(configmap_path, output_path) -> Result
- [ ] 5.5 Add optional ISO generation (stub or genisoimage) for CDROM attachment
- [x] 5.6 Export build function for swiftletd to call

## 6. swiftletd integration

- [x] 6.1 Add swiftletd logic to detect seed ConfigMap mount path
- [x] 6.2 Call swift-seed to build NoCloud directory (and optionally ISO) before VM launch
- [ ] 6.3 Pass seed path or ISO path to Cloud Hypervisor config
- [x] 6.4 Handle case when no seed mount present (no cloud-init)

## 7. Tests and documentation

- [x] 7.1 Add unit tests for internal/seed/resolve_refs (inline, SecretRef, ConfigMapRef)
- [x] 7.2 Add unit tests for BuildConfigMap (keys present, empty omitted)
- [x] 7.3 Add unit tests for rust/swift-seed NoCloud layout
- [x] 7.4 Add config/samples/swiftseedprofile-with-secret.yaml
- [x] 7.5 Add docs/seed-rendering.md documenting control-plane vs node flow and ConfigMap contract
