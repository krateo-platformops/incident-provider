package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
)

// LabelAlert carries the name of the Alert that opened an Incident. Writers set it at creation,
// with the value of spec.alertRef.name, so one alert's incidents list with a label selector in the
// alert's namespace.
const LabelAlert = "observability.krateo.io/alert"

// State is the lifecycle state of an Incident.
// +kubebuilder:validation:Enum=Analyzing;Open;Verifying;Resolved;Closed
type State string

const (
	// StateAnalyzing: the root-cause analysis runs. No check runs.
	StateAnalyzing State = "Analyzing"
	// StateOpen: the incident holds. The precondition runs every poll interval, until it gives no
	// verdict for too long (condition ChecksUnknown).
	StateOpen State = "Open"
	// StateVerifying: the precondition exited 0, or a human applied the fix. The verify script
	// runs, until it gives no verdict for too long (condition ChecksUnknown).
	StateVerifying State = "Verifying"
	// StateResolved: the verify script exited 0.
	StateResolved State = "Resolved"
	// StateClosed: a human closed the incident, or the controller did because its Alert was
	// deleted.
	StateClosed State = "Closed"
)

// Trigger is what started the investigation.
// +kubebuilder:validation:Enum=alert;composition-condition;user-ask
type Trigger string

const (
	TriggerAlert                Trigger = "alert"
	TriggerCompositionCondition Trigger = "composition-condition"
	TriggerUserAsk              Trigger = "user-ask"
)

// Script names a how-to-fix script whose result status.lastChecks records. Rollback is never
// checked.
// +kubebuilder:validation:Enum=precondition;apply;verify
type Script string

const (
	ScriptPrecondition Script = "precondition"
	ScriptApply        Script = "apply"
	ScriptVerify       Script = "verify"
)

// ResolvedBy is what ended an incident.
// +kubebuilder:validation:Enum=verify;user;alert-gone
type ResolvedBy string

const (
	// ResolvedByVerify: the verify script exited 0. Goes with state Resolved.
	ResolvedByVerify ResolvedBy = "verify"
	// ResolvedByUser: a human closed the incident. Goes with state Closed.
	ResolvedByUser ResolvedBy = "user"
	// ResolvedByAlertGone: the Alert in spec.alertRef was deleted, and stayed deleted for the
	// alert-gone grace. Goes with state Closed: nothing proved the problem gone.
	ResolvedByAlertGone ResolvedBy = "alert-gone"
)

// SourceType is the kind of a piece of evidence.
// +kubebuilder:validation:Enum=logs;events;metrics;object
type SourceType string

const (
	SourceLogs    SourceType = "logs"
	SourceEvents  SourceType = "events"
	SourceMetrics SourceType = "metrics"
	SourceObject  SourceType = "object"
)

// TypeReproduced is a condition on the first precondition run with a verdict. True: it exited 1,
// so the script sees the incident. False: it exited 0, so the problem was gone by the first check;
// the incident moves to Verifying and verify decides. It is a record only: neither status stops a
// script. Absent until the first verdict.
const TypeReproduced prv1.ConditionType = "Reproduced"

// Reasons of the Reproduced condition.
const (
	ReasonPreconditionHolds  prv1.ConditionReason = "PreconditionHolds"
	ReasonPreconditionPassed prv1.ConditionReason = "PreconditionPassed"
)

// TypeChecksUnknown is a condition on the current script. True: it gave no verdict (no exit 0 or
// 1) in too many consecutive runs, or for too long, so it no longer runs; the message carries the
// last exit code and output. Only spec.applied, spec.closed, spec.reanalysis or an apply run that
// exits 0 moves such an incident. False: the script it named is no longer current. Absent while
// no script has hit the bound.
const TypeChecksUnknown prv1.ConditionType = "ChecksUnknown"

// Reasons of the ChecksUnknown condition.
const (
	// ReasonPreconditionNoVerdict: True, the precondition hit the bound while Open.
	ReasonPreconditionNoVerdict prv1.ConditionReason = "PreconditionNoVerdict"
	// ReasonVerifyNoVerdict: True, verify hit the bound while Verifying.
	ReasonVerifyNoVerdict prv1.ConditionReason = "VerifyNoVerdict"
	// ReasonChecksResumed: False, the script that hit the bound is no longer current: the state
	// changed, or a reanalysis replaced the scripts.
	ReasonChecksResumed prv1.ConditionReason = "ChecksResumed"
)

