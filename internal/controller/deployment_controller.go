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

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	console "github.com/openshift/api/console/v1"
	routev1 "github.com/openshift/api/route/v1"
	techzonev1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	utils "github.ibm.com/itz-content/itz-deployer-operator/pkg"
	ibm "github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
	rbac "github.ibm.com/itz-content/itz-deployer-operator/pkg/rbac"
)

// DeploymentReconciler reconciles a Deployment object
type DeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *rest.Config
}

// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelineruns,verbs=get;list;watch;create;update;patch;delete

const (
	// Your base Git organization URL
	gitBaseOrgURL = "https://github.ibm.com/itz-content"

	pipelineFileName      = "pipeline.yaml"
	pipelineRunFileName   = "pipelinerun.yaml"
	postDeploymentMessage = "Post-deployment CRD created."
)

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient()
	if err != nil {
		logger.Error(err, err.Error())
	}
	deployment := &techzonev1alpha1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Deployment resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Deployment")
		return ctrl.Result{}, err
	}

	r.ReconcileConsoleNotification(ctx, deployment)
	// Step 1: Handle the main deployment phase.
	if deployment.Status.Phase != "Succeeded" && deployment.Status.Phase != "Failed" {
		result, err := r.reconcileDeploymentPhase(ctx, kclient, deployment, deployment.Spec)
		if err != nil {
			return result, err
		}
		// Requeue if the main deployment is not yet complete.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 2: If the main deployment succeeded, check for a post-deployment phase.
	// We'll use a new status field to track if we've already initiated the post-deployment.
	if deployment.Status.Phase == "Succeeded" && deployment.Spec.PostDeployment.RepoName != "" && deployment.Status.Message != postDeploymentMessage {
		logger.Info("Main deployment succeeded. Creating a new CRD for post-deployment.")

		// Create a new Deployment object for the post-deployment.
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

		// Set the owner reference so Kubernetes handles garbage collection.
		if err := ctrl.SetControllerReference(deployment, postDeployment, r.Scheme); err != nil {
			logger.Error(err, "Failed to set owner reference on post-deployment CRD.")
			return ctrl.Result{}, err
		}

		// Create the new Deployment resource in the cluster.
		if err := kclient.Create(ctx, postDeployment); err != nil {
			logger.Error(err, "Failed to create post-deployment CRD.")
			return ctrl.Result{}, err
		}

		// Update the status of the main deployment to indicate the post-deployment has been initiated.
		deployment.Status.Message = postDeploymentMessage
		if err := kclient.Status().Update(ctx, deployment); err != nil {
			logger.Error(err, "Failed to update main deployment status after creating post-deployment CRD.")
			return ctrl.Result{}, err
		}

		// Return and let the new Deployment CRD be handled by the normal reconcile loop.
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileDeploymentPhase encapsulates the core logic for a single deployment.
func (r *DeploymentReconciler) reconcileDeploymentPhase(ctx context.Context, kclient client.Client, deployment *techzonev1alpha1.Deployment, spec techzonev1alpha1.DeploymentSpec) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	gitRepoURL := fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), spec.RepoName)

	// Step 2: Get the IBM API Key and GitHub PAT (this can be a separate helper function)
	apiKey, err := ibm.GetIBMAPIKey(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get IBM API key: %w", err)
	}
	secretsManagerAPI, err := ibm.CreateSecretsManagerClient(apiKey)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create secrets manager client: %w", err)
	}
	pat, err := ibm.GetGitHubPAT(secretsManagerAPI, "4560e15e-8fb8-37d9-35f5-01fe2c4b77a7")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get GitHub PAT: %w", err)
	}

	// Step 3: Clone the Git repository.
	repoDir, err := r.cloneRepo(ctx, gitRepoURL, spec.Release, pat)
	if err != nil {
		logger.Error(err, "Failed to clone repository")
		deployment.Status.Phase = "Failed"
		deployment.Status.Message = fmt.Sprintf("Failed to clone repository: %s", err.Error())
		if err := kclient.Status().Update(ctx, deployment); err != nil {
			logger.Error(err, "Failed to update Deployment status")
		}
		return ctrl.Result{}, err
	}
	defer os.RemoveAll(repoDir)

	switch {
	case deployment.Spec.Ansible.AnsiblePlaybook != "":
		job, err := r.reconcileJobResources(ctx, deployment, gitRepoURL)
		if err != nil {
			logger.Error(err, "Failed to reconcile ansible job resources.")
		}
		if err := r.updateStatus(ctx, deployment, job); err != nil {
			logger.Error(err, "Failed to update Deployment status.")
			return ctrl.Result{}, err
		}
	default:
		pipelineRun, err := r.reconcileTektonResources(ctx, deployment, repoDir)
		if err != nil {
			logger.Error(err, "Failed to reconcile Tekton resources.")
			return ctrl.Result{}, err
		}

		if err := r.updateStatus(ctx, deployment, pipelineRun); err != nil {
			logger.Error(err, "Failed to update Deployment status")
			return ctrl.Result{}, err
		}
	}

	// Step 6: Requeue if the Deployment is not yet complete.
	if deployment.Status.Phase != "Succeeded" && deployment.Status.Phase != "Failed" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// If the pipeline is in a final state (Succeeded or Failed), no need to requeue from here.
	return ctrl.Result{}, nil
}

