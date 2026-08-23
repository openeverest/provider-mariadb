# provider-mariadb Roadmap

Plan to grow `provider-mariadb` from its current single-node baseline to a
production-capable OpenEverest provider covering HA, backups, PITR, observability,
transport encryption, external exposure and pod anti-affinity.

Everything below is expressed in terms of the three moving parts:

- **SDK** — `github.com/openeverest/openeverest/v2` (`Instance` API, `provider-runtime/controller`
  helpers, backup/BackupClass/BackupStorage types).
- **Operator** — `github.com/mariadb-operator/mariadb-operator/v26` (`MariaDB`, `Backup`,
  `PhysicalBackup`, `PointInTimeRecovery`, `Restore` CRs).
- **Provider** — this repo (`definition/`, `internal/provider/`, `charts/`).

---

## 1. Current state (baseline)

Source of truth: `internal/provider/`, `definition/`.

| Capability | Status | Where |
|---|---|---|
| Provisioning (standalone) | ✅ | `mariadb.go` `SyncMariaDB` / `buildInitialMariaDB` |
| Horizontal / vertical scaling | ✅ | `engine.replicas`, `engine.resources` |
| Version upgrades / bundles | ✅ | `definition/versions.yaml`, `resolveExporterImage` |
| Custom `my.cnf` | ✅ | `MariadbParameters.Configuration` → `spec.myCnf` |
| Persistent storage + expansion | ✅ | `engine.storage.size` → `spec.storage` |
| Monitoring (Prometheus) | ✅ | `monitoring` component → `spec.metrics` (mysqld-exporter + ServiceMonitor) |
| Service exposure (ClusterIP/LB/NodePort) | ✅ | `configureService` / `validateService` |
| Connection details | ✅ | `buildConnectionDetails` (user/pass, LB-aware host) |
| **HA topologies (Galera/replication)** | ❌ | only `standalone` topology exists |
| **Backups / restore / PITR** | ❌ | no `BackupProvider` implementation, no BackupClass |
| **TLS / transport encryption** | ✅ | enabled by default; operator-managed CA is published in connection details |
| **Anti-affinity / scheduling** | ❌ | `engine.Affinity` not mapped to the operator |

### Requested capabilities → gap summary

| Requested | Gap | Phase |
|---|---|---|
| Single node deployment | none (done) | — |
| Cluster deployment for HA (start with Galera) | new topology + `spec.galera` wiring | **P2** |
| Backups + restore to S3 (logical) | `BackupProvider` + BackupClass | **P4** |
| PITR (regular, not physical) | ⚠️ operator constraint (see §7) | **P5** |
| Observability with Prometheus | done; optional enhancements | **P6** |
| Transport encryption (TLS) | enabled by default + CA surfaced in connection details | **P3 (done)** |
| Expose via LoadBalancer / NodePort | done for standalone; extend to HA services | **P2 / P6** |
| Anti-affinity | map `engine.Affinity`, default-on for HA | **P1** |

---

## 2. Phase 1 — Pod anti-affinity & scheduling

Small, self-contained, and a prerequisite for meaningful HA. Ship first. **✅ Done.**

- The SDK already exposes `ComponentSpec.Affinity *corev1.Affinity`
  (`api/core/v1alpha1/instance_types.go`); the provider currently ignores it.
- The operator exposes `spec.podTemplate.affinity` as `AffinityConfig`
  (`api/v1alpha1/base_types.go`), a **trimmed** affinity type that models only
  `NodeAffinity` and `PodAntiAffinity` (no `PodAffinity`), plus a convenience
  `AntiAffinityEnabled *bool` that spreads pods across nodes.

**Tasks**

- [x] Map `engine.Affinity` (`corev1.Affinity`) onto `spec.affinity`
      (`AffinityConfig`) in `SyncMariaDB` — `internal/provider/affinity.go`
      (`convertAffinity`), applied for both create and update paths.
- [x] Reject `podAffinity` in `Validate` (`validateAffinity`) — the operator
      cannot express pod co-location.
- [x] Leave `AntiAffinityEnabled` unset: the user supplies explicit rules, so
      the operator's affinity defaulting stays a no-op and doesn't churn the value.
- [x] Unit tests for the affinity mapping and validation (`affinity_test.go`).
- [x] Example manifest (`examples/instance-anti-affinity.yaml`) + README capability row.
- [x] Default anti-affinity on for the HA/Galera topology — done in P2
      (`defaultGaleraAffinity`, soft/preferred).

**Note on template churn** — affinity lives in the pod template; changing it after
creation triggers a rolling update. It is only written when the user sets it.


---

## 3. Phase 2 — Galera HA topology

