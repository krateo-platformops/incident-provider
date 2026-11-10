---
type: API
title: incident-provider — api
description: The Incident and IncidentApply custom resources (observability.krateo.io/v1alpha1) field by field, who writes each field, and the rules the apiserver enforces.
resource: github.com/krateo-platformops/incident-provider
tags: [crd, incident, observability, api]
timestamp: 2026-09-25T00:00:00Z
---

# API

The contract is the `Incident` custom resource, group `observability.krateo.io`,
version `v1alpha1`, plural `incidents`, short name `inc`, categories `krateo`
and `observability`, namespaced, with a status subresource.

The Go types in [`apis/incident/v1alpha1`](../apis/incident/v1alpha1/types.go)
are the source of truth. `make generate` writes the CRD from them into
[`helm/incident-provider-crds/templates/`](../helm/incident-provider-crds/templates/observability.krateo.io_incidents.yaml).
A sample with every field is [`examples/incident`](../examples/incident/README.md).

## Identity

| Item | Value |
|---|---|
| Name | `‹alert name›-‹yyyymmdd-hhmmss›` of the opening firing, one object per incident |
| Namespace | the Alert's namespace, `spec.alertRef.namespace` |
| Label | `observability.krateo.io/alert: ‹spec.alertRef.name›`, set by the writer at creation |

List one alert's incidents with the label, in the alert's namespace:

```sh
kubectl get incidents -n krateo-system -l observability.krateo.io/alert=compositiondefinitions-not-ready
```

`kubectl get incidents` prints `Alert` (`.spec.alertRef.name`), `State` and
`Age`.

## spec

| Field | Type | Written by | Meaning |
|---|---|---|---|
| `alertRef.name` | string, required, 1-63 chars | writer, at creation | the Alert that opened the incident; also the label value, hence 63 |
| `alertRef.namespace` | string, required | writer, at creation | the Alert's namespace |
| `trigger` | `alert` \| `composition-condition` \| `user-ask` | writer | what started the investigation |
| `prompt` | string | writer | the root-cause-analysis prompt sent to incident-agent |
| `triggeredAt` | date-time | writer | when the opening firing arrived |
| `applied` | bool | human: "I applied it", or the portal's Apply; reset by the controller | the apply script ran. The controller consumes it: it moves Open to Verifying, records an `apply` result and sets `applied` back to `false` |
| `closed` | bool | human: the portal's Close; the writer, closing a failed analysis | moves any state to Closed. The writer sets it with the annotation `observability.krateo.io/closed-reason: analysis-failed` in one patch (see `resolution.by`) |
| `reanalysis` | `{feedback}` | human: the portal's Re-run analysis; removed by the writer | asks for a new analysis of an `Open` or `Verifying` incident. The writer consumes it: it records it in `status.reanalysis`, moves the incident back to `Analyzing`, removes the field and runs the analysis again with the previous one, the checks' results and `feedback` (at most 2000 characters). In any other state it is removed unanswered |

## status

Lifecycle and checks:

