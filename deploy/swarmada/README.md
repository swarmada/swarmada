# Swarmada Helm chart

Installs the Swarmada control plane (RFC-0001): the controller-manager, the
`api/v1` CRDs, RBAC (manager role + the built-in `swarmada:*` roles), the
admission webhooks, the ControlStream gRPC endpoint, and an optional metrics
surface. The chart is **derived from `config/`** (kustomize) — CRDs and manager
RBAC are generated from `config/crd/bases` and `config/rbac`, not hand-maintained.

> Swarmada is a Prometheus **scrape target only**. This chart never packages,
> installs, or runs Prometheus / Grafana / Alertmanager. It ships *optional*
> ServiceMonitor / PrometheusRule manifests for an Operator you already run.

## Prerequisites

- Kubernetes ≥ 1.27 (webhook `admissionregistration.k8s.io/v1`, structural CRDs).
- [cert-manager](https://cert-manager.io) — **only if** `webhooks.certManager.enabled=true`
  (the default). See "Webhook certificates" for the no-cert-manager path.
- [Prometheus Operator](https://prometheus-operator.dev) — **only if** you enable
  `metrics.serviceMonitor` / `metrics.prometheusRule` (both default off).

## Install / upgrade / uninstall

```sh
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace
helm upgrade swarmada deploy/swarmada -n swarmada-system
helm uninstall swarmada -n swarmada-system   # CRDs and CRs are KEPT (see below)
```

## Key values

| Key | Default | Purpose |
|-----|---------|---------|
| `image.repository` / `image.tag` | `ghcr.io/swarmada/swarmada` / chart appVersion | Controller-manager image |
| `replicaCount` | `1` | Manager replicas (`manager.leaderElect` for >1) |
| `controlStream.enabled` / `.port` | `true` / `9440` | Fleet Adapter gRPC endpoint (off the fixed 9443 webhook port) |
| `modelWebhook.enabled` / `.port` | `true` / `9444` | ModelPolicy training-completion HMAC webhook |
| `webhooks.enabled` | `true` | Admission webhooks (`ENABLE_WEBHOOKS`) + configurations |
| `webhooks.certManager.enabled` | `true` | Provision the serving cert via cert-manager |
| `webhooks.caBundle` | `""` | CA bundle when cert-manager is disabled |
| `installCRDs` | `true` | Install the 13 CRDs with the chart |
| `rbac.create` / `rbac.clusterRoles` | `true` / `true` | Manager RBAC / the five `swarmada:*` roles |
| `metrics.service.port` | `8080` | `/metrics` endpoint (RFC-0001 §9.3.8) |
| `metrics.serviceMonitor.enabled` | `false` | Emit a Prometheus-Operator ServiceMonitor |
| `metrics.prometheusRule.enabled` | `false` | Emit the §9.3.8 alert rules |

## CRDs and the upgrade caveat

CRDs ship in `templates/` (gated by `installCRDs`), **not** Helm's `crds/` dir, so
`helm upgrade` updates their schemas as the alpha API evolves — the `crds/` dir
installs once and never upgrades. Each CRD carries
`helm.sh/resource-policy: keep`, so **`helm uninstall` leaves the CRDs — and every
CustomResource — intact**. Removing them is a deliberate manual step:

```sh
kubectl delete crd $(kubectl get crd -o name | grep swarmada.io)
```

Set `installCRDs: false` if a cluster admin applies `config/crd` out of band.
The chart CRDs are regenerated from `config/crd/bases` with `make helm-sync`
(CI enforces they are in sync via `make helm-verify-sync`).

## Webhook certificates

- **cert-manager (default):** a self-signed `Issuer` + `Certificate` provision the
  serving Secret; the ca-injector fills each webhook `caBundle`.
- **Without cert-manager:** set `webhooks.certManager.enabled=false`, create the TLS
  Secret `swarmada-webhook-server-cert` (`tls.crt`/`tls.key`) yourself, and pass the
  signing CA as `webhooks.caBundle` (base64 PEM). To skip webhooks entirely for a
  dev cluster, set `webhooks.enabled=false`.

## Metrics scraping

- **Prometheus Operator:** `--set metrics.serviceMonitor.enabled=true` (and
  `metrics.prometheusRule.enabled=true` for the §9.3.8 alerts). Ensure your
  Prometheus's `serviceMonitorSelector`/`ruleSelector` match
  `metrics.serviceMonitor.labels` / `metrics.prometheusRule.labels`.
- **No Prometheus Operator (raw scrape):** annotate the manager pod via
  `manager.podAnnotations`:

  ```yaml
  manager:
    podAnnotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "8080"
      prometheus.io/path: "/metrics"
  ```

The `/metrics` endpoint is unauthenticated by default (§9.3.8); front it with a
NetworkPolicy or an authenticating proxy in production.