The core "cluster deployment for HA" ask. The operator supports Galera natively
(`spec.galera`, `api/v1alpha1/mariadb_galera_types.go`) with SST, recovery,
primary/secondary services and a PodDisruptionBudget. **✅ Done.**

**Tasks**

- [x] Add a `galera` topology under `definition/topologies/galera/`
      (`topology.yaml` + `types.go`), default replicas 3.
- [x] Add Galera handling in `SyncMariaDB` (`internal/provider/galera.go`):
      `applyGaleraOverlay` sets `spec.galera.enabled=true` and preserves the
      operator-defaulted Galera sub-fields across reconciles.
- [x] Enforce the operator's odd-replica rule (`>= 3`, odd) in `validate.go`
      (`validateGaleraReplicas`) **and** the topology UI schema (CEL).
- [x] Default a **soft** (preferred) pod anti-affinity for Galera
      (`defaultGaleraAffinity`) so nodes are spread without blocking scheduling —
      closes the deferred P1 item.
- [x] PodDisruptionBudget: **no action needed** — the operator auto-creates an HA
      PDB when Galera is enabled (`reconcilePodDisruptionBudget`).
- [x] Connection host: for Galera, `resolveHost` selects the primary Service
      (`<name>-primary`); standalone still uses the general Service.
- [x] Map exposure (`engine.Service`) onto `spec.primaryService` for Galera,
      `spec.service` for standalone.
- [x] Topology immutability: `validateTopologyImmutable` rejects standalone ↔
      galera changes by comparing against the existing MariaDB CR's Galera state.
- [x] Unit tests (`galera_test.go`) + example (`examples/instance-galera.yaml`)
      + README topology/capability rows.
- [x] Integration test (chainsaw) bringing up a 3-node Galera cluster
      (`test/integration/galera/`), wired as a **separate** CI job
      (`test-integration-galera` in `ci.yaml`) so it fails independently.

**Deferred:** async `replication` topology (operator marks it alpha — `TopologyTypeReplication`
already stubbed in `definition/types.go`). Needed later as a prerequisite for operator PITR (§7).


---

## 4. Phase 3 — Transport encryption (TLS)

The provider enables TLS by default while leaving enforcement opt-in so existing
username/password clients can migrate without disruption. The operator-generated
CA is copied into the OpenEverest connection Secret for server verification.

The operator's TLS support (`api/v1alpha1/mariadb_types.go` `TLS`) can issue and
mount server/client certs via its own CA or cert-manager, and `TLS.Required`
controls enforcement.

**Tasks**

- [x] Introduce TLS controls on the engine component under `parameters.tls`,
      defaulting TLS on with an opt-in `required` selector.
- [x] Map `enabled`, `required`, and Galera SST encryption to `spec.tls` while
      keeping username/password authentication available.
- [x] Decide CA source: operator-generated CA vs cert-manager (`pkg/discovery` gates
      cert-manager availability in the operator). Start with the operator CA.
- [x] Extend `ConnectionDetails` / connection Secret to carry the CA cert so
      consumers can verify the server.
- [x] Reconcile the TLS toggle in the read-modify-write overlay without churning the
      pod template every reconcile.
- [x] Validation + docs; update README (TLS ❌ → ✅).

**Risk:** flipping TLS on/off changes the pod template → rolling restart. Treat as a
deliberate, user-initiated change.

---

## 5. Phase 4 — Logical backups & restore to S3

Implement the SDK `BackupProvider` interface
(`provider-runtime/controller/interface.go`) with a **ProviderManaged** `BackupClass`,
following the MongoDB provider as the reference (`provider-percona-server-mongodb/internal/provider/backup.go`).

Operator building blocks:
- `Backup` CR (`api/v1alpha1/backup_types.go`) — logical `mariadb-dump`, `spec.storage.s3`,
  `spec.schedule`, `spec.maxRetention`, `spec.compression`.
- `Restore` CR / `spec.bootstrapFrom` on `MariaDB` — restore path.
- SDK `Instance.spec.backup` (`InstanceBackupSpec`) — storages, per-storage schedules, PITR flag.
- SDK `DataSource` — create a new Instance from an existing `Backup`.

**Tasks**

- [ ] Add a `backupclasses/mariadb-dump/` definition (`class.yaml` + `types.go`),
      `executionMode: ProviderManaged`, `supportedProviders: [mariadb]`, with
      backup/restore parameter schemas. Mirror the MongoDB layout.
