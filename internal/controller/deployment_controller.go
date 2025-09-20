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

	"github.com/IBM/go-sdk-core/v5/core"
	sm "github.com/IBM/secrets-manager-go-sdk/v2/secretsmanagerv2"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	techzonev1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	utils "github.ibm.com/itz-content/itz-deployer-operator/pkg"
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

	pipelineFileName    = "pipeline.yaml"
	pipelineRunFileName = "pipelinerun.yaml"
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

	// Determine which Git repository to clone based on the workload type
	var gitRepoURL string
	switch {
	case deployment.Spec.Ansible.AnsiblePlaybook != "":
		// This case is for the special Ansible template
		gitRepoURL = fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), ansibleTemplateRepoName)
	default:
		// The default case handles all other scenarios, which we've defined as standard Tekton pipelines.
		gitRepoURL = fmt.Sprintf("%s/%s.git", strings.TrimSuffix(gitBaseOrgURL, "/"), deployment.Spec.RepoName)
	}

	// Step 1: Get the IBM API Key from the Kubernetes Secret
	apiKey, err := getIBMAPIKey(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get IBM API key: %w", err)
	}

	// Step 2: Create a Secrets Manager client
	secretsManagerAPI, err := createSecretsManagerClient(apiKey)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create secrets manager client: %w", err)
	}

	// Step 3: Get the GitHub PAT from Secrets Manager
	// Replace "deployer-github-pat" with your actual secret ID from Secrets Manager
	pat, err := getGitHubPAT(secretsManagerAPI, "4560e15e-8fb8-37d9-35f5-01fe2c4b77a7")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get GitHub PAT: %w", err)
	}

	// Now proceed with the rest of your reconciliation logic, using the dynamically set URL.
	// Step 1: Clone the Git repository.
	repoDir, err := r.cloneRepo(ctx, gitRepoURL, deployment.Spec.Release, pat)
	if err != nil {
		logger.Error(err, "Failed to clone repository")
		// Update status before returning
		deployment.Status.Phase = "Failed"
		deployment.Status.Message = fmt.Sprintf("Failed to clone repository: %s", err.Error())
		if err := kclient.Status().Update(ctx, deployment); err != nil {
			logger.Error(err, "Failed to update Deployment status")
		}
		return ctrl.Result{}, err
	}
	defer os.RemoveAll(repoDir)

	// Step 2: Reconcile the Tekton resources (Pipeline and PipelineRun).
	pipelineRun, err := r.reconcileTektonResources(ctx, deployment, repoDir)
	if err != nil {
		logger.Error(err, "Failed to reconcile Tekton resources")
		return ctrl.Result{}, err
	}

	// Step 3: Update the Deployment's status based on the PipelineRun's state.
	if err := r.updateStatus(ctx, deployment, pipelineRun); err != nil {
		logger.Error(err, "Failed to update Deployment status")
		return ctrl.Result{}, err
	}

	// Step 4: Requeue if the PipelineRun is not yet complete.
	if deployment.Status.Phase != "Succeeded" && deployment.Status.Phase != "Failed" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// Necessary in order for us to access github.ibm.com and the secret manager
func getIBMAPIKey(ctx context.Context) (string, error) {
	// Creates a Kubernetes client from the in-cluster configuration
	config, err := rest.InClusterConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	// Get the secret from the kube-system namespace
	secret, err := clientset.CoreV1().Secrets("kube-system").Get(ctx, "ibm-secret", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get secret 'ibm-secret': %w", err)
	}

	// Extract the API key from the secret's data.
	apiKey, ok := secret.Data["apiKey"]
	if !ok {
		return "", fmt.Errorf("apiKey not found in secret 'ibm-secret'")
	}

	return string(apiKey), nil
}

func createSecretsManagerClient(apiKey string) (*sm.SecretsManagerV2, error) {
	// Create an IAM authenticator with your API key
	authenticator := &core.IamAuthenticator{
		ApiKey: apiKey,
	}

	// Create a new Secrets Manager service client
	// Replace the URL with your specific region's endpoint
	// e.g., "https://{instance_ID}.us-south.secrets-manager.appdomain.cloud"
	secretsManagerAPI, err := sm.NewSecretsManagerV2(&sm.SecretsManagerV2Options{
		Authenticator: authenticator,
		URL:           "https://afa20521-cd75-4864-843f-e59fd0ffd49d.us-south.secrets-manager.appdomain.cloud", // Replace with your instance URL
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create secrets manager client: %w", err)
	}

	return secretsManagerAPI, nil
}

func getGitHubPAT(secretsManagerAPI *sm.SecretsManagerV2, secretID string) (string, error) {
	// Get the secret payload using the secret's ID
	secret, _, err := secretsManagerAPI.GetSecret(
		&sm.GetSecretOptions{
			ID: &secretID,
		},
	)
	if err != nil {
		return "", fmt.Errorf("failed to get secret: %w", err)
	}

	// Use a type assertion with the comma-ok idiom to safely cast the secret
	arbitrarySecret, ok := secret.(*sm.ArbitrarySecret)
	if !ok {
		// If the secret isn't of type ArbitrarySecret, return an error
		return "", fmt.Errorf("secret with ID %s is not an ArbitrarySecret", secretID)
	}

	// Check for a nil payload to prevent a panic
	if arbitrarySecret.Payload == nil {
		return "", fmt.Errorf("secret payload is empty")
	}

	// Extract the payload (which is your GitHub PAT)
	// The Payload is already a *string, so we can directly dereference it.
	pat := *arbitrarySecret.Payload

	pat = strings.TrimSpace(pat)

	return pat, nil
}

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
				Name: "entrypoint",
				Value: tektonv1.ParamValue{
					Type:      tektonv1.ParamTypeString,
					StringVal: deployment.Spec.Ansible.AnsiblePlaybook, // Assumes Entrypoint is a new field on your spec
				},
			},
			{
				Name: "params-file",
				Value: tektonv1.ParamValue{
					Type:      tektonv1.ParamTypeString,
					StringVal: deployment.Spec.Parameters,
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

	latestPipelineRun := &tektonv1.PipelineRun{}
	if err := kclient.Get(ctx, types.NamespacedName{Name: pipelineRun.Name, Namespace: pipelineRun.Namespace}, latestPipelineRun); err != nil {
		return nil, fmt.Errorf("failed to get latest PipelineRun status: %w", err)
	}

	return latestPipelineRun, nil
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
