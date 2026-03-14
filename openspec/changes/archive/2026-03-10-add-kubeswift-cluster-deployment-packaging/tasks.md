## 1. Image Build Definitions

- [ ] 1.1 Create `images/controller-manager/Containerfile` (multi-stage: build Go binary from cmd/controller-manager, minimal runtime)
- [ ] 1.2 Add `make build-images` target (builds controller-manager and swiftletd; integrates existing swiftletd build)

## 2. Cluster Install Manifests

- [ ] 2.1 Create `config/deploy/base/namespace.yaml` (kubeswift-system)
- [ ] 2.2 Create `config/deploy/base/serviceaccount.yaml` (controller-manager SA)
- [ ] 2.3 Create `config/deploy/base/controller-manager-rbac.yaml` (ClusterRole, ClusterRoleBinding for CRD reconciliation)
- [ ] 2.4 Create `config/deploy/base/controller-manager.yaml` (Deployment; image ghcr.io/projectbeskar/kubeswift/controller-manager:latest)

## 3. Install Kustomization

- [ ] 3.1 Create `config/deploy/base/kustomization.yaml` (resources: namespace, serviceaccount, controller-manager-rbac, controller-manager; images for tag override)

## 4. Makefile Targets

- [ ] 4.1 Add `make deploy` (apply config/crd, then kubectl apply -k config/deploy/base)
- [ ] 4.2 Add `make undeploy` (kubectl delete -k config/deploy/base, then delete CRDs)
- [ ] 4.3 Add `make load-images` (load controller-manager and swiftletd images into kind/minikube)
- [ ] 4.4 Update `make help` with build-images, deploy, undeploy, load-images

## 5. Verification

- [ ] 5.1 Run `make deploy` on a fresh cluster; verify controller-manager runs (swiftletd image used by SwiftGuest pods when controller creates them)
- [ ] 5.2 Add deploy/undeploy instructions to docs (e.g. docs/deploy.md)