- [ ] Implement `BackupProvider` on `MariaDBProvider`:
  - [ ] `SyncBackup` → create/patch an operator `Backup` CR from S3 `BackupStorage`
        (`c.BackupStorage(name)`), map operator conditions → `BackupExecutionStatus`.
  - [ ] `SyncRestore` → create an operator `Restore` CR (or `bootstrapFrom`) pointing at
        the source backup; handle cross-instance restore via `DataSource`.
  - [ ] `CleanupBackup` / `CleanupRestore` → delete operator CRs, honor finalizers.
- [ ] Implement `BackupMirror` (`Mirror` + `OperatorBackupType`) so scheduled operator
      backups surface as `Backup` CRs. **Design note:** the operator models a schedule as
      a single `Backup` CR with `spec.schedule` (CronJob), not one CR per run — unlike PBM.
      Decide the mapping: one operator `Backup` per SDK schedule, mirroring produced Jobs/
      snapshots back into per-run `Backup` CRs.
- [ ] Wire `spec.backup.storages[].schedules` → operator `Backup.spec.schedule` +
      `maxRetention` (retention copies vs duration — operator uses a duration).
- [ ] Register watches/field indexes (`BackupWatcher`, `FieldIndexProvider`).
- [ ] RBAC for `backups`, `restores` (kubebuilder markers → `make generate` → promote to chart).
- [ ] Integration tests against MinIO (S3): backup → delete data → restore.
- [ ] Update README (backups/restore ❌ → ✅).

---

## 6. Phase 5 — Point-in-time recovery (PITR)

> ⚠️ **Operator constraint — read before committing to scope.**
> The operator's `PointInTimeRecovery` CR (`api/v1alpha1/pointintimerecovery_types.go`)
> **requires a `PhysicalBackupRef` as the base backup** (binlog replay is anchored to a
> physical base), and per `docs/pitr.md` **binlog archival + PITR are currently only
> supported on the asynchronous replication topology** — "Galera and standalone will be
> supported in upcoming releases."
>
> This conflicts with the request of *"PITR, regular for now to S3, not physical."*
> There is no logical-backup-based PITR in the operator today.

**Options**

1. **Defer PITR** until the operator supports it on Galera/standalone, ship logical
   backups (P4) first without PITR. *(Recommended near-term.)*
2. **Enable physical backups + async replication topology** as the PITR foundation:
   - [ ] Add `PhysicalBackup` support (`internal/provider`, new backup class or class option).
   - [ ] Add the `replication` topology (P2 deferred item).
   - [ ] Wire `PointInTimeRecovery` CR (S3 storage, `archiveTimeout`, `strictMode`).
   - [ ] Map `spec.backup.storages[].pitr.enabled` → `PointInTimeRecovery`, advertise
         `providerManaged.supportsPITR: true` on the class and enforce
         `maxPITREnabledStorages`.
   - [ ] Restore-to-timestamp via `bootstrapFrom.targetRecoveryTime`.
3. **Track upstream** for logical-backup PITR / standalone+Galera binlog support and
   revisit.

**Action:** confirm the desired option with stakeholders before implementation. This
roadmap assumes **P4 ships first (logical backups, no PITR)**, and PITR follows via
option 2 once physical backups + a replication topology are in place.

---

## 7. Phase 6 — Observability & exposure hardening

Both are largely working; this phase closes gaps.

**Observability (Prometheus) — mostly ✅**

- [ ] Optional `PrometheusRule` (alerts) alongside the existing `ServiceMonitor`.
- [ ] Ship a reference Grafana dashboard.
- [ ] Ensure metrics work across topologies (per-pod scraping for Galera).

**Exposure — mostly ✅**

- [ ] For HA, expose the **primary** (and optionally **secondary**) Service, not just the
      general one (tie-in with P2).
- [ ] Surface NodePort port(s) and LB ingress in connection details consistently across topologies.

---

## 8. Suggested delivery order

1. **P1 Anti-affinity** — small, unblocks HA quality.
2. **P2 Galera HA** — headline "cluster for HA" capability.
3. **P3 TLS** — transport encryption; independent of HA.
4. **P4 Logical backups + restore (S3)** — largest net-new subsystem.
5. **P6 Observability/exposure hardening** — polish, partly parallelizable.
6. **P5 PITR** — gated on the operator constraint decision (§6); likely last.

## 9. Cross-cutting reminders

- After any `definition/` or RBAC change: `make generate`, then `make verify`
  (CI fails on uncommitted generated diffs).
- Keep the read-modify-write discipline in `SyncMariaDB` — never rebuild the whole
  `MariaDB` spec; overlay only managed fields to avoid endless rolling updates.
- New spec/params must be optional with safe defaults (backward compatibility).
- Add a chainsaw integration suite per phase under `test/integration/`.
- Follow the MongoDB provider (`provider-percona-server-mongodb/`) for patterns:
  backup wiring, status mapping, watches, RBAC promotion.
