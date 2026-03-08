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

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"knative.dev/pkg/apis"

	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	console "github.com/openshift/api/console/v1"
	techzonev1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
)

// DeploymentReconciler reconciles a Deployment object
type DeploymentReconciler struct {
	Loader   ConfigLoader
	Creds    CredentialProvider
	Cloner   RepoCloner
	Resolver RouteResolver
	client.Client
	Scheme *runtime.Scheme
	Config *rest.Config
}

// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelineruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tekton.dev,resources=taskruns,verbs=get;list;watch

const (
	gitBaseOrgURL         = "https://github.ibm.com/itz-content"
	pipelineFileName      = "pipeline.yaml"
	pipelineRunFileName   = "pipelinerun.yaml"
	postDeploymentMessage = "Post-deployment CRD created."
	notificationFinalizer = "techzone.ibm.com/console-notification-cleanup"

	// Standard condition types for the Deployment CR.
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"
)

// ============================================================================
// Reconcile
// ============================================================================

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	cfg, err := r.Loader.Load(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to load operator config: %w", err)
	}

	deployment := &techzonev1alpha1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion: clean up the ConsoleNotification before allowing Kubernetes to delete the CR.
	if !deployment.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(deployment, notificationFinalizer) {
			logger.Info("Deployment is being deleted, cleaning up ConsoleNotification.")
			if err := r.deleteConsoleNotification(ctx, deployment); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(deployment, notificationFinalizer)
			if err := r.Update(ctx, deployment); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure our finalizer is registered.
	if !controllerutil.ContainsFinalizer(deployment, notificationFinalizer) {
		controllerutil.AddFinalizer(deployment, notificationFinalizer)
		return ctrl.Result{}, r.Update(ctx, deployment)
	}

	if err := r.ReconcileConsoleNotification(ctx, deployment); err != nil {
		logger.Error(err, "Failed to reconcile ConsoleNotification")
	}

	// Active deployment phase.
	if deployment.Status.Phase != "Succeeded" && deployment.Status.Phase != "Failed" {
		if _, err := r.reconcileDeploymentPhase(ctx, deployment, cfg); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Post-deployment: trigger a follow-up CR if configured and not yet initiated.
	if deployment.Status.Phase == "Succeeded" &&
		deployment.Spec.PostDeployment.RepoName != "" &&
		deployment.Status.Message != postDeploymentMessage {
		return ctrl.Result{}, r.reconcilePostDeployment(ctx, deployment)
	}

	return ctrl.Result{}, nil
}

// reconcilePostDeployment creates a child Deployment CR for the post-deployment phase.
func (r *DeploymentReconciler) reconcilePostDeployment(ctx context.Context, deployment *techzonev1alpha1.Deployment) error {
	logger := logf.FromContext(ctx)

	postDeployment := &techzonev1alpha1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-post-deploy", deployment.Name),
			Namespace: deployment.Namespace,
		},
		Spec: techzonev1alpha1.DeploymentSpec{
			RepoName:   deployment.Spec.PostDeployment.RepoName,
			Release:    deployment.Spec.PostDeployment.Release,
			Parameters: deployment.Spec.PostDeployment.Parameters,
			Ansible:    deployment.Spec.PostDeployment.Ansible,
		},
	}

	if err := ctrl.SetControllerReference(deployment, postDeployment, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on post-deployment CR: %w", err)
	}
	if err := r.Create(ctx, postDeployment); err != nil {
		return fmt.Errorf("failed to create post-deployment CR: %w", err)
	}

	deployment.Status.Message = postDeploymentMessage
	if err := r.Status().Update(ctx, deployment); err != nil {
		logger.Error(err, "Failed to update status after creating post-deployment CR")
	}
	return nil
}

// ============================================================================
// Deployment phase
// ============================================================================