| Field | Type | Written by | Meaning |
|---|---|---|---|
| `state` | `Analyzing` \| `Open` \| `Verifying` \| `Resolved` \| `Closed` | writer: `Analyzing`, then `Open`; controller: every other transition | see [overview](./overview.md#lifecycle) |
| `howToFix.precondition` | string (bash) | writer | exit `1` while the incident holds, `0` once it is gone; tests the root-cause object, never the alert's rows |
| `howToFix.apply` | string (bash) | writer | the fix; a human runs it, or the controller runs it as the user who creates an [IncidentApply](#incidentapply) |
| `howToFix.verify` | string (bash) | writer | exit `0` once the fix worked, `1` if it did not |
| `howToFix.rollback` | string (bash) | writer | undoes apply; a human runs it to revert an applied fix. Never run by the controller, moves no state |
| `lastChecks.precondition`, `.apply`, `.verify` | `{exit, since}` | controller | the latest result of each script. Moving back to `Open` drops the precondition result, entering `Verifying` the verify result: each describes a state that has moved on |
| `lastChecks.*.exit` | int 0-255 | controller | the exit code; absent for a timeout, for a pod that never ran, and for the `apply` result that records a consumed `spec.applied`. An `apply` result from an IncidentApply run carries the run's exit |
| `lastChecks.*.since` | date-time, required | controller | when the script started giving this result. A precondition or verify run with the same result leaves it unchanged, and with the same verdict the whole status too; every apply run sets it |
| `lastChecks.*.output` | string, at most 512 bytes | controller | for an unknown precondition or verify result only: what the run printed (the end of its log), or why it gave no exit code. Kept from the run that started giving the result |
| `lastChecks.*.noVerdict` | `{runs, since, last}` | controller | for an unknown precondition or verify result only: the streak of consecutive runs without a verdict, whatever their exit codes; `runs` ≥ 1, `since` and `last` when its first and latest runs ended. Each unknown run extends it; a verdict ends it. See [the no-verdict bound](./overview.md#no-verdict) |
| `verifyingSince` | date-time | controller | the start of the settle window: when the incident entered `Verifying`, or was applied again while `Verifying`. Set only while `Verifying` |
| `resolution.by` | `verify` \| `user` \| `alert-gone` \| `analysis-failed`, required | controller | `verify` with `Resolved`; `user`, `alert-gone` or `analysis-failed` with `Closed`. `alert-gone`: the Alert in `spec.alertRef` was deleted. `analysis-failed`: the writer closed the incident because its analysis failed: `spec.closed` carries the annotation `observability.krateo.io/closed-reason: analysis-failed`, and the field managers that own `spec.closed` all own that annotation too (see [overview](./overview.md#closed-by-the-writer)). Any other `spec.closed` is `user` |
| `resolution.at` | date-time, required | controller | when the incident ended |
| `conditions[]` | provider-runtime `Condition` | controller | `Ready` and `Synced` from provider-runtime, plus `Reproduced`, `ChecksUnknown` and `AlertGone` below |

Any other exit code, and a timeout, is unknown: it is recorded and changes no
state; too many in a row stop the script (`ChecksUnknown`).

The root-cause analysis, written by the writer:

| Field | Type | Meaning |
|---|---|---|
| `report` | string (markdown) | the analysis |
| `rootCause.statement` | string | the root cause |
| `rootCause.confidence` | string | a decimal in [0, 1], e.g. `"0.85"` |
| `rootCause.category` | string | `config`, `capacity`, `image`, `network`, `dependency`, `other` |
| `sources[]` | `{type, ref, excerpt}` | the evidence; `type` is `logs` \| `events` \| `metrics` \| `object` |
| `reasoningTrace[]` | `{step, statement, evidenceRefs[]}` | the reasoning; `evidenceRefs` index `sources` from 0 |
| `analyzedResources[]` | `{gvr, name, namespace, whatWasRead}` | the cluster objects the analysis read |
| `missingContext[]` | string | what the analysis could not see |
| `assumptions[]` | string | what it assumed for lack of it |
| `error` | string | why the analysis failed |
| `completedAt` | date-time | when the analysis finished |
| `reanalysis` | `{at, feedback, count}` | the latest `spec.reanalysis` the writer consumed: when, its feedback, and how many reanalyses the incident has had |

A reanalysis clears every field above, `howToFix`, `lastChecks`,
`verifyingSince` and the `Reproduced` condition before the new analysis runs:
they describe the previous one.

## The Reproduced condition

The controller sets it on the first precondition run:

| Status | Reason | Meaning |
|---|---|---|
| `True` | `PreconditionHolds` | the first run exited `1`: the script sees the incident |
| `False` | `PreconditionPassed` | the first run exited `0`: the problem was gone by the first check. The incident moves to `Verifying` and verify decides |

It is a record: neither status stops a script. It is absent until the first
run with a verdict.

## The ChecksUnknown condition

| Status | Reason | Meaning |
|---|---|---|
| `True` | `PreconditionNoVerdict` | the precondition gave no verdict for `noVerdictRuns` runs in a row or for `noVerdictFor`, so it no longer runs. The message has the run count, the last exit code and the start of the last output |
| `True` | `VerifyNoVerdict` | the same for verify, while `Verifying` |
| `False` | `ChecksResumed` | the script it named is no longer current: the state changed, a human applied again, or a reanalysis replaced the scripts |

It is absent until a script hits the bound.

## The AlertGone condition

| Status | Reason | Meaning |
|---|---|---|
| `True` | `AlertNotFound` | the apiserver answered NotFound for the Alert in `spec.alertRef`. Once `True` for the alert-gone grace, the incident is `Closed` with `resolution.by: alert-gone` |
| `False` | `AlertFound` | the Alert exists again |

It is read only while `Open` or `Verifying`, and is absent while the Alert has
always been found.

## Rules the apiserver enforces

| Rule | Message |
|---|---|
| `spec.alertRef` does not change | `alertRef is immutable` |
| `spec.closed` stays `true` once set | `closed cannot be unset` |
| `status.resolution` is set exactly when `state` is `Resolved` or `Closed` | `resolution is set exactly when state is Resolved or Closed` |
| `Resolved` goes with `by: verify`, `Closed` with `by: user`, `by: alert-gone` or `by: analysis-failed` | `Resolved goes with resolution.by verify, Closed with resolution.by user, alert-gone or analysis-failed` |
| a `Resolved` incident only becomes `Closed`; a `Closed` one stays `Closed` | `Resolved can only become Closed, and Closed is final` |

A writer's status patch that breaks a rule is rejected whole. In particular, a
writer finishing an analysis on an incident a human already closed cannot move
it to `Open`.

## IncidentApply

One request to run an Incident's apply script as the user who creates it: group
`observability.krateo.io`, version `v1alpha1`, plural `incidentapplies`, short
name `incapply`, namespaced (the incident's namespace), with a status
subresource. A sample: [`examples/incidentapply`](../examples/incidentapply/README.md).

| Field | Type | Written by | Meaning |
|---|---|---|---|
| `spec.incidentRef.name` | string, required | the creator | the Incident, in this object's namespace |
| `spec.requestedBy.username`, `.groups` | string, []string, required | the `incidentapply-requested-by` admission policy | the user the apiserver authenticated for the create, written over anything the client sent. The script runs as this user |
| `status.phase` | `Running` \| `Succeeded` \| `Failed` \| `Rejected` | controller | absent until the controller acts. `Rejected`: the script never ran; `Failed`: it exited non-zero or did not finish in time |
| `status.message` | string | controller | why it was rejected, or how it ended |
| `status.script` | string (bash) | controller | the apply script as it ran |
| `status.exitCode` | int 0-255 | controller | absent when the run produced none |
| `status.output` | string | controller | the end of the run's stdout and stderr, at most 4096 bytes |
| `status.startedAt`, `status.finishedAt` | time | controller | when the pod was created; when the run ended or the request was rejected |

The controller emits one Event per request when it reaches a terminal phase,
reason `ApplyFinished`, its message starting with the phase
(`Succeeded: the apply script exited 0`). It deletes the request its TTL
(`apply.ttl`, 30 days by default) after `finishedAt`.

`spec` is immutable (`spec is immutable`). `kubectl get incidentapplies` prints
`Incident`, `User`, `Phase`, `Exit` and `Age`.

## Go

- `v1alpha1.Incident` satisfies provider-runtime's `resource.Managed`,
  `v1alpha1.IncidentList` its `resource.ManagedList`.
- `v1alpha1.IncidentApply` satisfies `resource.Managed` too.
- Constants: `RequesterPolicy`, `MaxApplyOutput`, `ReasonApplyFinished`, the `Apply*` phases, `LabelAlert`, the `State*`, `Script*`, `ResolvedBy*`,
  `Trigger*` and `Source*` values, `TypeReproduced`, `TypeChecksUnknown`, `TypeAlertGone` and
  their reasons.
- `apis.AddToScheme` registers the group.
