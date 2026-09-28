package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceGuardSpec says what to probe, which Deployment to heal, and how
// aggressively the operator is allowed to act.
type ServiceGuardSpec struct {
	// Deployment (same namespace) that gets rollout-restarted when the endpoint is unhealthy.
	Deployment string `json:"deployment"`

	// URL probed with GET. 2xx/3xx within the timeout counts as healthy.
	// +kubebuilder:validation:Pattern=`^https?://`
	Endpoint string `json:"endpoint"`

	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// A probe slower than this counts as a failure.
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// Consecutive failed probes before any action. This is the flap filter.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// For https endpoints: warn (event + condition + metric) when the served
	// certificate expires sooner than this. 0 disables. Never remediates.
	// +optional
	MinCertValidityHours int32 `json:"minCertValidityHours,omitempty"`

	Remediation RemediationSpec `json:"remediation,omitempty"`
}

// RemediationSpec bounds what the operator may do. Every action is rate limited.
type RemediationSpec struct {
	// Minimum time between two actions on this guard, so a rollout has time to finish.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	CooldownSeconds int32 `json:"cooldownSeconds,omitempty"`

	// At most this many restarts inside WindowSeconds. After that the
	// operator fails over (if configured) or stops and reports.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	MaxRestarts int32 `json:"maxRestarts,omitempty"`

	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// Optional last resort: repoint a Service at a standby Deployment.
	// +optional
	Failover *FailoverSpec `json:"failover,omitempty"`
}

// FailoverSpec replaces a Service's selector. The operator never fails back
// on its own; a human does that after fixing the primary.
type FailoverSpec struct {
	Service  string            `json:"service"`
	Selector map[string]string `json:"selector"`
}

type ServiceGuardStatus struct {
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +optional
	LastProbeLatencyMs int64 `json:"lastProbeLatencyMs,omitempty"`
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// +optional
	LastRemediationTime *metav1.Time `json:"lastRemediationTime,omitempty"`
	// Restart times still inside the window. Kept in status (not memory) so a
	// new leader after failover inherits the budget instead of resetting it.
	// +optional
	RecentRestarts []metav1.Time `json:"recentRestarts,omitempty"`
	// +optional
	TotalRemediations int32 `json:"totalRemediations,omitempty"`
	// +optional
	FailedOver bool `json:"failedOver,omitempty"`
	// +optional
	CertNotAfter *metav1.Time `json:"certNotAfter,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Healthy, Remediating/Degraded, CertExpiringSoon.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sg
// +kubebuilder:printcolumn:name="Healthy",type=string,JSONPath=`.status.conditions[?(@.type=="Healthy")].status`
// +kubebuilder:printcolumn:name="Failures",type=integer,JSONPath=`.status.consecutiveFailures`
// +kubebuilder:printcolumn:name="Remediations",type=integer,JSONPath=`.status.totalRemediations`
// +kubebuilder:printcolumn:name="FailedOver",type=boolean,JSONPath=`.status.failedOver`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type ServiceGuard struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceGuardSpec   `json:"spec,omitempty"`
	Status ServiceGuardStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type ServiceGuardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceGuard `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceGuard{}, &ServiceGuardList{})
}
