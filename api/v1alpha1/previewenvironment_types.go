package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PRState is the state of the pull request as last reported by the SCM.
// +kubebuilder:validation:Enum=open;closed
type PRState string

const (
	// PRStateOpen means the pull request is open and wants a preview.
	PRStateOpen PRState = "open"
	// PRStateClosed means the pull request was closed or merged.
	PRStateClosed PRState = "closed"
)

// Phase is the coarse lifecycle phase of a preview environment.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed;TearingDown;Closed;Expired
type Phase string

// Lifecycle phases. Closed and Expired are terminal for a given PR state;
// a reopen or a new push moves an Expired/Closed environment back to Pending.
const (
	PhasePending      Phase = "Pending"
	PhaseProvisioning Phase = "Provisioning"
	PhaseReady        Phase = "Ready"
	PhaseFailed       Phase = "Failed"
	PhaseTearingDown  Phase = "TearingDown"
	PhaseClosed       Phase = "Closed"
	PhaseExpired      Phase = "Expired"
)

// RepositoryRef identifies the source repository of the pull request.
type RepositoryRef struct {
	// Owner is the user or organisation that owns the repository.
	// +kubebuilder:validation:MinLength=1
	Owner string `json:"owner"`
	// Name is the repository name without the owner.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// FullName returns "owner/name".
func (r RepositoryRef) FullName() string { return r.Owner + "/" + r.Name }

// PullRequestRef carries the pull-request facts the controller renders from.
type PullRequestRef struct {
	// Number is the pull request number.
	// +kubebuilder:validation:Minimum=1
	Number int `json:"number"`
	// HeadSHA is the commit the preview should run.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{7,64}$`
	HeadSHA string `json:"headSHA"`
	// HeadRef is the source branch name.
	HeadRef string `json:"headRef,omitempty"`
	// BaseRef is the target branch name.
	BaseRef string `json:"baseRef,omitempty"`
	// Author is the login of the pull request author.
	Author string `json:"author,omitempty"`
	// Title is the pull request title, for display only.
	Title string `json:"title,omitempty"`
	// Labels are the pull request labels at the time of the last event.
	Labels []string `json:"labels,omitempty"`
	// Draft is true when the pull request is a draft.
	Draft bool `json:"draft,omitempty"`
}

// PreviewEnvironmentSpec is the desired state, written by the webhook intake
// (or by hand) and read by the reconciler.
type PreviewEnvironmentSpec struct {
	Repository  RepositoryRef  `json:"repository"`
	PullRequest PullRequestRef `json:"pullRequest"`
	// State is the pull request state.
	State PRState `json:"state"`
	// Template names the template set used to render the environment.
	// +kubebuilder:default=default
	Template string `json:"template,omitempty"`
	// TTL overrides the template set's time-to-live. The environment
	// expires at lastActivityTime + TTL.
	TTL *metav1.Duration `json:"ttl,omitempty"`
	// LastActivityTime is the SCM-side updated_at of the most recent event
	// applied to this spec. It anchors the TTL and orders out-of-order
	// webhook deliveries.
	LastActivityTime metav1.Time `json:"lastActivityTime"`
}

// ReportedStatus records the last status successfully posted to the SCM so
// that reconciles do not re-post identical statuses.
type ReportedStatus struct {
	SHA   string `json:"sha"`
	State string `json:"state"`
}

// PreviewEnvironmentStatus is the observed state written by the reconciler.
type PreviewEnvironmentStatus struct {
	// Phase is the coarse lifecycle phase.
	Phase Phase `json:"phase,omitempty"`
	// Namespace is the Kubernetes namespace that hosts the environment.
	Namespace string `json:"namespace,omitempty"`
	// URL is the public URL of the preview, if the template defines one.
	URL string `json:"url,omitempty"`
	// ExpiresAt is when the TTL elapses.
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// AppliedHash is the content hash of the last successfully applied bundle.
	AppliedHash string `json:"appliedHash,omitempty"`
	// AppliedSHA is the head commit of the last applied bundle.
	AppliedSHA string `json:"appliedSHA,omitempty"`
	// Resources is the number of objects in the last applied bundle.
	Resources int32 `json:"resources,omitempty"`
	// LastReported is the last status posted to the SCM.
	LastReported *ReportedStatus `json:"lastReported,omitempty"`
	// TerminatedAt is when the environment reached Closed or Expired.
	TerminatedAt *metav1.Time `json:"terminatedAt,omitempty"`
	// FailureCount counts consecutive render/apply failures (drives backoff).
	FailureCount int32 `json:"failureCount,omitempty"`
	// Message is a human-readable explanation of the phase.
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the spec generation the status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions follow the Kubernetes condition conventions.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PreviewEnvironment is one ephemeral environment for one pull request.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pe;preview
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.repository.name`
// +kubebuilder:printcolumn:name="PR",type=integer,JSONPath=`.spec.pullRequest.number`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.namespace`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
type PreviewEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PreviewEnvironmentSpec   `json:"spec,omitempty"`
	Status PreviewEnvironmentStatus `json:"status,omitempty"`
}

// PreviewEnvironmentList is a list of PreviewEnvironment.
//
// +kubebuilder:object:root=true
type PreviewEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PreviewEnvironment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PreviewEnvironment{}, &PreviewEnvironmentList{})
}

// Condition types set on PreviewEnvironment.
const (
	// ConditionRendered reports whether the template rendered and validated.
	ConditionRendered = "Rendered"
	// ConditionApplied reports whether the current bundle is applied.
	ConditionApplied = "Applied"
	// ConditionReady reports whether all workloads are available.
	ConditionReady = "Ready"
)

// Well-known labels and annotations written on managed objects.
const (
	LabelManagedBy  = "app.kubernetes.io/managed-by"
	ManagedByValue  = "previewd"
	LabelInstance   = "previewd.dev/instance"
	LabelRepo       = "previewd.dev/repo"
	LabelPR         = "previewd.dev/pr"
	LabelExpiresAt  = "previewd.dev/expires-at"
	AnnotationSHA   = "previewd.dev/head-sha"
	AnnotationHash  = "previewd.dev/bundle-hash"
	AnnotationOwner = "previewd.dev/owner"
	Finalizer       = "previewd.dev/teardown"
)