// Necessary in order for us to access github.ibm.com and the secret manager

// cloneRepo handles cloning the repository and returns the path to the temporary directory.
func (r *DeploymentReconciler) cloneRepo(ctx context.Context, repoName, release string, pat string) (string, error) {
	logger := logf.FromContext(ctx)

	repoDir, err := os.MkdirTemp("", "git-repo")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}

	refName := plumbing.ReferenceName(fmt.Sprintf("refs/tags/%s", release))

	logger.Info("Cloning repository", "URL", repoName, "tag", release)

	auth := &http.BasicAuth{
		Username: "x-oauth-basic",
		Password: pat,
	}

	_, err = git.PlainClone(repoDir, false, &git.CloneOptions{
		URL:           repoName,
		ReferenceName: refName,
		SingleBranch:  true,
		Auth:          auth,
	})

	if err != nil {
		os.RemoveAll(repoDir)
		return "", fmt.Errorf("failed to clone repository %s with tag %s: %w", repoName, release, err)
	}

	return repoDir, nil
}

// reconcileJobResources handles the Ansible Job creation and status check.
// It returns the Job object and an error. If reconciliation needs to requeue,
// the error will be nil and a Requeue result will be set via the context.
func (r *DeploymentReconciler) reconcileJobResources(ctx context.Context, deployment *techzonev1alpha1.Deployment, gitRepoURL string) (*batchv1.Job, error) {
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient()

	if err != nil {
		logger.Error(err, "Could not access kclient.")
	}

	jobName := fmt.Sprintf("%s-ansible-runner", deployment.Name)
	foundJob := &batchv1.Job{}

	err = kclient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: deployment.Namespace}, foundJob)
	if err != nil && errors.IsNotFound(err) {
		// Job not found, create it now.
		logger.Info("Creating new Ansible Job", "Job.Name", jobName)
		job := r.newAnsibleJob(deployment, jobName, gitRepoURL)

		if err := controllerutil.SetControllerReference(deployment, job, r.Scheme); err != nil {
			return nil, err
		}

		if err := kclient.Create(ctx, job); err != nil {
			return nil, err
		}

		logger.Info("Ansible job created successfully. Requeuing to monitor status.")
		deployment.Status.PipelineRunName = job.Name

		if err := kclient.Status().Update(ctx, deployment); err != nil {
			logger.Error(err, "Failed to update Deployment status with run name.")
		}

		return job, nil
	} else if err != nil {
		return nil, err
	}

	logger.Info("Found existing Ansible job. Checking status.", "Job.Name", foundJob.Name)
	for _, condition := range foundJob.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			logger.Info("Ansible Job succeeded.", "Job.Name", foundJob.Name)
			// Job succeeded. Return the job so the main Reconcile function can proceed.
			return foundJob, nil
		}
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			errMsg := fmt.Sprintf("Ansible Job failed. Reason: %s", condition.Reason)
			logger.Error(nil, errMsg, "Job.Name", foundJob.Name)
			// Job failed. Stop reconciliation for this CR.
			return foundJob, fmt.Errorf("%s", errMsg)
		}
	}

	// Job is still running (or pending). Requeue to check status later.
	logger.Info("Ansible Job still running or pending. Requeueing.", "Job.Name", foundJob.Name)
	return nil, nil
}

