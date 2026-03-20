package rbac

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"

	backoff "github.com/cenkalti/backoff/v5"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
	ibm "github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
)

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;create;update

var rbacLog = ctrl.Log.WithName("RBAC")

const retryMaxElapsed = 360 * time.Second

// retry is a generic helper that wraps backoff.Retry with a standard exponential backoff policy.
func retry[T any](ctx context.Context, op func() (T, error)) (T, error) {
	b := backoff.NewExponentialBackOff()
	opts := []backoff.RetryOption{
		backoff.WithBackOff(b),
		backoff.WithMaxElapsedTime(retryMaxElapsed),
	}
	return backoff.Retry(ctx, op, opts...)
}

// getGitHubPAT retrieves the IBM API key, creates the Secrets Manager client,
// and fetches the GitHub PAT. This is the shared path used by both CreateArgoCDRBAC
// and createPipelineSecretAndMount.
func getGitHubPAT(ctx context.Context, cfg config.OperatorConfig) (string, error) {
	apiKey, err := retry(ctx, func() (string, error) {
		rbacLog.Info("Attempting to retrieve IBM API Key.")
		return ibm.GetIBMAPIKey(ctx)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get IBM API key: %w", err)
	}

	smClient, err := retry(ctx, func() (ibm.SecretsManagerClient, error) {
		rbacLog.Info("Attempting to create IBM Secrets Manager client.")
		return ibm.CreateSecretsManagerClient(apiKey, cfg)
	})
	if err != nil {
		return "", fmt.Errorf("failed to create Secrets Manager client: %w", err)
	}

	pat, err := retry(ctx, func() (string, error) {
		rbacLog.Info("Attempting to retrieve the GitHub token.")
		return ibm.GetGitHubPAT(smClient, cfg.SecretsManagerSecretID)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get GitHub PAT: %w", err)
	}

	return pat, nil
}

func CreateArgoCDRBAC(ctx context.Context, c client.Client) error {

	cfg, err := config.LoadFromConfigMap(ctx, c)
	if err != nil {
		return err
	}

	githubPAT, err := getGitHubPAT(ctx, cfg)
	if err != nil {
		return err
	}

	_, err = retry(ctx, func() (*corev1.Secret, error) {
		rbacLog.Info(fmt.Sprintf("Attempting to create secret: %s.", config.ArgoCDSecretName))
		return createArgoCDSecret(ctx, c, config.ArgoCDNamespace, config.ArgoCDSecretName, cfg.ArgoCDRepoURL, config.GitHubUsername, githubPAT)
	})
	if err != nil {
		return fmt.Errorf("failed to create ArgoCD GitHub secret: %w", err)
	}

	return nil
}

func createArgoCDSecret(ctx context.Context, c client.Client, namespace, secretName, repo, username, pat string) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				"argocd.argoproj.io/secret-type": "repository",
			},
		},
		Type: corev1.SecretTypeBasicAuth,
		StringData: map[string]string{
			"url":      repo,
			"username": username,
			"password": pat,
		},
	}

	err := c.Create(ctx, secret)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return secret, nil
		}
		return nil, fmt.Errorf("failed to create secret %s in namespace %s: %w", secretName, namespace, err)
	}

	return secret, nil
}

func Run(ctx context.Context, c client.Client) error {
	if err := createPipelineRBAC(ctx, c); err != nil {
		rbacLog.Error(err, "Failed to create pipeline RBAC")
		return err
	}
	rbacLog.Info("RBAC role created successfully.")

	cfg, err := config.LoadFromConfigMap(ctx, c)
	if err != nil {
		return err
	}

	if err := createPipelineSecretAndMount(ctx, c, cfg); err != nil {
		rbacLog.Error(err, "Failed to create required GitHub secret")
		return err
	}

	return nil
}

func createPipelineRBAC(ctx context.Context, kclient client.Client) error {
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind: "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pipeline-clusteradmin-crb",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      config.PipelineServiceAccount,
				Namespace: config.PipelineNamespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}

	existingCRB := &rbacv1.ClusterRoleBinding{}
	err := kclient.Get(ctx, client.ObjectKey{Name: crb.Name}, existingCRB)
	if err != nil {
		if errors.IsNotFound(err) {
			return kclient.Create(ctx, crb)
		}
		return err
	}

	return kclient.Update(ctx, crb)
}

// createGitHubSecret creates a Kubernetes Secret to store the GitHub PAT.
func createGitHubSecret(ctx context.Context, c client.Client, namespace, secretName, username, pat string) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Annotations: map[string]string{
				"tekton.dev/git-0": "https://github.ibm.com",
			},
		},
		Type: corev1.SecretTypeBasicAuth,
		StringData: map[string]string{
			"username": username,
			"password": pat,
		},
	}

	err := c.Create(ctx, secret)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			return secret, nil
		}
		return nil, fmt.Errorf("failed to create secret %s in namespace %s: %w", secretName, namespace, err)
	}

	return secret, nil
}

// mountSecretToServiceAccount mounts a secret to a service account.
func mountSecretToServiceAccount(ctx context.Context, c client.Client, namespace, saName, secretName string) error {
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: saName, Namespace: namespace}, sa); err != nil {
		return fmt.Errorf("failed to get service account %s: %w", saName, err)
	}

	// Check if the secret is already mounted
	for _, secretRef := range sa.Secrets {
		if secretRef.Name == secretName {
			return nil // already mounted, nothing to do
		}
	}

	sa.Secrets = append(sa.Secrets, corev1.ObjectReference{Name: secretName})
	sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: secretName})

	if err := c.Update(ctx, sa); err != nil {
		return fmt.Errorf("failed to update service account %s: %w", saName, err)
	}

	return nil
}

// createPipelineSecretAndMount creates the GitHub secret and mounts it to the pipeline service account.
func createPipelineSecretAndMount(ctx context.Context, c client.Client, cfg config.OperatorConfig) error {
	githubPAT, err := getGitHubPAT(ctx, cfg)
	if err != nil {
		return err
	}

	_, err = retry(ctx, func() (*corev1.Secret, error) {
		rbacLog.Info(fmt.Sprintf("Attempting to create secret: %s.", config.GitHubSecretName))
		return createGitHubSecret(ctx, c, config.PipelineNamespace, config.GitHubSecretName, config.GitHubUsername, githubPAT)
	})
	if err != nil {
		return fmt.Errorf("failed to create GitHub secret: %w", err)
	}

	_, err = retry(ctx, func() (struct{}, error) {
		rbacLog.Info(fmt.Sprintf("Attempting to mount %s to %s.", config.GitHubSecretName, config.PipelineServiceAccount))
		return struct{}{}, mountSecretToServiceAccount(ctx, c, config.PipelineNamespace, config.PipelineServiceAccount, config.GitHubSecretName)
	})
	if err != nil {
		return fmt.Errorf("failed to mount secret to service account: %w", err)
	}

	rbacLog.Info(fmt.Sprintf("Successfully created and mounted secret %s to service account %s in namespace %s.", config.GitHubSecretName, config.PipelineServiceAccount, config.PipelineNamespace))

	return nil
}