// TypeAlertGone is a condition on the Alert in spec.alertRef, read while the incident is Open or
// Verifying. True: the API server answered NotFound for it. Once True for the alert-gone grace,
// the controller closes the incident with resolution.by alert-gone. False: the Alert exists again.
// Absent while the Alert has always been found.
const TypeAlertGone prv1.ConditionType = "AlertGone"

// Reasons of the AlertGone condition.
const (
	ReasonAlertNotFound prv1.ConditionReason = "AlertNotFound"
	ReasonAlertFound    prv1.ConditionReason = "AlertFound"
)

// AlertRef locates the Alert that opened an Incident.
type AlertRef struct {
	// Name of the Alert. It is also the value of the observability.krateo.io/alert label, so it is
	// at most 63 characters.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// Namespace of the Alert. Writers create the Incident in this namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
}

// IncidentSpec is what opened the incident and what humans decided about it.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.closed) || !oldSelf.closed || (has(self.closed) && self.closed)",message="closed cannot be unset"
type IncidentSpec struct {
	// AlertRef is the Alert that opened the incident. It is immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="alertRef is immutable"
	AlertRef AlertRef `json:"alertRef"`

	// Trigger is what started the investigation.
	// +optional
	Trigger Trigger `json:"trigger,omitempty"`

	// Prompt is the root-cause-analysis prompt sent to incident-agent.
	// +optional
	Prompt string `json:"prompt,omitempty"`

	// TriggeredAt is when the firing that opened the incident arrived.
	// +optional
	TriggeredAt *metav1.Time `json:"triggeredAt,omitempty"`

	// Applied says a human ran the apply script: in a terminal followed by "I applied it", or with
	// the portal's Apply. The controller consumes it: it moves an Open incident to Verifying,
	// records an apply result and sets applied back to false.
	// +optional
	Applied bool `json:"applied,omitempty"`

	// Closed says a human closed the incident. It moves any state to Closed and cannot be unset.
	// +optional
	Closed bool `json:"closed,omitempty"`

	// Reanalysis asks for a new root-cause analysis of an Open or Verifying incident. The writer
	// (alert-provider) consumes it: it records it in status.reanalysis, moves the incident back to
	// Analyzing, removes this field and runs the analysis again. In any other state it is removed
	// unanswered.
	// +optional
	Reanalysis *ReanalysisRequest `json:"reanalysis,omitempty"`
}

// ReanalysisRequest is a request for a new root-cause analysis.
type ReanalysisRequest struct {
	// Feedback is what the person asking found wrong with the analysis, for the agent to address.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Feedback string `json:"feedback,omitempty"`
}

// Reanalysis records the latest consumed spec.reanalysis: the request the current analysis
// answers.
type Reanalysis struct {
	// At is when the request was consumed.
	At metav1.Time `json:"at"`

	// Feedback is the request's feedback.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Feedback string `json:"feedback,omitempty"`

	// Count is how many reanalyses the incident has had.
	// +kubebuilder:validation:Minimum=1
	Count int32 `json:"count"`
}

// HowToFix is the fix as bash scripts, written by the root-cause analysis. The controller runs
// precondition and verify in a read-only sandbox; a human runs apply, and rollback to undo it. For
// precondition and verify, exit 0 means the incident is gone and exit 1 that it holds; any other
// exit, or a timeout, is unknown and changes no state. A script that stays unknown too long stops
// (condition ChecksUnknown).
type HowToFix struct {
	// Precondition tests whether the incident still holds. It tests the root-cause object, never
	// the alert's rows.
	// +optional
	Precondition string `json:"precondition,omitempty"`

	// Apply is the fix. A human reviews and runs it.
	// +optional
	Apply string `json:"apply,omitempty"`

	// Verify tests whether the fix worked.
	// +optional
	Verify string `json:"verify,omitempty"`

	// Rollback undoes apply: it restores what apply changed. A human runs it to revert an applied
	// fix. The controller never runs it, and running it moves no state.
	// +optional
	Rollback string `json:"rollback,omitempty"`

	// ApplyAction is apply as one Kubernetes API write, when it is one. The portal's Apply sends it
	// as the clicking user, then sets spec.applied. The controller never sends it.
	// +optional
	ApplyAction *ApplyAction `json:"applyAction,omitempty"`
}