// newAnsibleJob constructs the Kubernetes Job object based on the Deployment CRD spec.
// This implements the two-container pattern (Git Cloner + Ansible Runner).
func (r *DeploymentReconciler) newAnsibleJob(deployment *techzonev1alpha1.Deployment, jobName string, gitRepoURL string) *batchv1.Job {
	// Define constants for volume and mount path
	const volumeName = "ansible-repo"
	const mountPath = "/workspace"

	// Define the shared EmptyDir volume
	volume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
	}

	patEnvVar := corev1.EnvVar{
		Name: "GIT_PAT",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: rbac.SecretName,
				},
				Key: "password",
			},
		},
	}

	parts := strings.SplitN(gitRepoURL, "//", 2)
	if len(parts) != 2 {
		// Return error/nil for malformed URL
		return nil
	}
	gitAuthURL := fmt.Sprintf("%s//$(%s)@%s", parts[0], "GIT_PAT", parts[1])

	// Init Container (Git Clone)
	initContainer := corev1.Container{
		Name:    "git-cloner",
		Image:   "alpine/git",
		Command: []string{"git"},
		Args: []string{
			"clone",
			"--single-branch",
			"--branch",
			deployment.Spec.Release,
			gitAuthURL,
			mountPath,
		},
		VolumeMounts: []corev1.VolumeMount{volumeMount},
		Env:          []corev1.EnvVar{patEnvVar},
	}

	playbookPath := mountPath + "/" + deployment.Spec.Ansible.AnsiblePlaybook

	// mandatory args
	args := []string{
		playbookPath,
	}

	if deployment.Spec.Parameters != "" {
		varsFilePath := mountPath + "/" + deployment.Spec.Parameters
		args = append(args, "-e", "@"+varsFilePath)
	}

	// Main Container (Ansible Runner)
	mainContainer := corev1.Container{
		Name:         "ansible-runner",
		Image:        "docker.io/knickkennedy/k8s-tools@sha256:542002707d909d25b3ed05654f77c514a507b1fc916ad3d44adb5a672adb4299",
		Command:      []string{"ansible-playbook"},
		Args:         args,
		WorkingDir:   mountPath,
		VolumeMounts: []corev1.VolumeMount{volumeMount},
	}

	// Construct the final Job object
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: deployment.Namespace,
			Labels:    map[string]string{"app": "ansible-runner", "deployment": deployment.Name},
		},
		Spec: batchv1.JobSpec{
			// Setting BackoffLimit=0 ensures no retries on failure (crucial for idempotency)
			BackoffLimit: new(int32),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:  corev1.RestartPolicyNever,
					InitContainers: []corev1.Container{initContainer},
					Containers:     []corev1.Container{mainContainer},
					Volumes:        []corev1.Volume{volume},
				},
			},
		},
	}
}

