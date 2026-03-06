package ibm

import (
	"context"
	"fmt"
	"strings"

	"github.com/IBM/go-sdk-core/v5/core"
	sm "github.com/IBM/secrets-manager-go-sdk/v2/secretsmanagerv2"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// SecretsManagerClient is an interface over *sm.SecretsManagerV2, allowing
// the type to be used as a generic type parameter in retry helpers.
type SecretsManagerClient = *sm.SecretsManagerV2

func GetIBMAPIKey(ctx context.Context) (string, error) {
	// Creates a Kubernetes client from the in-cluster configuration
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return "", fmt.Errorf("failed to get in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("failed to create clientset: %w", err)
	}

	// Get the secret from the kube-system namespace
	secret, err := clientset.CoreV1().Secrets(config.IBMSecretNamespace).Get(ctx, config.IBMSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get secret '%s': %w", config.IBMSecretName, err)
	}

	// Extract the API key from the secret's data.
	apiKey, ok := secret.Data["apiKey"]
	if !ok {
		return "", fmt.Errorf("apiKey not found in secret '%s'", config.IBMSecretName)
	}

	return string(apiKey), nil
}

func CreateSecretsManagerClient(apiKey string, cfg config.OperatorConfig) (*sm.SecretsManagerV2, error) {
	// Create an IAM authenticator with your API key
	authenticator := &core.IamAuthenticator{
		ApiKey: apiKey,
	}

	// Create a new Secrets Manager service client
	// Replace the URL with your specific region's endpoint
	// e.g., "https://{instance_ID}.us-south.secrets-manager.appdomain.cloud"
	secretsManagerAPI, err := sm.NewSecretsManagerV2(&sm.SecretsManagerV2Options{
		Authenticator: authenticator,
		URL:           cfg.SecretsManagerURL, // Replace with your instance URL
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create secrets manager client: %w", err)
	}

	return secretsManagerAPI, nil
}

func GetGitHubPAT(secretsManagerAPI *sm.SecretsManagerV2, secretID string) (string, error) {
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
