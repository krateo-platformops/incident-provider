---
type: Architecture
title: incident-provider — overview
description: What an Incident is, its lifecycle, who writes which part of it, and how the controller maps onto provider-runtime.
resource: github.com/krateo-platformops/incident-provider
tags: [crd, incident, observability, provider-runtime, architecture]
timestamp: 2026-09-25T00:00:00Z
---

# Overview

An **Incident** is one occurrence of an Alert's problem, from its root-cause
analysis to its end. An Alert (`alerts.observability.krateo.io`, owned by
alert-provider) opens Incidents; it never closes them. Each Incident carries
its own checks as bash scripts, and ends when a script proves the problem gone,
a human closes it, its Alert is deleted, or its analysis fails.

## Who writes what

| Actor | Writes |
|---|---|
| alert-provider (the writer) | creates the Incident with its label and `spec`; writes the analysis and `howToFix`; sets `Analyzing`, then `Open`; consumes `spec.reanalysis`; closes a failed analysis: `spec.closed` with the annotation `observability.krateo.io/closed-reason: analysis-failed` |
| incident-provider | runs precondition and verify; writes `lastChecks`, `verifyingSince`, every other state transition, `resolution` and the conditions; sets `spec.reanalysis` (`cause: ApplyActionStale`) once when the cluster stops serving `applyAction`'s target |
| a human, through the portal | `spec.applied` ("I applied it", or Apply), `spec.closed` (Close) and `spec.reanalysis` (Re-run analysis), with their own token and the `krateo-incident-responder` role; an IncidentApply (Run apply), which has the controller run the apply script with their own rights |

Field by field: [api](./api.md).

## Many incidents per alert

