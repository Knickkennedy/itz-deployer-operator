package rbac

import (
	"context"
	"fmt"

	utlis "github.ibm.com/itz-content/itz-deployer-operator/pkg"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"

	ibm "github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
)

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;create;update

const (
	serviceAccount = "pipeline"
	namespace      = "default"
	gitUsername    = "x-oauth-basic"
	secretName     = "github-ibm-pat"
	secretID       = "4560e15e-8fb8-37d9-35f5-01fe2c4b77a7"
)

func Run() error {
	client, _ := utlis.CreateClient()
	ctrl.Log.WithName("RBAC")
	err := createPipelineRBAC(client)
	if err != nil {
		ctrl.Log.Error(err, err.Error())
		return err
	}
	ctrl.Log.Info("RBAC role created successfully.")

	err = createPipelineSecretAndMount(
		context.TODO(),
		client,
		namespace,
		serviceAccount,
		secretID,
		secretName,
		gitUsername,
	)
	if err != nil {
		ctrl.Log.Error(err, "Failed to create required github secret")
		return err
	}

	return nil
}

func createPipelineRBAC(kclient client.Client) error {
	rbac := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind: "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pipeline-clusteradmin-crb",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      serviceAccount,
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}

	existingRBAC := &rbacv1.ClusterRoleBinding{}
	err := kclient.Get(context.TODO(), client.ObjectKey{Name: rbac.Name}, existingRBAC)

	if err != nil {
		if errors.IsNotFound(err) {
			// If it doesn't exist, create it.
			return kclient.Create(context.TODO(), rbac)
		}
		// Return other errors.
		return err
	}

	// If it exists, update it if necessary.
	return kclient.Update(context.TODO(), rbac)
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
		return nil, fmt.Errorf("failed to create secret %s in namespace %s: %w", secretName, namespace, err)
	}

	return secret, nil
}

// mountSecretToServiceAccount mounts a secret to a service account.
func mountSecretToServiceAccount(ctx context.Context, c client.Client, namespace, saName, secretName string) error {
	sa := &corev1.ServiceAccount{}
	err := c.Get(ctx, types.NamespacedName{Name: saName, Namespace: namespace}, sa)
	if err != nil {
		return fmt.Errorf("failed to get service account %s: %w", saName, err)
	}

	// Check if the secret is already mounted
	secretExists := false
	for _, secretRef := range sa.Secrets {
		if secretRef.Name == secretName {
			secretExists = true
			break
		}
	}
	if !secretExists {
		sa.Secrets = append(sa.Secrets, corev1.ObjectReference{Name: secretName})
		sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: secretName})
	}

	err = c.Update(ctx, sa)
	if err != nil {
		return fmt.Errorf("failed to update service account %s: %w", saName, err)
	}

	return nil
}

// createPipelineSecretAndMount creates the GitHub secret and mounts it to the specified service account.
func createPipelineSecretAndMount(ctx context.Context, c client.Client, namespace, saName, secretID, secretName, username string) error {
	// Step 1: Retrieve the IBM API Key (your ibm package logic is unchanged)
	apiKey, err := ibm.GetIBMAPIKey(ctx)
	if err != nil {
		return fmt.Errorf("failed to get IBM API key: %w", err)
	}

	// Step 2: Create the Secrets Manager client (your ibm package logic is unchanged)
	smClient, err := ibm.CreateSecretsManagerClient(apiKey)
	if err != nil {
		return fmt.Errorf("failed to create Secrets Manager client: %w", err)
	}

	// Step 3: Retrieve the GitHub PAT from Secrets Manager (your ibm package logic is unchanged)
	githubPAT, err := ibm.GetGitHubPAT(smClient, secretID)
	if err != nil {
		return fmt.Errorf("failed to get GitHub PAT: %w", err)
	}

	// Step 4: Create the Kubernetes Secret using the new client
	_, err = createGitHubSecret(ctx, c, namespace, secretName, username, githubPAT)
	if err != nil {
		return fmt.Errorf("failed to create GitHub secret: %w", err)
	}

	// Step 5: Mount the secret to the ServiceAccount using the new client
	err = mountSecretToServiceAccount(ctx, c, namespace, saName, secretName)
	if err != nil {
		return fmt.Errorf("failed to mount secret to service account: %w", err)
	}

	fmt.Printf("Successfully created and mounted secret %s to service account %s in namespace %s.\n", secretName, saName, namespace)

	return nil
}