// ApplyAction is one Kubernetes API write: the object at apiVersion, resource, namespace and name,
// and what to send it.
type ApplyAction struct {
	// Verb is patch (payload is a JSON merge patch), create (payload is the object) or delete.
	// +kubebuilder:validation:Enum=patch;create;delete
	Verb string `json:"verb"`

	// APIVersion is the target's v1 or <group>/<version>.
	APIVersion string `json:"apiVersion"`

	// Resource is the target's lowercase plural resource name.
	Resource string `json:"resource"`

	// Namespace is the target's namespace; empty for a cluster-scoped object.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the target's name; for create, the new object's.
	Name string `json:"name"`

	// Payload is the body: the merge patch, or the object to create. A delete has none.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	// +optional
	Payload *runtime.RawExtension `json:"payload,omitempty"`
}

// LastCheck is the latest result of a script.
type LastCheck struct {
	// Exit is the script's exit code. It is absent when the run produced none (a timeout, a pod
	// that never ran) and on the apply result that records a consumed spec.applied.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	// +optional
	Exit *int32 `json:"exit,omitempty"`

	// Since is when the script started giving this result. A precondition or verify run that gives
	// the same result again leaves it unchanged; every apply run sets it.
	Since metav1.Time `json:"since"`

	// Output is what an unknown precondition or verify run printed (the end of its log, cut to 512
	// bytes), or why it gave no exit code. It is set only for unknown results, from the run that
	// started giving the result.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	Output string `json:"output,omitempty"`

	// NoVerdict is the streak of consecutive precondition or verify runs without a verdict that
	// ends at this result, whatever their exit codes. It is set only for unknown results; a
	// verdict, 0 or 1, ends the streak.
	// +optional
	NoVerdict *NoVerdict `json:"noVerdict,omitempty"`
}

// NoVerdict is a streak of consecutive runs of one script that gave no exit 0 or 1.
type NoVerdict struct {
	// Runs is how many runs the streak has.
	// +kubebuilder:validation:Minimum=1
	Runs int32 `json:"runs"`

	// Since is when the first run of the streak ended.
	Since metav1.Time `json:"since"`

	// Last is when the latest run of the streak ended. A run that ended no later is not counted
	// again.
	Last metav1.Time `json:"last"`
}

// LastChecks holds the latest result of each script.
type LastChecks struct {
	// +optional
	Precondition *LastCheck `json:"precondition,omitempty"`

	// +optional
	Apply *LastCheck `json:"apply,omitempty"`

	// +optional
	Verify *LastCheck `json:"verify,omitempty"`
}

// Resolution records how an incident ended.
type Resolution struct {
	// By is what ended it: verify (state Resolved), or user or alert-gone (state Closed).
	By ResolvedBy `json:"by"`

	// At is when it ended.
	At metav1.Time `json:"at"`
}

// RootCause is the conclusion of the root-cause analysis.
type RootCause struct {
	// Statement is the root cause.
	// +optional
	Statement string `json:"statement,omitempty"`

	// Confidence is a decimal string in [0, 1], e.g. "0.85".
	// +optional
	Confidence string `json:"confidence,omitempty"`

	// Category is the failure class: config, capacity, image, network, dependency, other.
	// +optional
	Category string `json:"category,omitempty"`
}

// Source is one piece of evidence the analysis rests on.
type Source struct {
	// Type is the kind of evidence.
	// +optional
	Type SourceType `json:"type,omitempty"`

	// Ref is where the evidence came from: a query, an object reference, a metric name.
	// +optional
	Ref string `json:"ref,omitempty"`

	// Excerpt is the relevant part of the evidence, verbatim.
	// +optional
	Excerpt string `json:"excerpt,omitempty"`
}

// ReasoningStep is one step of the reasoning trace.
type ReasoningStep struct {
	// Step is the 1-based position of this step.
	// +optional
	Step int32 `json:"step,omitempty"`

	// Statement is what this step concludes.
	// +optional
	Statement string `json:"statement,omitempty"`

	// EvidenceRefs are the 0-based indices into status.sources that back this step.
	// +optional
	EvidenceRefs []int32 `json:"evidenceRefs,omitempty"`
}

// AnalyzedResource is a cluster object the analysis read.
type AnalyzedResource struct {
	// GVR is the object's group/version/resource, e.g. apps/v1/deployments.
	// +optional
	GVR string `json:"gvr,omitempty"`

	// +optional
	Name string `json:"name,omitempty"`

	// +optional
	Namespace string `json:"namespace,omitempty"`

	// WhatWasRead is what of the object the analysis inspected: status, events, spec, ...
	// +optional
	WhatWasRead string `json:"whatWasRead,omitempty"`
}