// reconcileDeploymentPhase runs the core deploy logic: fetches credentials,
// clones the repo, then dispatches to either Ansible or Tekton.
func (r *DeploymentReconciler) reconcileDeploymentPhase(ctx context.Context, deployment *techzonev1alpha1.Deployment, cfg config.OperatorConfig) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	pat, err := r.Creds.GetGitHubPAT(ctx, cfg)
	if err != nil {
		r.applyFailedStatus(ctx, deployment, techzonev1alpha1.FailureCategoryCredential,
			fmt.Sprintf("Failed to retrieve GitHub PAT: %s", err),
			[]techzonev1alpha1.DiagnosticEntry{{
				Component:       "Operator",
				Message:         err.Error(),
				RemediationHint: "Verify SecretsManagerURL and SecretsManagerSecretID in the itz-deployer-config ConfigMap, and that the IBM API key in the operator environment is valid.",
			}},
		)
		return ctrl.Result{}, err
	}

	gitRepoURL := fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), deployment.Spec.RepoName)
	repoDir, cleanup, err := r.Cloner.Clone(ctx, gitRepoURL, deployment.Spec.Release, pat)
	if err != nil {
		logger.Error(err, "Failed to clone repository")
		r.applyFailedStatus(ctx, deployment, techzonev1alpha1.FailureCategoryClone,
			fmt.Sprintf("Failed to clone repository: %s", err),
			[]techzonev1alpha1.DiagnosticEntry{{
				Component:       "Operator",
				Message:         err.Error(),
				RemediationHint: fmt.Sprintf("Verify that the repository %q exists, the tag %q is valid, and the GitHub PAT has read access.", deployment.Spec.RepoName, deployment.Spec.Release),
			}},
		)
		return ctrl.Result{}, err
	}
	defer cleanup()

	if deployment.Spec.Ansible.AnsiblePlaybook != "" {
		job, err := r.reconcileJobResources(ctx, deployment, gitRepoURL)
		if err != nil {
			logger.Error(err, "Failed to reconcile Ansible job resources")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.updateJobStatus(ctx, deployment, job)
	}

	pipelineRun, err := r.reconcileTektonResources(ctx, deployment, repoDir)
	if err != nil {
		logger.Error(err, "Failed to reconcile Tekton resources")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.updatePipelineRunStatus(ctx, deployment, pipelineRun)
}

// applyFailedStatus marks the deployment as Failed with a category, message,
// and structured diagnostics, then persists the status. It replaces the old
// setFailedStatus helper so all failure paths produce rich diagnostics.
func (r *DeploymentReconciler) applyFailedStatus(
	ctx context.Context,
	deployment *techzonev1alpha1.Deployment,
	category techzonev1alpha1.FailureCategory,
	message string,
	diagnostics []techzonev1alpha1.DiagnosticEntry,
) {
	logger := logf.FromContext(ctx)

	deployment.Status.Phase = "Failed"
	deployment.Status.Message = message
	deployment.Status.FailureCategory = category
	deployment.Status.Diagnostics = diagnostics

	apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "Failed",
		Message:            message,
		ObservedGeneration: deployment.Generation,
	})
	apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
		Type:               ConditionProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             "Failed",
		Message:            "Deployment is no longer progressing.",
		ObservedGeneration: deployment.Generation,
	})
	apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
		Type:               ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             "Failed",
		Message:            message,
		ObservedGeneration: deployment.Generation,
	})

	if err := r.Status().Update(ctx, deployment); err != nil {
		logger.Error(err, "Failed to update Deployment status")
	}
}

// ============================================================================
// Ansible / Job
// ============================================================================

func (r *DeploymentReconciler) reconcileJobResources(ctx context.Context, deployment *techzonev1alpha1.Deployment, gitRepoURL string) (*batchv1.Job, error) {
	logger := logf.FromContext(ctx)
	jobName := fmt.Sprintf("%s-ansible-runner", deployment.Name)

	foundJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: deployment.Namespace}, foundJob)
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}

	if errors.IsNotFound(err) {
		logger.Info("Creating new Ansible Job", "Job.Name", jobName)
		job, err := r.newAnsibleJob(deployment, jobName, gitRepoURL)
		if err != nil {
			return nil, fmt.Errorf("failed to build Ansible job spec: %w", err)
		}
		if err := controllerutil.SetControllerReference(deployment, job, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, job); err != nil {
			return nil, err
		}
		deployment.Status.PipelineRunName = job.Name
		if err := r.Status().Update(ctx, deployment); err != nil {
			logger.Error(err, "Failed to update Deployment status with job name")
		}
		return job, nil
	}

	// Job exists — check its terminal conditions.
	for _, condition := range foundJob.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			logger.Info("Ansible Job succeeded", "Job.Name", foundJob.Name)
			return foundJob, nil
		}
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return foundJob, fmt.Errorf("ansible Job failed: %s", condition.Reason)
		}
	}

	logger.Info("Ansible Job still running", "Job.Name", foundJob.Name)
	return nil, nil
}