The writer evaluates each firing alert about every 60 s. A firing that an
open incident (any state but `Resolved` and `Closed`) describes opens nothing
and writes nothing. An LLM compares the firing with each open incident to
decide. When none is the same problem, the firing opens a new incident with its
own analysis, so an alert can have several open incidents at once. An incident
still `Analyzing`, or whose analysis failed, takes the firing without a
comparison. [The writer's docs](https://github.com/krateo-platformops/alert-troubleshooter/blob/main/docs/overview.md)
have the details.

## Lifecycle

```
Analyzing ──▶ Open ◀──▶ Verifying ──▶ Resolved
   ▲           │            │
   └───────────┴────────────┘ spec.reanalysis
any state ──spec.closed──▶ Closed
Open, Verifying ──Alert deleted──▶ Closed
```

| From | To | On |
|---|---|---|
| — | `Analyzing` | the writer's first status write |
| `Analyzing` | `Open` | the analysis finished, or failed with `error` set |
| `Open` | `Verifying` | precondition exit `0`, the first run included; `spec.applied`, which the controller consumes: it records an `apply` result and sets `applied` back to `false`; or an IncidentApply run that exits `0` |
| `Verifying` | `Resolved` | verify exit `0`, at any time |
| `Verifying` | `Open` | verify exit `1` once the settle window has passed |
| `Open`, `Verifying` | `Analyzing` | `spec.reanalysis`, which the writer consumes: it clears the analysis and the checks' results and runs the analysis again |
| any | `Closed` | `spec.closed: true`: `resolution.by: user`, or `analysis-failed` when the writer closed a failed analysis (see [Closed by the writer](#closed-by-the-writer)) |
| `Open`, `Verifying` | `Closed` | the Alert in `spec.alertRef` NotFound for the alert-gone grace (10 minutes by default): `resolution.by: alert-gone` |

The settle window (5 minutes by default) gives a fix time to take effect. It
starts when the incident enters `Verifying` (`status.verifyingSince`): at the
precondition exit `0` or the apply that moved it. Inside the window a verify
exit `1` is recorded and verify runs again at the next poll; the first exit `1`
at or after the window's end moves the incident back to `Open`. An exit other
than `0` or `1`, or a timeout, never moves it, inside the window or after. A
human applying again while `Verifying`, or an IncidentApply run that exits `0`,
restarts the window; a failed IncidentApply run is recorded but does not.

`Resolved` means a script proved the problem gone; `Closed` means a person
decided, the Alert was deleted, or the analysis failed. `Closed` is final, and a `Resolved`
incident can only become `Closed`.

The first precondition run with a verdict sets `Reproduced`: `True` on exit
`1`, `False` on exit `0`. An exit `0` there means the problem was gone by the
first check: the incident moves to `Verifying` like any other precondition
exit `0`, and verify decides. Verify exit `0` resolves it; exit `1` after the
settle window sends it back to `Open`, where the precondition runs again.
`Reproduced=False` stays as a record. (Up to 0.7.0 such an incident was
flagged: it stayed `Open` with its precondition stopped until a human acted.
An incident flagged that way runs its precondition again.)

### No verdict

A precondition or verify run that exits other than `0` or `1`, or times out,
gives no verdict and moves no state. Each such run extends the script's
streak, `lastChecks.<script>.noVerdict` (`runs`, `since`, `last`); a verdict
ends it. When the streak reaches `noVerdictRuns` runs (6 by default) or lasts
`noVerdictFor` (24 hours by default), the script stops and the incident gets
`ChecksUnknown=True`, reason `PreconditionNoVerdict` (`Open`) or
`VerifyNoVerdict` (`Verifying`), whose message carries the run count, the last
exit code and the start of the last output. The incident stays in its state:
stuck is now visible, not silent. `spec.applied`, `spec.closed`,
`spec.reanalysis` (new scripts) or an IncidentApply run that exits `0` moves
it; the condition then turns `False`, reason `ChecksResumed`. An unknown
result recorded by 0.7.0 or earlier starts its streak at its `since`, so an incident
stuck for longer than `noVerdictFor` stops at its next run.

### Alert deleted

While `Open` or `Verifying`, the controller reads the Alert in
`spec.alertRef` once a poll interval. A NotFound that names the Alert sets
`AlertGone=True`; the Alert found again sets it `False`. Once `AlertGone` has
been `True` for the alert-gone grace, the incident is `Closed` with
`resolution.by: alert-gone`. `Closed`, not `Resolved`: nothing proved the
problem gone. A missing Alert CRD, a denied read or an unreachable apiserver
change nothing.

### Apply target not served

While `Open` or `Verifying`, the controller reads from API discovery whether
the cluster serves the `apiVersion` and `resource` of `howToFix.applyAction`,
once a poll interval, and sets `ApplyActionStale` (see
[the API](./api.md#the-applyactionstale-condition)). A composition's CRD
stops serving old versions as its chart is upgraded, so an analysis written
against one goes stale. The controller never rewrites `applyAction`, whose
payload is bound to the old version's schema: when the condition becomes
`True` it asks for a reanalysis itself, once per episode
([the automatic reanalysis](./api.md#the-automatic-reanalysis)), and the
writer's new analysis writes a new `applyAction`. It reads discovery uncached: the manager's RESTMapper learns new versions but
keeps answering for one the cluster no longer serves. A failed read changes
nothing.

### Closed by the writer

The writer closes an incident whose analysis failed (no scripts that can run)
with one object patch: `spec.closed: true` and the annotation
`observability.krateo.io/closed-reason: analysis-failed`. The controller
records `resolution.by: analysis-failed` for it, `user` for any other
`spec.closed`.

The annotation alone does not say who closed: a person can close an incident
that carries it, one the writer annotated in a race with their Close, or one
copied from another incident. So it counts only when the field managers that
own `spec.closed` in `metadata.managedFields` all own the annotation too:
whoever set `closed` also set the reason. The apiserver gives a field to the
manager of the write that changed it, so a person's Close is owned by their
client (`kubectl-edit`, the portal's), and the writer's later patch of the
same value takes nothing from it. Without `managedFields` that name an owner
of `spec.closed`, the answer is `user`, as before `analysis-failed` existed.

Field managers are names clients choose, not identities: a person who writes
both fields in one write, or under the writer's manager name, is recorded as
`analysis-failed`. `resolution.by` is a record, not a grant: anyone who may
write `spec.closed` may close the incident anyway.

A failed analysis the writer did not close (an older writer, or a refused
close) stays `Open` with `error` set; a human can close it. An incident with
no `howToFix` scripts gets no checks run.

## The scripts

`status.howToFix` holds bash scripts written by the analysis:

| Script | Run by | When |
|---|---|---|
| `precondition` | the controller, in a read-only sandbox | every poll interval while `Open` |
| `apply` | a human: their own terminal, or the portal's Apply; or the controller, as the user who asks with Run apply | when they decide |
| `verify` | the controller, in a read-only sandbox | while `Verifying` |
| `rollback` | a human, to undo apply | when an applied fix must be reverted; it moves no state |

Exit `0` means the incident is gone, `1` that it holds; any other exit, or a
timeout, is unknown and changes no state, up to the [no-verdict
bound](#no-verdict). The precondition tests the
root-cause object, never the alert's rows, so it still reports the right
answer when the alert fires for another reason.

## On provider-runtime

The Incident is a provider-runtime managed resource: it embeds provider-runtime's
`ConditionedStatus` and satisfies `resource.Managed`. It has no external
resource: Observe always reports it existing, and decides from the Incident
whether there is work to do. Check pods only carry the runs, and are where a
run's result is read.

| provider-runtime | incident-provider |
|---|---|
| `ExternalClient.Observe` | reads the incident's check pods; records a finished one in `status.lastChecks` and applies its transition; applies `spec.closed` and `spec.applied`; reads the incident's Alert once a poll interval while `Open` or `Verifying`. A status to persist, a check pod to delete or a check that is due make the Incident out of date. It changes nothing but the Incident held in memory |
| `Create` | never called |
| `Update` | persists the status when it changed, then sets `spec.applied` back to `false`, deletes finished and stale check pods, and starts the next check when it is due: precondition when `Open`, verify when `Verifying` |
| `Delete` + finalizer | the Incident is deleted: removes its check pods and ConfigMaps |
| `WithPollInterval(1m)` | the 60 s check cadence; a poll-interval hook requeues when the next check falls due |
| `krateo.io/paused` | pauses one incident: no checks, no transitions, `spec.closed` included |

`Ready` follows provider-runtime's Ready contract (since v1.8.0). Every Observe
that succeeds sets it: `Available`, or `Deleting` once the Incident or
IncidentApply is deleted. When Observe cannot read the check or apply pods,
it returns a plain error, and the runtime sets `Ready` to
`Unknown/NotObserved` next to `Synced=False`, or to `Deleting` on a deleted
object. An IncidentApply is `Available`
once its pod is read; a later read that fails, while admitting or recording
the run, fails the reconcile but keeps `Available`: the error is wrapped with
`reconciler.ReadyAssessed`.

A check is due a poll interval after the last one ended, or after the
incident entered `Verifying` when that is later. The controller holds the end
of each incident's last run in memory, so after a restart every check is due at
once. A check pod's watch events requeue its incident, so a finished check is
read at once. Update writes the status before anything else, so a result is
never lost with its pod and a consumed `spec.applied` is never lost with its
flag. A result whose pod outlives a failed delete is recorded again and changes
nothing. A running check of a script that is no longer current (the incident
was applied or closed meanwhile) is deleted and its result dropped.

### Writes

`status.lastChecks` keeps one result per script, with `since`: when the script
started giving it. A precondition or verify run that gives the same verdict as
the last leaves the status as it is, so the controller writes nothing. An Open
incident whose precondition keeps exiting `1` is not written to until the
result changes, a human acts, or it is closed; a watcher of Incidents sees no
event from it. A result that changes, a transition, and every apply run are
one status write each. An unknown run is one too, since it extends the
no-verdict streak; the bound caps them.

### Signals

Every transition and condition change the controller makes is a Kubernetes
Event on the Incident and an INFO log line (`service.name: incident-provider`)
with the attributes `incident` (`namespace/name`) and `reason`:

| `reason` | Attributes | When |
|---|---|---|
| `StateChanged` | `from`, `to`, `why` | every state transition the controller makes; the message is `‹from› -> ‹to›: ‹why›` |
| `ChecksUnknown` | `state`, `script`, `exit`, `runs` | a script hit the no-verdict bound and stopped |
| `AlertGone` | `state`, `alert` | the Alert in `spec.alertRef` was first found missing |
| `ApplyActionStale` | `state`, `apiVersion`, `resource` | `ApplyActionStale` became `True`: the cluster does not serve the target of `howToFix.applyAction`; the message is the condition's |
| `ApplyActionServed` | `state`, `apiVersion`, `resource` | `ApplyActionStale` went from `True` to `False` |
| `ReanalysisRequested` | `cause`, `state` | the controller set `spec.reanalysis` (`cause: ApplyActionStale`); the message ends with the condition's |

To alert on stuck incidents, match `ChecksUnknown=True` on the Incident, or
the `ChecksUnknown` log line; for incidents left `Open` too long, compare the
time of the last `StateChanged` with `to: Open` (or the Incident's
`creationTimestamp`) with now. The writer's transitions (`Analyzing`, then
`Open`) are logged by alert-provider, not here.

## Run apply

An **IncidentApply** asks the controller to run one Incident's apply script as
the user who creates it. The portal's Run apply creates one, as the clicking
user, through snowplow.

1. **Who asked.** The `incidentapply-requested-by` MutatingAdmissionPolicy, in
   the CRD chart, writes the user the apiserver authenticated for the create
   (`request.userInfo`) into `spec.requestedBy`, over anything the client sent.
   The CRD keeps `spec` immutable after that.
2. **Admission.** The controller runs the script only when the policy and its
   binding existed before the request, the incident is `Open` with an apply
   script, no other request for it is pending or running, the user may `patch`
   the incident (a SubjectAccessReview), and the user's login credentials
   exist, are theirs and outlast the run. Otherwise the request is `Rejected`
   with the reason.
3. **The run.** The controller reads the user's `<username>-clientconfig`
   Secret, the client certificate authn issued them at login, and writes a
   kubeconfig from it into a Secret owned by the apply pod. The apply pod is a
   check pod with no service account token: kubectl reaches the apiserver by
   IP as that user, so RBAC judges every command on the user's own rights.
4. **The result.** The exit code and the end of the output go into the
   request's status, and one `ApplyFinished` Event. The run is also an `apply`
   check on the incident with its exit; exit `0` moves an `Open` incident to
   `Verifying`, as `spec.applied` does. The apply pod, its Secret and its
   ConfigMap are deleted once the run ends.
5. **Retention.** The controller deletes a finished IncidentApply its TTL
   (`apply.ttl`, 30 days by default) after `finishedAt`.

## Check pods

Each run is a pod in a dedicated namespace (`krateo-incident-checks` by
default), holding only the check pods and their read-only service account:

- the script from a ConfigMap owned by the pod, run with bash; the image has
  bash, kubectl and jq;
- the service account `incident-check`, bound to `view` and to
  `incident-provider-check-reader`, which reads every resource but Secrets
  and subresources, its rules kept current from discovery by the controller;
  no writes;
- no DNS, and a NetworkPolicy allowing egress to the apiserver only, and no
  ingress (effective only where the cluster enforces NetworkPolicy; see
  [configuration](./configuration.md));
- `activeDeadlineSeconds: 60`, and the `restricted` Pod Security Standard.

Apply pods differ in one way: they mount no service account token, and
kubectl uses the requester's kubeconfig. Details: [configuration](./configuration.md).

## Layout

| Path | What |
|---|---|
| `apis/incident/v1alpha1` | the Go types, the source of truth |
| `helm/incident-provider-crds` | the CRD chart: the CRDs, generated from the Go types, and the `incidentapply-requested-by` admission policy |
| `helm/incident-provider` | the controller chart: Deployment, RBAC, the checks namespace, the check pod template, the NetworkPolicy, the unbound `krateo-incident-viewer` and `krateo-incident-responder` roles |
| `main.go`, `internal/controllers/incident` | the controller: `machine.go` is the state machine, `checkpod.go` the check and apply pods, `incident.go` the Incident's provider-runtime client, `apply.go` the IncidentApply's |
| `check/Dockerfile` | the check pod image |
| `examples/incident`, `examples/incidentapply` | a sample Incident and IncidentApply, validated by the unit tests |
