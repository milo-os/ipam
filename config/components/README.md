# IPAM Kustomize Components

Each subdirectory is a standalone `kind: Component` kustomization. Overlays opt
in by listing the component under `components:`. Components are independent
and order-insensitive within an overlay (with a few documented exceptions).

| Component                | Purpose                                                                  |
|--------------------------|--------------------------------------------------------------------------|
| `namespace`              | Creates `ipam-system` namespace                                          |
| `api-registration`       | `APIService` for `v1alpha1.ipam.miloapis.com`                            |
| `cert-manager-ca`        | Namespaced CA `Issuer` + `Certificate` (overrides selfsigned default)    |
| `postgres`               | Bitnami PostgreSQL `HelmRelease` (the only supported storage backend)    |
| `postgres-migrations`    | Job + ConfigMap that applies `migrations/*.sql`                          |
| `observability`          | `ServiceMonitor` + `GrafanaDashboard` resources                          |
| `k6-performance-tests`   | k6 SA/RBAC + `TestRun` resources for the perf suite                      |
| `service-catalog`        | `Service` + `ServiceConfiguration` registering IPAM (incl. IPClaim quota) on the Milo control plane |

Order matters when:
- `cert-manager-ca` must precede the deployment so its CSI volume can mount.
- `postgres-migrations` requires `postgres` to be applied first.