// newAnsibleJob constructs the Job spec using the two-container pattern (git-cloner + ansible-runner).
func (r *DeploymentReconciler) newAnsibleJob(deployment *techzonev1alpha1.Deployment, jobName, gitRepoURL string) (*batchv1.Job, error) {
	const volumeName = "ansible-repo"
	const mountPath = "/workspace"

	parts := strings.SplitN(gitRepoURL, "//", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed git repo URL: %s", gitRepoURL)
	}
	gitAuthURL := fmt.Sprintf("%s//$(GIT_PAT)@%s", parts[0], parts[1])

	volumeMount := corev1.VolumeMount{Name: volumeName, MountPath: mountPath}

	patEnvVar := corev1.EnvVar{
		Name: "GIT_PAT",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: config.GitHubSecretName},
				Key:                  "password",
			},
		},
	}

	initContainer := corev1.Container{
		Name:    "git-cloner",
		Image:   "alpine/git",
		Command: []string{"sh", "-c"},
		Args: []string{fmt.Sprintf(
			"git clone --single-branch --branch %s %s %s && chmod go-w %s",
			deployment.Spec.Release, gitAuthURL, mountPath, mountPath,
		)},
		ImagePullPolicy: corev1.PullAlways,
		VolumeMounts:    []corev1.VolumeMount{volumeMount},
		Env:             []corev1.EnvVar{patEnvVar},
	}

	playbookArgs := []string{mountPath + "/" + deployment.Spec.Ansible.AnsiblePlaybook}
	if deployment.Spec.Parameters != "" {
		playbookArgs = append(playbookArgs, "-e", "@"+mountPath+"/"+deployment.Spec.Parameters)
	}

	var shellCmd string
	if deployment.Spec.Ansible.Requirements != "" {
		reqPath := mountPath + "/" + deployment.Spec.Ansible.Requirements
		shellCmd = fmt.Sprintf(
			`git config --global url."https://${GIT_PAT}@github.ibm.com".insteadOf "https://github.ibm.com" && ansible-galaxy install -r %s && `,
			reqPath,
		)
	}
	shellCmd += "ansible-playbook " + strings.Join(playbookArgs, " ")

	isPrivileged := true
	uidRoot := int64(0)

	mainContainer := corev1.Container{
		Name:            "ansible-runner",
		Image:           "docker.io/knickkennedy/k8s-tools:v1.0",
		Command:         []string{"/bin/sh"},
		Args:            []string{"-c", shellCmd},
		Env:             []corev1.EnvVar{patEnvVar},
		WorkingDir:      mountPath,
		ImagePullPolicy: corev1.PullAlways,
		VolumeMounts:    []corev1.VolumeMount{volumeMount},
		SecurityContext: &corev1.SecurityContext{
			Privileged: &isPrivileged,
			RunAsUser:  &uidRoot,
		},
	}

	backoffLimit := int32(3)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: deployment.Namespace,
			Labels:    map[string]string{"app": "ansible-runner", "deployment": deployment.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					InitContainers:     []corev1.Container{initContainer},
					Containers:         []corev1.Container{mainContainer},
					Volumes:            []corev1.Volume{{Name: volumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					ServiceAccountName: config.PipelineServiceAccount,
				},
			},
		},
	}, nil
}

// ============================================================================
// Tekton / PipelineRun
// ============================================================================

func (r *DeploymentReconciler) reconcileTektonResources(ctx context.Context, deployment *techzonev1alpha1.Deployment, repoDir string) (*tektonv1.PipelineRun, error) {
	logger := logf.FromContext(ctx)

	// If we already launched a PipelineRun, just track it.
	if deployment.Status.PipelineRunName != "" {
		existing := &tektonv1.PipelineRun{}
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Status.PipelineRunName, Namespace: deployment.Namespace}, existing); err != nil {
			return nil, fmt.Errorf("failed to get existing PipelineRun: %w", err)
		}
		return existing, nil
	}

	pipelineYAML, err := os.ReadFile(filepath.Join(repoDir, pipelineFileName))
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file: %w", err)
	}
	var pipeline tektonv1.Pipeline
	if err := yaml.Unmarshal(pipelineYAML, &pipeline); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Pipeline YAML: %w", err)
	}
	pipeline.SetNamespace(deployment.Namespace)

	pipelineRunYAML, err := os.ReadFile(filepath.Join(repoDir, pipelineRunFileName))
	if err != nil {
		return nil, fmt.Errorf("failed to read PipelineRun file: %w", err)
	}
	var pipelineRun tektonv1.PipelineRun
	if err := yaml.Unmarshal(pipelineRunYAML, &pipelineRun); err != nil {
		return nil, fmt.Errorf("failed to unmarshal PipelineRun YAML: %w", err)
	}
	pipelineRun.SetNamespace(deployment.Namespace)

	if deployment.Spec.Parameters != "" {
		paramsYAML, err := os.ReadFile(filepath.Join(repoDir, deployment.Spec.Parameters))
		if err != nil {
			return nil, fmt.Errorf("failed to read parameters file: %w", err)
		}
		if err := mergeParams(&pipelineRun, paramsYAML); err != nil {
			return nil, fmt.Errorf("failed to merge parameters: %w", err)
		}
	}

	if err := controllerutil.SetControllerReference(deployment, &pipeline, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, &pipeline); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create Pipeline: %w", err)
	}

	if err := controllerutil.SetControllerReference(deployment, &pipelineRun, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, &pipelineRun); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create PipelineRun: %w", err)
	}

	deployment.Status.PipelineRunName = pipelineRun.Name
	if err := r.Status().Update(ctx, deployment); err != nil {
		logger.Error(err, "Failed to update Deployment status with PipelineRun name")
	}

	return &pipelineRun, nil
}

