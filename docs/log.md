---
type: Log
title: incident-provider — log
description: Curated chronological history of incident-provider — notable changes and decisions, newest first.
resource: github.com/krateo-platformops/incident-provider
tags: [controller, history]
timestamp: 2026-11-10T00:00:00Z
---

# Log

Curated history — notable changes and decisions, newest first. Full release
notes live in GitHub Releases. Entries record what was true when written.

## 2026-11-12 — Closed and Resolved incidents are deleted after 30 days (unreleased)

Incidents were never deleted. On demo, 1953 of 1957 were `Closed`, about 150 more a day, each
carrying its analysis prompt (about 20 KB, 37.8 MB of JSON in all). The backlog OOMKilled
alert-provider, which cached them whole (fixed there in 0.2.80), and it grows etcd, every
consumer's cache and the portal's Incidents page without bound. Decided (Diego): keep a
`Closed` or `Resolved` incident 30 days after it ended. Changed:
- **`-retention` / `controller.retention`, default `720h`.** The controller deletes a `Closed` or
  `Resolved` incident once `status.resolution.at` plus the retention has passed, within one poll
  interval, last in its pass and pinned to its UID; its check pods and ConfigMaps go as for any
  deletion. `0` keeps them for ever. `Open`, `Analyzing` and `Verifying` incidents are never
  deleted, nor one with no `resolution.at` (when it ended is unknown).
- **The controller's Role gains `delete` on `incidents`.**
- An incident closed in a pass starts its retention then: `resolution.at` is that pass's time.

Upgrading deletes, within one poll interval, every incident that ended more than 30 days ago.
On demo, today, that is none: every `resolution.at` is from 2026-11-11 or later.

## 2026-11-12 — the check fence needs NetworkPolicy enforcement (docs)

Found on krateo-057, which runs no NetworkPolicy engine: the check
NetworkPolicy exists there but does nothing, so check pods are not kept from
the network, while demo (Calico) enforces it. The docs now say the fence needs
enforcement and that nothing in the chart detects its absence. Read-only RBAC
and `dnsPolicy: None` are unaffected. No code change.

## 2026-11-10 — `resolution.by: analysis-failed` (unreleased)

alert-provider now closes an incident whose analysis finally failed:
`spec.closed: true` and the annotation
`observability.krateo.io/closed-reason: analysis-failed`, in one patch.
incident-provider recorded every `spec.closed` as `resolution.by: user`, and
the portal shows that as closed by a person. Changed:
- **New `resolution.by: analysis-failed`** (additive enum value), with
  `Closed`. The controller sets it when the annotation is present with that
  value and the field managers that own `spec.closed` all own the annotation
  too; any other `spec.closed` stays `user`.
- **Why managedFields.** The annotation alone would turn a person's Close
  into `analysis-failed` whenever the incident carries it: a Close racing the
  writer's patch (the writer's patch of an unchanged `closed` takes no
  ownership, so the person keeps it), or a copied manifest. Field managers are
  client-chosen names, not identities; a person who writes both fields in one
  write is recorded as `analysis-failed`. Accepted: `resolution.by` is a
  record, and whoever may write `spec.closed` may close anyway. With no
  owner of `spec.closed` in `managedFields`, the answer is `user`.

As with `alert-gone`, the CRD chart must be upgraded before the controller:
the old enum rejects a status write with `analysis-failed`.

## 2026-11-09 — bounds for incidents that get no verdict (unreleased)

On one cluster, incidents sat `Open` or `Verifying` for weeks. Three causes,
all at 0.7.0: an unknown result (an exit other than `0`/`1`, or a timeout)
moved nothing, ever, so a broken script parked its incident for good, in
`Open` and in `Verifying`; a first precondition exit `0` flagged the incident
and stopped the precondition until a human acted; and a precondition reading
a deleted Alert exited `2` forever. Changed:
- **No-verdict bound.** Each unknown run extends
  `lastChecks.<script>.noVerdict`; a verdict ends it. At `noVerdictRuns` (6)
  runs or after `noVerdictFor` (24h) the script stops and
  `ChecksUnknown=True` carries the last exit code and output. Applies to the
  precondition in `Open` and verify in `Verifying`. Apply stays human-only.
- **Alert deleted.** While `Open` or `Verifying` the controller reads the
  Alert in `spec.alertRef` once a poll interval. NotFound sets
  `AlertGone=True`; after `alertGoneGrace` (10m) the incident is `Closed`
  with the new `resolution.by: alert-gone`. `Closed`, not `Resolved`, since
  nothing proved the problem gone. The controller needs `get` on `alerts`.
- **First run exits `0`.** The incident moves to `Verifying` and verify
  decides; `Reproduced=False` stays as a record. Incidents flagged by 0.7.0
  run their precondition again.
- **Signals.** `StateChanged` log lines carry `from`, `to`, `why`; new
  `ChecksUnknown` and `AlertGone` Events and log lines.

The CRD change is additive (`noVerdict`, the `alert-gone` enum value), but
the CRD chart must be upgraded before the controller: the old enum rejects a
status write with `alert-gone`, and the old schema prunes `noVerdict`.

Not changed: the check-reader ClusterRole. The `nodes` failure seen on that
cluster was a script calling `kubectl get nodes.core …`: kubectl does not
resolve a `core` group suffix (`the server doesn't have a resource type
"nodes"`), while the role, written from discovery, already grants
`nodes`. Static rules in the chart would also be replaced wholesale on every
helm upgrade, dropping the discovered ones until the next refresh.

