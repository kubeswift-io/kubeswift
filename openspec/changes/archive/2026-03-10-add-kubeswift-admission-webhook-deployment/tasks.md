## 1. Controller-Manager Webhook Configuration

- [x] 1.1 Add webhook options to `cmd/controller-manager/main.go`: Port 9443, Host "0.0.0.0", CertDir from env or flag (default /tmp/k8s-webhook-server/serving-certs)
- [x] 1.2 Add containerPort 9443 to controller-manager Deployment in `config/manager/deployment.yaml`

## 2. Webhook Service

- [x] 2.1 Add `config/webhook/service.yaml` (Service selecting controller-manager, port 9443)
- [x] 2.2 Add service to `config/webhook/kustomization.yaml`

## 3. TLS Certificate (cert-manager)

- [x] 3.1 Add `config/webhook/certificate.yaml` (cert-manager Certificate for webhook Service DNS name)
- [x] 3.2 Add certificate volume and volumeMount to controller-manager Deployment (via overlay patch)
- [x] 3.3 Add certificate.yaml to `config/webhook/kustomization.yaml`

## 4. Webhook Configurations

- [x] 4.1 Add `config/webhook/validating-webhook.yaml` (ValidatingWebhookConfiguration for SwiftGuest, SwiftImage, SwiftSeedProfile; annotate for cert-manager CA injection)
- [x] 4.2 Add `config/webhook/mutating-webhook.yaml` (MutatingWebhookConfiguration for same resources; annotate for CA injection)
- [x] 4.3 Add validating-webhook.yaml and mutating-webhook.yaml to `config/webhook/kustomization.yaml`

## 5. Webhook Overlay (Preserve Minimal Path)

- [x] 5.1 Create `config/overlays/webhook/kustomization.yaml` that composes default + webhook and patches deployment
- [x] 5.2 Ensure minimal `config/default` remains deployable without webhook

## 6. Documentation

- [x] 6.1 Document cert-manager prerequisite in `docs/deploy.md` (install command, version)
- [x] 6.2 Document webhook deploy flow: apply cert-manager, then deploy with webhook overlay
- [x] 6.3 Document rollback: delete ValidatingWebhookConfiguration, MutatingWebhookConfiguration if webhook blocks create/update