// reconcileTektonResources clones the Git repo and creates/updates the Tekton Pipeline and PipelineRun.
func (r *DeploymentReconciler) reconcileTektonResources(ctx context.Context, deployment *techzonev1alpha1.Deployment, repoDir string) (*tektonv1.PipelineRun, error) {
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient()
	if err != nil {
		logger.Error(err, err.Error())
	}

	// Read and unmarshal pipeline.yaml
	pipelineYAML, err := os.ReadFile(filepath.Join(repoDir, pipelineFileName))
	if err != nil {
		return nil, fmt.Errorf("could not read pipeline file: %w", err)
	}
	var pipeline tektonv1.Pipeline
	if err := yaml.Unmarshal(pipelineYAML, &pipeline); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Pipeline YAML: %w", err)
	}
	pipeline.SetNamespace(deployment.Namespace)

	// Read and unmarshal pipelinerun.yaml
	pipelineRunYAML, err := os.ReadFile(filepath.Join(repoDir, pipelineRunFileName))
	if err != nil {
		return nil, fmt.Errorf("failed to read PipelineRun file: %w", err)
	}
	var pipelineRun tektonv1.PipelineRun
	if err := yaml.Unmarshal(pipelineRunYAML, &pipelineRun); err != nil {
		return nil, fmt.Errorf("failed to unmarshal PipelineRun YAML: %w", err)
	}
	pipelineRun.SetNamespace(deployment.Namespace)

	// If it does, we track it instead of creating a new one.
	if deployment.Status.PipelineRunName != "" {
		existingPipelineRun := &tektonv1.PipelineRun{}
		err := kclient.Get(ctx, types.NamespacedName{Name: deployment.Status.PipelineRunName, Namespace: deployment.Namespace}, existingPipelineRun)
		if err != nil {
			// If we can't get the existing PipelineRun, it might have been deleted.
			// Re-creating it is a valid recovery step, but let's just return an error for now.
			return nil, fmt.Errorf("failed to get existing PipelineRun: %w", err)
		}
		// Return the existing PipelineRun to be tracked by the updateStatus function.
		return existingPipelineRun, nil
	}

	// Step 4: Conditionally inject parameters based on WorkloadType.
	if deployment.Spec.Parameters != "" {
		paramsPath := filepath.Join(repoDir, deployment.Spec.Parameters)
		paramsYAML, err := os.ReadFile(paramsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read tekton parameters file: %w", err)
		}
		if err := r.mergeParams(&pipelineRun, paramsYAML); err != nil {
			return nil, fmt.Errorf("failed to merge parameters: %w", err)
		}
	}

	// Step 5: Create or update the Pipeline.
	if err := controllerutil.SetControllerReference(deployment, &pipeline, r.Scheme); err != nil {
		return nil, err
	}
	if err := kclient.Create(ctx, &pipeline); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create Pipeline: %w", err)
	} else if errors.IsAlreadyExists(err) {
		logger.Info("Pipeline already exists, skipping creation.")
	}

	// Step 6: Create or update the PipelineRun.
	if err := controllerutil.SetControllerReference(deployment, &pipelineRun, r.Scheme); err != nil {
		return nil, err
	}

	if err := kclient.Create(ctx, &pipelineRun); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("failed to create PipelineRun: %w", err)
	} else if errors.IsAlreadyExists(err) {
		logger.Info("PipelineRun already exists, skipping creation.")
	}

	deployment.Status.PipelineRunName = pipelineRun.Name

	if err := kclient.Status().Update(ctx, deployment); err != nil {
		logger.Error(err, "Failed to update Deployment status with PipelineRun name.")
		// Don't return here, we still want to return the pipelineRun object for the main reconcile loop
	}

	return &pipelineRun, nil
}

// mergeParams reads parameters from a YAML byte slice and merges them into the PipelineRun's Spec.
func (r *DeploymentReconciler) mergeParams(pipelineRun *tektonv1.PipelineRun, paramsYAML []byte) error {
	var overrideParams map[string]string
	if err := yaml.Unmarshal(paramsYAML, &overrideParams); err != nil {
		return fmt.Errorf("failed to unmarshal parameters YAML: %w", err)
	}

	existingParams := make(map[string]tektonv1.Param)
	for _, param := range pipelineRun.Spec.Params {
		existingParams[param.Name] = param
	}

	for name, value := range overrideParams {
		existingParams[name] = tektonv1.Param{
			Name: name,
			Value: tektonv1.ParamValue{
				Type:      tektonv1.ParamTypeString,
				StringVal: value,
			},
		}
	}

	var updatedParams []tektonv1.Param
	for _, param := range existingParams {
		updatedParams = append(updatedParams, param)
	}
	pipelineRun.Spec.Params = updatedParams

	return nil
}

// Define the interface for status extraction
// This is not strictly necessary but can make the intent clearer
type ConditionGetter interface {
	GetCondition(t apis.ConditionType) *apis.Condition
	GetConditions() apis.Conditions
}

