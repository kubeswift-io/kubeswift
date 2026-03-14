## 1. Shared types and package setup

- [ ] 1.1 Add api/shared/ with LocalObjectReference (or use corev1.LocalObjectReference), Condition type, and Phase type
- [ ] 1.2 Add api/swift/v1alpha1/groupversion_info.go and doc.go
- [ ] 1.3 Add api/image/v1alpha1/groupversion_info.go and doc.go
- [ ] 1.4 Add api/seed/v1alpha1/groupversion_info.go and doc.go

## 2. Swift API types

- [ ] 2.1 Add api/swift/v1alpha1/swiftguestclass_types.go with SwiftGuestClass, SwiftGuestClassSpec, RootDiskSpec, DiskFormat
- [ ] 2.2 Add api/swift/v1alpha1/swiftguest_types.go with SwiftGuest, SwiftGuestSpec, SwiftGuestStatus, RunPolicy
- [ ] 2.3 Add RunPolicy constants (Running, Stopped) and DiskFormat constants (raw, qcow2)
- [ ] 2.4 Add +kubebuilder markers for CRD generation (resource, subresource, printcolumn)
- [ ] 2.5 Add +kubebuilder:object:root for DeepCopy generation

## 3. Image API types

- [ ] 3.1 Add api/image/v1alpha1/swiftimage_types.go with SwiftImage, SwiftImageSpec, SwiftImageStatus, ImageSource
- [ ] 3.2 Add status phase (Pending, Importing, Ready, Failed) and conditions
- [ ] 3.3 Add +kubebuilder markers for CRD and status subresource
- [ ] 3.4 Add +kubebuilder:object:root for DeepCopy generation

## 4. Seed API types

- [ ] 4.1 Add api/seed/v1alpha1/swiftseedprofile_types.go with SwiftSeedProfile, SwiftSeedProfileSpec, DatasourceType
- [ ] 4.2 Add DatasourceType constant NoCloud
- [ ] 4.3 Add +kubebuilder markers for CRD generation
- [ ] 4.4 Add +kubebuilder:object:root for DeepCopy generation

## 5. CRD generation

- [ ] 5.1 Add controller-gen to project (go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest or equivalent)
- [ ] 5.2 Add Makefile target `generate` that runs controller-gen for object, crd, and rbac
- [ ] 5.3 Configure controller-gen to output CRDs to config/crd/bases/
- [ ] 5.4 Run `make generate` and verify config/crd/bases/ contains swiftguest*.yaml, swiftguestclass*.yaml, swiftimage*.yaml, swiftseedprofile*.yaml
- [ ] 5.5 Add config/crd/kustomization.yaml or patch to include CRDs in kustomize build

## 6. Sample YAML

- [ ] 6.1 Add config/samples/swiftguestclass.yaml with cpu, memory, rootDisk (size, format: raw)
- [ ] 6.2 Add config/samples/swiftimage.yaml with source (URL or PVC) and format
- [ ] 6.3 Add config/samples/swiftseedprofile.yaml with datasource NoCloud and userData
- [ ] 6.4 Add config/samples/swiftguest.yaml referencing the above (imageRef, guestClassRef, seedProfileRef)
- [ ] 6.5 Add config/samples/README.md with brief usage and prerequisite order (GuestClass, Image, SeedProfile before Guest)
