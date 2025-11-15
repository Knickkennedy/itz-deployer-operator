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
	"strings"
	"time"

	"knative.dev/pkg/apis"

	"k8s.io/apimachinery/pkg/util/yaml"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

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

	techzonev1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	utils "github.ibm.com/itz-content/itz-deployer-operator/pkg"
	ibm "github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
)

// DeploymentReconciler reconciles a Deployment object
type DeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=techzone.techzone.ibm.com,resources=deployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tekton.dev,resources=pipelineruns,verbs=get;list;watch;create;update;patch;delete

const (
	// Your base Git organization URL
	gitBaseOrgURL = "https://github.ibm.com/itz-content"

	// Template repository names
	ansibleTemplateRepoName = "deployer-ansible-runner"

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

	// Step 1: Determine which Git repository to clone based on the workload type
	var gitRepoURL string
	switch {
	case spec.Ansible.AnsiblePlaybook != "":
		gitRepoURL = fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), ansibleTemplateRepoName)
	default:
		gitRepoURL = fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), spec.RepoName)
	}
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

	// Step 4: Reconcile the Tekton resources (Pipeline and PipelineRun).
	pipelineRun, err := r.reconcileTektonResources(ctx, deployment, repoDir)
	if err != nil {
		logger.Error(err, "Failed to reconcile Tekton resources")
		return ctrl.Result{}, err
	}

	// Step 5: Update the Deployment's status based on the PipelineRun's state.
	if err := r.updateStatus(ctx, deployment, pipelineRun); err != nil {
		logger.Error(err, "Failed to update Deployment status")
		return ctrl.Result{}, err
	}

	// Step 6: Requeue if the PipelineRun is not yet complete.
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

// reconcileTektonResources clones the Git repo and creates/updates the Tekton Pipeline and PipelineRun.
func (r *DeploymentReconciler) reconcileTektonResources(ctx context.Context, deployment *techzonev1alpha1.Deployment, repoDir string) (*tektonv1.PipelineRun, error) {
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient()
	if err != nil {
		logger.Error(err, err.Error())
	}

	// ... (Code for reading pipeline.yaml and pipelinerun.yaml remains the same)
	// (You will need to ensure pipeline.yaml and pipelinerun.yaml exist in both template repos)

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
	switch {
	case deployment.Spec.Ansible.AnsiblePlaybook != "":
		// For Ansible, we construct the parameters from the DeploymentSpec directly.
		pipelineRun.Spec.Params = []tektonv1.Param{
			{
				Name: "repo-url",
				Value: tektonv1.ParamValue{
					Type:      tektonv1.ParamTypeString,
					StringVal: fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), deployment.Spec.RepoName),
				},
			},
			{
				Name: "playbook",
				Value: tektonv1.ParamValue{
					Type:      tektonv1.ParamTypeString,
					StringVal: deployment.Spec.Ansible.AnsiblePlaybook, // Assumes Entrypoint is a new field on your spec
				},
			},
			{
				Name: "parameters",
				Value: tektonv1.ParamValue{
					Type:      tektonv1.ParamTypeString,
					StringVal: deployment.Spec.Parameters,
				},
			},
			{
				Name: "requirements",
				Value: tektonv1.ParamValue{
					Type:      tektonv1.ParamTypeString,
					StringVal: deployment.Spec.Ansible.Requirements,
				},
			},
		}
	default:
		// For Tekton, we use the old logic of merging an external parameters file.
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

// updateStatus checks the PipelineRun's status and updates the Deployment CRD accordingly.
func (r *DeploymentReconciler) updateStatus(ctx context.Context, deployment *techzonev1alpha1.Deployment, pipelineRun *tektonv1.PipelineRun) error {
	logger := logf.FromContext(ctx)
	kclient, err := utils.CreateClient()

	if err != nil {
		logger.Error(err, err.Error())
	}

	if pipelineRun.Status.Conditions != nil {
		succeededCondition := pipelineRun.Status.GetCondition(apis.ConditionSucceeded)
		if succeededCondition != nil {
			switch succeededCondition.Status {
			case corev1.ConditionTrue:
				deployment.Status.Phase = "Succeeded"
				deployment.Status.Message = "PipelineRun completed successfully."
			case corev1.ConditionFalse:
				deployment.Status.Phase = "Failed"
				deployment.Status.Message = succeededCondition.Message
			case corev1.ConditionUnknown:
				deployment.Status.Phase = "Running"
				deployment.Status.Message = "PipelineRun is in progress."
			}

			if err := kclient.Status().Update(ctx, deployment); err != nil {
				logger.Error(err, "Failed to update Deployment status")
				return err
			}
		}
	}
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