// mergeParams merges override parameters from YAML into the PipelineRun spec.
func mergeParams(pipelineRun *tektonv1.PipelineRun, paramsYAML []byte) error {
	var overrides map[string]string
	if err := yaml.Unmarshal(paramsYAML, &overrides); err != nil {
		return fmt.Errorf("failed to unmarshal parameters YAML: %w", err)
	}

	index := make(map[string]tektonv1.Param, len(pipelineRun.Spec.Params))
	for _, p := range pipelineRun.Spec.Params {
		index[p.Name] = p
	}
	for name, value := range overrides {
		index[name] = tektonv1.Param{
			Name:  name,
			Value: tektonv1.ParamValue{Type: tektonv1.ParamTypeString, StringVal: value},
		}
	}

	updated := make([]tektonv1.Param, 0, len(index))
	for _, p := range index {
		updated = append(updated, p)
	}
	pipelineRun.Spec.Params = updated
	return nil
}

// ============================================================================
// Status updates
// ============================================================================

// updatePipelineRunStatus maps a PipelineRun's condition onto the Deployment status.
func (r *DeploymentReconciler) updatePipelineRunStatus(ctx context.Context, deployment *techzonev1alpha1.Deployment, pr *tektonv1.PipelineRun) error {
	if pr == nil || pr.Status.Conditions == nil {
		return nil
	}
	cond := pr.Status.GetCondition(apis.ConditionSucceeded)
	return r.applyCondition(ctx, deployment, cond, pr, nil)
}

// updateJobStatus maps a Kubernetes Job's conditions onto the Deployment status.
func (r *DeploymentReconciler) updateJobStatus(ctx context.Context, deployment *techzonev1alpha1.Deployment, job *batchv1.Job) error {
	if job == nil || job.Status.Conditions == nil {
		return nil
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return r.applyCondition(ctx, deployment, &apis.Condition{
				Type:    apis.ConditionSucceeded,
				Status:  corev1.ConditionTrue,
				Message: "Kubernetes Job completed successfully.",
			}, nil, nil)
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return r.applyCondition(ctx, deployment, &apis.Condition{
				Type:    apis.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Message: cond.Message,
			}, nil, job)
		}
	}
	if job.Status.Active > 0 {
		return r.applyCondition(ctx, deployment, &apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionUnknown,
			Message: "Kubernetes Job is in progress.",
		}, nil, nil)
	}
	return nil
}

// applyCondition translates a knative Condition into a Deployment phase,
// sets the standard Kubernetes status conditions, collects diagnostics on
// failure, and persists the status.
func (r *DeploymentReconciler) applyCondition(
	ctx context.Context,
	deployment *techzonev1alpha1.Deployment,
	cond *apis.Condition,
	pr *tektonv1.PipelineRun,
	job *batchv1.Job,
) error {
	if cond == nil {
		return nil
	}
	logger := logf.FromContext(ctx)

	switch cond.Status {
	case corev1.ConditionTrue:
		deployment.Status.Phase = "Succeeded"
		deployment.Status.Message = "Resource completed successfully."
		// Clear any diagnostics from a previous failure so the status is clean.
		deployment.Status.Diagnostics = nil
		deployment.Status.FailureCategory = ""
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Succeeded",
			Message:            "Deployment completed successfully.",
			ObservedGeneration: deployment.Generation,
		})
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionProgressing,
			Status:             metav1.ConditionFalse,
			Reason:             "Succeeded",
			Message:            "Deployment is no longer progressing.",
			ObservedGeneration: deployment.Generation,
		})
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             "Succeeded",
			Message:            "Deployment is not degraded.",
			ObservedGeneration: deployment.Generation,
		})

	case corev1.ConditionFalse:
		deployment.Status.Phase = "Failed"
		deployment.Status.Message = cond.Message
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "Failed",
			Message:            cond.Message,
			ObservedGeneration: deployment.Generation,
		})
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionProgressing,
			Status:             metav1.ConditionFalse,
			Reason:             "Failed",
			Message:            "Deployment is no longer progressing.",
			ObservedGeneration: deployment.Generation,
		})
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             "Failed",
			Message:            cond.Message,
			ObservedGeneration: deployment.Generation,
		})

		// Collect structured diagnostics from the failed Tekton or Ansible resource.
		// This is best-effort — a failure here must not prevent the status update.
		collector := &diagnosticsCollector{c: r.Client, restConfig: r.Config}
		switch {
		case pr != nil:
			collector.collectTektonDiagnostics(ctx, deployment, pr)
		case job != nil:
			collector.collectAnsibleDiagnostics(ctx, deployment, job)
		}

	case corev1.ConditionUnknown:
		deployment.Status.Phase = "Running"
		deployment.Status.Message = cond.Message
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "Progressing",
			Message:            "Deployment is in progress.",
			ObservedGeneration: deployment.Generation,
		})
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionProgressing,
			Status:             metav1.ConditionTrue,
			Reason:             "Progressing",
			Message:            cond.Message,
			ObservedGeneration: deployment.Generation,
		})
		apimeta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               ConditionDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             "Progressing",
			Message:            "Deployment is not degraded.",
			ObservedGeneration: deployment.Generation,
		})
	}

	if err := r.Status().Update(ctx, deployment); err != nil {
		logger.Error(err, "Failed to update Deployment status")
		return err
	}
	return nil
}