## 2026-11-04 — provider-runtime v1.8.2: the Ready contract (unreleased)

provider-runtime v1.8.0 (taken at v1.8.2) sets `Ready` to
`Unknown/NotObserved` when Connect or Observe fails, and expects every
Observe that succeeds to set `Ready` again; one that doesn't gets a
`ReadyNotSetAfterObserve` Warning. On a resource being deleted the runtime
answers `Ready=Deleting` itself: after a successful Observe since v1.8.1, and
after a failed one since v1.8.2. Both controllers already set `Available` on
their main paths. Changed:
- The deletion paths of both Observes set `Deleting`. The runtime doesn't
  require it there; it keeps every successful Observe answering `Ready`.
- The IncidentApply's errors after it set `Available` (reading the run's
  ConfigMap, admission's reads) are wrapped with `reconciler.ReadyAssessed`,
  so the runtime keeps `Available` next to the error.
- The example precondition treats only `Ready=True` as gone. It held only
  on `Ready=False`, so a `NotObserved` CompositionDefinition read as fixed.

Not to be released before core-provider 2.13.56, whose rollup maps
`NotObserved` (core-provider#310), is live on the target cluster.

## 2026-11-03 — 0.6.1: keep the requested-by admission policy

The CRD chart's MutatingAdmissionPolicy `incidentapply-requested-by` and its
binding now carry `helm.sh/resource-policy: keep`, like the two CRDs. Without
it, uninstalling the CRD chart, or swapping it for another chart, deleted them
with no ownership check. Recreated, they are newer than every IncidentApply
already in the cluster, and the controller's `vouched` check refuses each of
those requests. The 0.5.x line gets the same change.

## 2026-11-01 — renamed from incident-controller (unreleased)

The component is now incident-provider, the name the other Krateo providers
use. Releases up to 0.5.2 were published as incident-controller.

Renamed:
- Go module `github.com/krateo-platformops/incident-controller` →
  `github.com/krateo-platformops/incident-provider`. Nothing else imports it.
- Images `ghcr.io/krateo-platformops/incident-controller` and
  `…/incident-controller-check` → `…/incident-provider` and
  `…/incident-provider-check`. The old packages stay in ghcr with their
  history; the new ones start empty.
- Charts `helm/incident-controller` and `helm/incident-controller-crds` →
  `helm/incident-provider` and `helm/incident-provider-crds`: directories,
  Chart `name`, `description` and `sources`, the `_helpers.tpl` template
  names, schema titles, NOTES.txt. Every object name the controller chart
  derives from the chart name follows: the Deployment, the ServiceAccount
  default, the ClusterRoles and bindings (`incident-provider`,
  `-checks`, `-check-reader`, `-credentials`, `-leader-election`) and the
  ConfigMap `incident-provider-check-pod`.
- `service.name` `incident-controller` → `incident-provider`. The log alert
  exclusions in observability and alert-provider match on it and must move
  in the same release, or the "Reconciler error" alert loop returns.
- Env var prefix `INCIDENT_CONTROLLER_` → `INCIDENT_PROVIDER_`. The chart sets
  the new names; a deployment that sets the old ones by hand loses them.
- Leader election Lease `incident-controller.observability.krateo.io` →
  `incident-provider.observability.krateo.io`. The old and new controllers
  don't see each other's lease, so the old one must be stopped before the
  new one starts.
- Check pod template path `/etc/incident-controller/check-pod.yaml` →
  `/etc/incident-provider/check-pod.yaml`, in the flag default and the chart
  mount.
- The `app.kubernetes.io/managed-by` label on check and apply pods
  `incident-controller` → `incident-provider`. Nothing selects on it: the
  controller finds pods by incident or apply UID, the NetworkPolicy by
  `app.kubernetes.io/component: incident-check`, so pods started by the old
  controller are still picked up.
- README, docs and examples.

Kept, deliberately:
- The API: group `observability.krateo.io`, version `v1alpha1`, kinds
  `Incident` and `IncidentApply`. The generated CRDs are byte-identical to
  before; only their directory moved. Clusters hold live Incidents (633 across
  two clusters on 2026-11-01), and deleting a CRD deletes every object of it.
  Both CRDs and both user ClusterRoles still carry `helm.sh/resource-policy: keep`
  (since 0.5.2) in their new location.
- The CRD side's installer identity (composition `incident-controller-crd`,
  kind `IncidentControllerCrds`) is outside this repo. Installer keeps it so
  the CRD release is upgraded in place to the renamed chart, never
  uninstalled. The charts derive names from the chart name, not the release
  name, so any release name works.
- The ClusterRoles `krateo-incident-viewer` and `krateo-incident-responder`:
  portal's `sre-incident-responder` binding points at the responder role by
  name.
- Label and annotation keys under `observability.krateo.io/`, the component
  label `incident-check`, and the admission policy `incidentapply-requested-by`.
- Past release notes and tags (0.2.0 … 0.5.2) as published.

Corrected: the 0.5.2 README said that deleting a Krateo composition that
installed the CRD chart still deleted the CRDs, because core-provider deleted
every release object with finalizers before uninstalling. It doesn't:
core-provider's teardown reads each object and skips it when it has no
finalizers, and the installed CRDs have none at rest. So keep protects the
CRDs and the user ClusterRoles on composition deletion as well as on
`helm uninstall`.
