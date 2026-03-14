## 1. Image Build Definitions

- [x] 1.1 Ensure `images/controller-manager/Containerfile` exists (multi-stage: build Go binary from cmd/controller-manager, minimal runtime)
- [x] 1.2 Ensure `images/swiftletd/Containerfile` exists and builds swiftletd with Cloud Hypervisor

## 2. Namespace and Service Accounts

- [x] 2.1 Ensure `config/namespace/namespace.yaml` exists (kubeswift-system)
- [x] 2.2 Ensure `config/namespace/kustomization.yaml` exists
- [x] 2.3 Ensure `config/manager/serviceaccount.yaml` exists (controller-manager SA)
- [x] 2.4 Ensure `config/daemonset/serviceaccount.yaml` exists (swiftletd SA)

## 3. Controller-Manager Manifests

- [x] 3.1 Ensure `config/manager/controller-manager-rbac.yaml` exists (ClusterRole, ClusterRoleBinding for CRD reconciliation)
- [x] 3.2 Ensure `config/manager/deployment.yaml` exists (controller-manager Deployment; image ghcr.io/projectbeskar/kubeswift/controller-manager:latest)
- [x] 3.3 Ensure `config/manager/kustomization.yaml` exists and includes serviceaccount, rbac, deployment (exclude webhook resources for minimal)

## 4. Swiftletd DaemonSet

- [x] 4.1 Ensure `config/daemonset/daemonset.yaml` exists (swiftletd DaemonSet with privileged, hostPath for /var/lib/kubeswift, /dev/kvm)
- [x] 4.2 Ensure `config/daemonset/kustomization.yaml` exists

## 5. Install Kustomization

- [x] 5.1 Ensure `config/default/kustomization.yaml` composes namespace, manager, daemonset only (no webhook for minimal install)

## 6. Makefile Targets

- [x] 6.1 Ensure `make build-images` builds controller-manager and swiftletd (no webhook-server for minimal)
- [x] 6.2 Ensure `make deploy` applies config/crd, then kubectl apply -k config/default
- [x] 6.3 Ensure `make undeploy` deletes config/default, then CRDs
- [x] 6.4 Ensure `make load-images` loads controller-manager and swiftletd into kind/minikube
- [x] 6.5 Update `make help` with build-images, deploy, undeploy, load-images

## 7. Documentation

- [x] 7.1 Add or update `docs/deploy.md` with minimal install flow: build images, deploy, undeploy, load-images for local clusters
- [x] 7.2 Document post-deploy step: apply RBAC in SwiftGuest namespace (`kubectl apply -k config/rbac -n <namespace>`)