// updateStatus checks the status of a Tekton PipelineRun OR a Kubernetes Job
// and updates the Deployment CRD accordingly.
func (r *DeploymentReconciler) updateStatus(ctx context.Context, deployment *techzonev1alpha1.Deployment, statusObject any) error {
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient() // kclient is assumed to be an existing kubernetes client

	if err != nil {
		logger.Error(err, err.Error())
		return err // Return the error here
	}

	var succeededCondition *apis.Condition
	var isComplete bool // Flag to check if the status update is complete

	if statusObject == nil {
		logger.Info("Status object is currently nil")
		return nil
	}

	// 1. Use a type switch to handle different status objects
	switch obj := statusObject.(type) {
	case *tektonv1.PipelineRun:
		// Tekton PipelineRun status logic
		if obj != nil {
			if obj.Status.Conditions != nil {
				succeededCondition = obj.Status.GetCondition(apis.ConditionSucceeded)
				isComplete = true // We have successfully found the Tekton condition
			}
		}
	case *batchv1.Job:
		// Kubernetes Job status logic
		if obj != nil {
			if obj.Status.Conditions != nil {
				for _, cond := range obj.Status.Conditions {
					// A Job is complete if it has a "Complete" condition set to true
					// or a "Failed" condition set to true. For simplicity, we check
					// the completion conditions and map them to our Succeeded/Failed/Running phases.
					if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
						// Map Job success to a Tekton-like succeeded condition
						succeededCondition = &apis.Condition{
							Type:    apis.ConditionSucceeded,
							Status:  corev1.ConditionTrue,
							Message: "Kubernetes Job completed successfully.",
						}
						isComplete = true
						break
					} else if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
						// Map Job failure to a Tekton-like succeeded condition (but False)
						succeededCondition = &apis.Condition{
							Type:    apis.ConditionSucceeded,
							Status:  corev1.ConditionFalse,
							Message: cond.Message, // Use the Job's failure message
						}
						isComplete = true
						break
					}
				}
			}

			// If the Job isn't explicitly Complete or Failed, it's considered Running.
			if !isComplete && obj.Status.Active > 0 {
				// Set a "Running" status similar to Tekton's ConditionUnknown
				succeededCondition = &apis.Condition{
					Type:    apis.ConditionSucceeded,
					Status:  corev1.ConditionUnknown,
					Message: "Kubernetes Job is in progress.",
				}
				isComplete = true
			}
		}
	default:
		// Handle unsupported type
		logger.Info("Unsupported status object type provided", "type", fmt.Sprintf("%T", statusObject))
		return nil
	}

	// 2. Common status update logic
	if succeededCondition != nil {
		switch succeededCondition.Status {
		case corev1.ConditionTrue:
			deployment.Status.Phase = "Succeeded"
			deployment.Status.Message = "Resource completed successfully."
		case corev1.ConditionFalse:
			deployment.Status.Phase = "Failed"
			deployment.Status.Message = succeededCondition.Message
		case corev1.ConditionUnknown:
			deployment.Status.Phase = "Running"
			deployment.Status.Message = succeededCondition.Message // Use the derived running message
		default:
			// Optionally handle other statuses if necessary
		}

		// 3. Update the Deployment CRD
		if err := kclient.Status().Update(ctx, deployment); err != nil {
			logger.Error(err, "Failed to update Deployment status")
			return err
		}
	}

	// If it's a Job that hasn't started yet (no Active/Complete/Failed count > 0) or
	// a PipelineRun without conditions, the loop will continue reconciling until it gets a status.
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&techzonev1alpha1.Deployment{}).
		Named("deployment").
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		Complete(r)
}