// ============================================================================
// Console notifications
// ============================================================================

func (r *DeploymentReconciler) ReconcileConsoleNotification(ctx context.Context, deployment *techzonev1alpha1.Deployment) error {
	logger := logf.FromContext(ctx)

	externalURL, err := r.Resolver.GetExternalClusterBaseURL(ctx)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("%s-status-banner", deployment.Name)
	desired := r.buildNotification(name, externalURL, deployment)

	existing := &console.ConsoleNotification{}
	err = r.Get(ctx, client.ObjectKey{Name: name}, existing)
	if errors.IsNotFound(err) {
		logger.Info("Creating ConsoleNotification", "Name", name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("failed to get ConsoleNotification: %w", err)
	}

	if !reflect.DeepEqual(existing.Spec, desired.Spec) {
		logger.Info("Updating ConsoleNotification", "Name", name)
		existing.Spec = desired.Spec
		if err := r.Update(ctx, existing); err != nil {
			if errors.IsConflict(err) {
				return fmt.Errorf("conflict updating ConsoleNotification, requeueing: %w", err)
			}
			return err
		}
	}

	return nil
}

// buildNotification constructs the desired ConsoleNotification for the current deployment phase.
func (r *DeploymentReconciler) buildNotification(name, externalURL string, deployment *techzonev1alpha1.Deployment) *console.ConsoleNotification {
	podLink := &console.Link{
		Href: externalURL + "/k8s/ns/default/core~v1~Pod",
		Text: "See more information in the pods here.",
	}

	var text, color string
	switch deployment.Status.Phase {
	case "Failed":
		text = fmt.Sprintf("CR '%s' failed: %s", deployment.Name, deployment.Status.Message)
		color = "#C00000"
	case "Running":
		text = fmt.Sprintf("CR '%s' is running...", deployment.Name)
		color = "#0043CE"
	case "Succeeded":
		text = fmt.Sprintf("CR '%s' is finished!", deployment.Name)
		color = "#198038"
	default:
		text = fmt.Sprintf("CR '%s' is starting...", deployment.Name)
		color = "#6929C4"
	}

	return &console.ConsoleNotification{
		TypeMeta: metav1.TypeMeta{
			APIVersion: console.GroupVersion.String(),
			Kind:       "ConsoleNotification",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: console.ConsoleNotificationSpec{
			Text:            text,
			Link:            podLink,
			Location:        console.BannerTop,
			BackgroundColor: color,
		},
	}
}

// deleteConsoleNotification deletes the ConsoleNotification associated with a Deployment CR.
func (r *DeploymentReconciler) deleteConsoleNotification(ctx context.Context, deployment *techzonev1alpha1.Deployment) error {
	name := fmt.Sprintf("%s-status-banner", deployment.Name)
	notification := &console.ConsoleNotification{}
	if err := r.Get(ctx, client.ObjectKey{Name: name}, notification); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get ConsoleNotification %s: %w", name, err)
	}
	if err := r.Delete(ctx, notification); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("failed to delete ConsoleNotification %s: %w", name, err)
	}
	return nil
}

// ============================================================================
// Manager setup
// ============================================================================

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&techzonev1alpha1.Deployment{}).
		Named("deployment").
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Complete(r)
}