// IncidentStatus is the incident's lifecycle, its checks and its root-cause analysis.
// +kubebuilder:validation:XValidation:rule="has(self.resolution) == (has(self.state) && self.state in ['Resolved', 'Closed'])",message="resolution is set exactly when state is Resolved or Closed"
// +kubebuilder:validation:XValidation:rule="!has(self.resolution) || (has(self.state) && ((self.resolution.by == 'verify') == (self.state == 'Resolved')))",message="Resolved goes with resolution.by verify, Closed with resolution.by user or alert-gone"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.state) || !(oldSelf.state in ['Resolved', 'Closed']) || (has(self.state) && (self.state == oldSelf.state || self.state == 'Closed'))",message="Resolved can only become Closed, and Closed is final"
type IncidentStatus struct {
	prv1.ConditionedStatus `json:",inline"`

	// State is the lifecycle state. Writers set Analyzing in the first status write.
	// +optional
	State State `json:"state,omitempty"`

	// HowToFix is the fix the root-cause analysis wrote.
	// +optional
	HowToFix *HowToFix `json:"howToFix,omitempty"`

	// LastChecks is the latest result of each script. It changes only when a result does, so a
	// check that keeps giving the same verdict writes nothing; each unknown run is counted in its
	// noVerdict streak, up to the bound that stops the script.
	// +optional
	LastChecks *LastChecks `json:"lastChecks,omitempty"`

	// VerifyingSince is when the incident last entered Verifying, or was applied again while
	// Verifying: the start of the settle window. It is set only while Verifying.
	// +optional
	VerifyingSince *metav1.Time `json:"verifyingSince,omitempty"`

	// Resolution records how the incident ended. It is set exactly when state is Resolved or Closed.
	// +optional
	Resolution *Resolution `json:"resolution,omitempty"`

	// Report is the root-cause analysis in markdown.
	// +optional
	Report string `json:"report,omitempty"`

	// RootCause is the analysis' conclusion.
	// +optional
	RootCause *RootCause `json:"rootCause,omitempty"`

	// Sources is the evidence the analysis rests on. reasoningTrace steps cite it by 0-based index.
	// +optional
	Sources []Source `json:"sources,omitempty"`

	// ReasoningTrace is the ordered reasoning, each step citing its sources.
	// +optional
	ReasoningTrace []ReasoningStep `json:"reasoningTrace,omitempty"`

	// AnalyzedResources are the cluster objects the analysis read.
	// +optional
	AnalyzedResources []AnalyzedResource `json:"analyzedResources,omitempty"`

	// MissingContext is what the analysis could not see but needed.
	// +optional
	MissingContext []string `json:"missingContext,omitempty"`

	// Assumptions are what the analysis assumed for lack of context.
	// +optional
	Assumptions []string `json:"assumptions,omitempty"`

	// Error is why the root-cause analysis failed.
	// +optional
	Error string `json:"error,omitempty"`

	// CompletedAt is when the root-cause analysis finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Reanalysis is the latest spec.reanalysis the writer consumed.
	// +optional
	Reanalysis *Reanalysis `json:"reanalysis,omitempty"`
}

// +kubebuilder:object:root=true

// An Incident is one occurrence of an alert's problem, from its root-cause analysis to its end. An
// Alert has any number of Incidents: while it fires, a firing an open Incident describes opens
// nothing, and a firing none describes opens a new one. Its own checks resolve it, or a
// human closes it; the Alert returning to OK does neither.
// +kubebuilder:printcolumn:name="Alert",type="string",JSONPath=".spec.alertRef.name"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.state"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=inc,categories={krateo,observability}
// +kubebuilder:metadata:annotations="helm.sh/resource-policy=keep"
type Incident struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IncidentSpec   `json:"spec"`
	Status IncidentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IncidentList contains a list of Incident.
type IncidentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Incident `json:"items"`
}

var (
	_ resource.Managed     = &Incident{}
	_ resource.ManagedList = &IncidentList{}
)

// GetCondition of this Incident.
func (mg *Incident) GetCondition(ct prv1.ConditionType) prv1.Condition {
	return mg.Status.GetCondition(ct)
}

// SetConditions of this Incident.
func (mg *Incident) SetConditions(c ...prv1.Condition) {
	mg.Status.SetConditions(c...)
}

// GetItems of this IncidentList.
func (l *IncidentList) GetItems() []resource.Managed {
	items := make([]resource.Managed, len(l.Items))
	for i := range l.Items {
		items[i] = &l.Items[i]
	}
	return items
}