func (r *DeploymentReconciler) ReconcileConsoleNotification(ctx context.Context, deployment *techzonev1alpha1.Deployment) error {
	// 1. Define the desired notification object based on the current phase
	var desiredNotification *console.ConsoleNotification
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient()

	if err != nil {
		return err
	}

	logger.Info("Checking console notification banner.")
	// Use a unique, predictable name for the notification
	name := fmt.Sprintf("%s-status-banner", deployment.Name)
	externalURL, err := r.GetExternalClusterBaseURL(ctx)

	if err != nil {
		return err
	}

	switch deployment.Status.Phase {
	case "Failed":
		// Create a persistent, non-dismissible red alert banner
		desiredNotification = &console.ConsoleNotification{
			TypeMeta: metav1.TypeMeta{
				APIVersion: console.GroupVersion.String(),
				Kind:       "ConsoleNotification",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: console.ConsoleNotificationSpec{
				Text: fmt.Sprintf("CR '%s' failed: %s", deployment.Name, deployment.Status.Message),
				Link: &console.Link{
					Href: externalURL + "/k8s/ns/default/core~v1~Pod",
					Text: "See more information in the pods here.",
				},
				Location:        console.BannerTop,
				BackgroundColor: "#C00000", // Dark Red for critical failure
			},
		}
	case "Running":
		// Create a temporary, dismissible yellow banner for long operations
		desiredNotification = &console.ConsoleNotification{
			TypeMeta: metav1.TypeMeta{
				APIVersion: console.GroupVersion.String(),
				Kind:       "ConsoleNotification",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: console.ConsoleNotificationSpec{
				Text: fmt.Sprintf("CR '%s' is running (Phase: %s)...", deployment.Name, deployment.Status.Phase),
				Link: &console.Link{
					Href: externalURL + "/k8s/ns/default/core~v1~Pod",
					Text: "See more information in the pods here.",
				},
				Location:        console.BannerTop,
				BackgroundColor: "#006699",
			},
		}
	case "Succeeded":
		// Fall through to delete the notification if it exists
		desiredNotification = &console.ConsoleNotification{
			TypeMeta: metav1.TypeMeta{
				APIVersion: console.GroupVersion.String(),
				Kind:       "ConsoleNotification",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: console.ConsoleNotificationSpec{
				Text: fmt.Sprintf("CR '%s' is finished! (Phase: %s)...", deployment.Name, deployment.Status.Phase),
				Link: &console.Link{
					Href: externalURL + "/k8s/ns/default/core~v1~Pod",
					Text: "See more information in the pods here.",
				},
				Location:        console.BannerTop,
				BackgroundColor: "#4CAF50",
			},
		}
	default:
		desiredNotification = &console.ConsoleNotification{
			TypeMeta: metav1.TypeMeta{
				APIVersion: console.GroupVersion.String(),
				Kind:       "ConsoleNotification",
			},
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: console.ConsoleNotificationSpec{
				Text: fmt.Sprintf("CR '%s' is starting...", deployment.Name),
				Link: &console.Link{
					Href: externalURL + "/k8s/ns/default/core~v1~Pod",
					Text: "See more information in the pods here.",
				},
				Location:        console.BannerTop,
				BackgroundColor: "#FFD700",
			},
		}
	}

	existingNotification := &console.ConsoleNotification{}
	key := client.ObjectKey{Name: name}

	err = kclient.Get(ctx, key, existingNotification)

	if err != nil && errors.IsNotFound(err) {
		// The desiredNotification already has the correct Spec set from the switch case
		logger.Info("Creating ConsoleNotification banner", "Name", name)
		if createErr := kclient.Create(ctx, desiredNotification); createErr != nil {
			logger.Error(createErr, "Failed to create ConsoleNotification")
			return createErr
		}

	} else if err == nil {
		// B.2. Resource exists: UPDATE it.

		if !reflect.DeepEqual(existingNotification.Spec, desiredNotification.Spec) {

			existingNotification.Spec = desiredNotification.Spec

			logger.Info("Updating ConsoleNotification banner", "Name", name)
			if updateErr := kclient.Update(ctx, existingNotification); updateErr != nil {
				if errors.IsConflict(updateErr) {
					return fmt.Errorf("conflict during ConsoleNotification update, requeueing: %w", updateErr)
				}
				logger.Error(updateErr, "Failed to update ConsoleNotification")
				return updateErr
			}
		} else {
			logger.V(1).Info("ConsoleNotification already in desired state, skipping update.")
		}

	} else {
		// B.3. Failed to Get for an unknown reason.
		logger.Error(err, "Failed to get ConsoleNotification during update check")
		return err
	}

	return nil
}

// GetExternalClusterBaseURL fetches the public host address of the OpenShift Console.
func (r *DeploymentReconciler) GetExternalClusterBaseURL(ctx context.Context) (string, error) {
	kclient, err := utils.CreateClient()

	if err != nil {
		return "", err
	}

	// 1. Define the target Route key (namespace and name)
	routeKey := types.NamespacedName{
		Namespace: "openshift-console",
		Name:      "console",
	}

	consoleRoute := &routev1.Route{}

	// 2. Fetch the Route object
	err = kclient.Get(ctx, routeKey, consoleRoute)
	if err != nil {
		// If the Route isn't found, log a warning (it may not exist in non-OCP clusters)
		return "", fmt.Errorf("failed to get OpenShift console Route: %w", err)
	}

	// 3. Extract the public host
	host := consoleRoute.Spec.Host
	if host == "" {
		return "", fmt.Errorf("console route spec.host is empty")
	}

	// Routes are always HTTPS by default in OpenShift, but prepend 'https://' for certainty.
	return fmt.Sprintf("https://%s", host), nil
}
