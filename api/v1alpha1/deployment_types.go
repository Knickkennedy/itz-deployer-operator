/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=deployer,singular=deployer
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Repo",type="string",JSONPath=".spec.repoName"
// +kubebuilder:printcolumn:name="Release",type="string",JSONPath=".spec.release"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// Deployment is the Schema for the deployments API.
type Deployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DeploymentSpec   `json:"spec,omitempty"`
	Status DeploymentStatus `json:"status,omitempty"`
}

// DeploymentSpec defines the desired state of Deployment.
type DeploymentSpec struct {
	RepoName       string         `json:"repoName"`
	Release        string         `json:"release"`
	Parameters     string         `json:"parameters,omitempty"`
	Ansible        Ansible        `json:"ansible,omitempty"`
	PostDeployment PostDeployment `json:"postDeployment,omitempty"`
}

// FailureCategory is a machine-readable classification of why a deployment
// failed. Intended for consumption by AI agents and automation.
// +kubebuilder:validation:Enum=CredentialError;CloneError;StepFailed;JobFailed;ConfigError;Unknown
type FailureCategory string

const (
	// FailureCategoryCredential indicates an authentication or authorisation
	// failure — e.g. IBM Secrets Manager unreachable, bad GitHub PAT.
	FailureCategoryCredential FailureCategory = "CredentialError"

	// FailureCategoryClone indicates the git clone failed — bad tag, repo not
	// found, or network error reaching GitHub Enterprise.
	FailureCategoryClone FailureCategory = "CloneError"

	// FailureCategoryStepFailed indicates a Tekton TaskRun step exited non-zero.
	FailureCategoryStepFailed FailureCategory = "StepFailed"

	// FailureCategoryJobFailed indicates a Kubernetes Job (Ansible runner) failed.
	FailureCategoryJobFailed FailureCategory = "JobFailed"

	// FailureCategoryConfig indicates a misconfiguration detectable at runtime
	// — e.g. missing pipeline.yaml in the repo, bad parameters file.
	FailureCategoryConfig FailureCategory = "ConfigError"

	// FailureCategoryUnknown is used when the failure cannot be categorised.
	FailureCategoryUnknown FailureCategory = "Unknown"
)

// DiagnosticEntry captures structured failure information from a single
// component (a Tekton step, an Ansible job container, etc.).
// Designed to give an AI agent enough context to diagnose and remediate
// without needing to query additional Kubernetes resources.
type DiagnosticEntry struct {
	// Component identifies which part of the system produced this entry.
	// e.g. "Tekton", "AnsibleJob", "Operator"
	Component string `json:"component"`

	// TaskName is the Tekton Task name that failed, if applicable.
	// +optional
	TaskName string `json:"taskName,omitempty"`

	// StepName is the Tekton step name that failed, if applicable.
	// +optional
	StepName string `json:"stepName,omitempty"`

	// ExitCode is the process exit code from the failed container, if available.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// Reason is the short machine-readable reason string from the container
	// status or condition, e.g. "Error", "OOMKilled", "DeadlineExceeded".
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is the human-readable failure message from the Kubernetes object.
	// +optional
	Message string `json:"message,omitempty"`

	// LogTail contains the last N lines of the failed container's log.
	// Populated at failure time so an AI agent does not need to query pod logs
	// separately — the pod may be gone by the time the agent acts.
	// +optional
	LogTail string `json:"logTail,omitempty"`

	// RemediationHint is a structured suggestion for fixing this failure.
	// Written by the operator based on the failure category and component.
	// +optional
	RemediationHint string `json:"remediationHint,omitempty"`
}

// DeploymentStatus defines the observed state of Deployment.
type DeploymentStatus struct {
	Conditions      []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	Phase           string             `json:"phase,omitempty"`
	Message         string             `json:"message,omitempty"`
	PipelineRunName string             `json:"pipelineRunName,omitempty"`

	// FailureCategory is a machine-readable classification of the failure.
	// Only populated when Phase is "Failed".
	// +optional
	FailureCategory FailureCategory `json:"failureCategory,omitempty"`

	// Diagnostics contains structured failure information collected at the time
	// of failure. Each entry corresponds to a failed component (Tekton step,
	// Ansible container, etc.). Populated only when Phase is "Failed".
	// +optional
	Diagnostics []DiagnosticEntry `json:"diagnostics,omitempty"`
}

type Ansible struct {
	AnsiblePlaybook string `json:"ansiblePlaybook,omitempty"`
	Requirements    string `json:"requirements,omitempty"`
}

type PostDeployment struct {
	RepoName   string  `json:"repoName"`
	Release    string  `json:"release"`
	Parameters string  `json:"parameters,omitempty"`
	Ansible    Ansible `json:"ansible,omitempty"`
}

// +kubebuilder:object:root=true

// DeploymentList contains a list of Deployment.
type DeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Deployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Deployment{}, &DeploymentList{})
}
