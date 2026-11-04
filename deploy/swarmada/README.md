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

- Kubernetes ≥ 1.31: it is the oldest version measured to admit an emergency stop on a zone stored before one of the `FleetZone` CRD's validation rules existed (CRD validation ratcheting).
- [cert-manager](https://cert-manager.io) — **only if** `webhooks.certManager.enabled=true`
  or `controlStream.tls.certManager.enabled=true` (both are the default). See "Webhook
  certificates" and "ControlStream mTLS" for the paths without it.
- [Prometheus Operator](https://prometheus-operator.dev) — **only if** you enable
  `metrics.serviceMonitor` / `metrics.prometheusRule` (both default off).

## Install / upgrade / uninstall

```sh
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace
helm upgrade swarmada deploy/swarmada -n swarmada-system
helm uninstall swarmada -n swarmada-system   # CRDs and CRs are KEPT (see below)
```

No manager image is published for this release; build it from source and give the
chart its name. On kind:

```sh
make docker-build IMG=swarmada-controller:dev
kind load docker-image swarmada-controller:dev --name <cluster>
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace \
  --set image.repository=swarmada-controller --set image.tag=dev
```

`make first-run` does all of this on a new kind cluster, then connects an adapter
and runs a task through it with admission webhooks and ControlStream authorization on.

## Key values

| Key | Default | Purpose |
|-----|---------|---------|
| `image.repository` / `image.tag` | `ghcr.io/swarmada/swarmada` / chart appVersion | Controller-manager image |
| `replicaCount` | `1` | Manager replicas. Must be `1` while `controlStream.enabled` (see "One replica") |
| `controlStream.enabled` / `.port` | `true` / `9445` | Fleet Adapter gRPC endpoint (off the fixed 9443 webhook port) |
| `controlStream.tls.certManager.enabled` | `true` | Issue the ControlStream CA and server certificate via cert-manager |
| `controlStream.tls.certManager.adapterClients` | `[]` | One client certificate per FleetAdapter (`name`, `namespace`, optional `secretName`) |
| `controlStream.tls.existingSecret` | `""` | Use your own Secret (`tls.crt`, `tls.key`, `ca.crt`) instead of cert-manager |
| `modelWebhook.enabled` / `.port` | `true` / `9444` | ModelPolicy training-completion HMAC webhook |
| `webhooks.enabled` | `true` | Admission webhooks (`ENABLE_WEBHOOKS`) + configurations. `false` turns off every admission check, the estop and `robots/status` checks included: see *Webhook certificates* below |
| `webhooks.certManager.enabled` | `true` | Provision the serving cert via cert-manager |
| `webhooks.caBundle` | `""` | CA bundle when cert-manager is disabled |
| `webhooks.failurePolicy` / `.acknowledgeFailOpenSecurity` | `Fail` / `false` | `Ignore` renders only with the acknowledgement set |
| `installCRDs` | `true` | Install the 21 CRDs with the chart |
| `rbac.create` / `rbac.clusterRoles` | `true` / `true` | Manager RBAC / the six built-in `swarmada:*` roles |
| `metrics.service.port` | `8080` | `/metrics` endpoint (RFC-0001 #control-plane-observability-prometheus-metrics-contract) |
| `metrics.serviceMonitor.enabled` | `false` | Emit a Prometheus-Operator ServiceMonitor |
| `metrics.prometheusRule.enabled` | `false` | Emit the RFC-0001 #control-plane-observability-prometheus-metrics-contract alert rules |

## ControlStream mTLS

The manager serves ControlStream only when it is given
`--fleet-adapter-tls-cert-file`, `--fleet-adapter-tls-key-file` and
`--fleet-adapter-client-ca-file`; otherwise it logs `NOT serving ControlStream (fail
closed)` and no adapter can connect. With `controlStream.enabled=true` the chart
always renders the three flags, mounts the Secret they name at
`/etc/controlstream-tls`, and refuses to render when it has no source for that
Secret. It never renders `--fleet-adapter-insecure-authz`, and refuses it in
`manager.extraArgs`: the manager refuses that flag together with mTLS.

- **cert-manager (default):** mirrors `config/controlstream-tls`. A self-signed root
  bootstraps a CA, and the CA issues the server certificate for
  `<fullname>-controlstream.<namespace>.svc`. The same CA signs adapter client
  certificates, and the manager trusts exactly those.
- **Your own material:** set `controlStream.tls.existingSecret` to a Secret in the
  release namespace holding `tls.crt` and `tls.key` (the server keypair, valid for
  the Service name above) and `ca.crt` (the CA that signs your adapters' client
  certificates). The chart then renders no ControlStream cert-manager objects.

### A client certificate for one FleetAdapter

An adapter's identity is its client certificate's DNS SAN,
`<adapter>.<namespace>.svc.cluster.local`, which must name the `FleetAdapter` object
and its namespace. List each adapter under `controlStream.tls.certManager.adapterClients`
and the chart issues its certificate from the ControlStream CA:

```sh
helm install swarmada deploy/swarmada -n swarmada-system --create-namespace \
  --set 'controlStream.tls.certManager.adapterClients[0].name=sim-fleet-adapter' \
  --set 'controlStream.tls.certManager.adapterClients[0].namespace=warehouse-a'
```

The Secret is `sim-fleet-adapter-warehouse-a-client-tls` in the release namespace
(override with `secretName`), so an adapter that mounts it runs in that namespace.
A template rather than a hand-applied `Certificate` because the issuer's name and
namespace come from the release, and because every certificate the chart issues is
checked for its usages: the server certificate carries `server auth` only and each
client certificate `client auth` only. Without that split the server certificate,
whose SAN has the adapter-identity shape, would also verify as an adapter.

The certificate authenticates the adapter; it does not authorize a robot. The
ControlStream accepts a robot's messages only when the robot is bound to that
`FleetAdapter` and its `robotClass` is in the adapter's `spec.servesRobotClasses`.

### One replica

`replicaCount` greater than 1 with `controlStream.enabled=true` is refused at render
time. Every replica accepts adapter streams, but commands are issued by the leading
replica and pushed only to the streams that replica holds in memory. The Service
spreads adapter connections across replicas, so an adapter connected to any other
replica receives no commands. Multi-replica command delivery is not implemented.
Leader election still protects a single replica across restarts and rollouts.

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
  signing CA as `webhooks.caBundle` (base64 PEM).
- **`webhooks.enabled=false`** installs no admission webhook at all, and the chart
  renders it without the acknowledgement `Ignore` needs. Nothing then checks the
  estop verbs on the estop annotation, the `cancel`, `append` and `activate` verbs, or the
  `robots/status` estop guard: any principal allowed to write `robots/status`
  (`swarmada:admin`) can set a stopped robot `Normal` with no clear, no resume
  checks and no audit entry, and a `Robot` that holds a stop can be deleted and
  created again `Normal`. Use it only on a single-user development cluster. See
  [Known limitations](../../docs/known-limitations.md#an-install-without-admission-webhooks-checks-nothing-at-admission).
- **`webhooks.failurePolicy=Ignore`** renders only with
  `webhooks.acknowledgeFailOpenSecurity=true`. While the webhook server is
  unreachable, `Ignore` admits the requests that the estop verb and estop-marker
  checks, the `robots/status` estop guard, the Robot admission gate, the RobotClass
  merge and the zone-delete check exist to refuse. The refusal names every webhook
  whose `failurePolicy` follows the value.
- **`vrobotdelete.swarmada.io`**, the check that refuses deleting a `Robot` that holds
  a stop, renders `Ignore` whatever `webhooks.failurePolicy` is: a manager outage never
  blocks deleting a `Robot`, and during one a `Robot` that holds a stop can be
  deleted, and comes back `Normal` if created again. The `FleetZone`, `FleetTask`,
  `FirmwareRollout` and `ModelRollout` delete checks follow `webhooks.failurePolicy`, so
  with `Fail` a namespace that holds one of those objects stays `Terminating` at least until
  the manager serves again. See
  [Deleting a namespace during a manager outage](../../docs/operations.md#4-deleting-a-namespace-during-a-manager-outage).

## Metrics scraping

- **Prometheus Operator:** `--set metrics.serviceMonitor.enabled=true` (and
  `metrics.prometheusRule.enabled=true` for the RFC-0001 #control-plane-observability-prometheus-metrics-contract alerts). Ensure your
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

The `/metrics` endpoint is unauthenticated by default (RFC-0001 #control-plane-observability-prometheus-metrics-contract); front it with a
NetworkPolicy or an authenticating proxy in production.
