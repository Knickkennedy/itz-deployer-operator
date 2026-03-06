package config

import (
	"context"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ArgoCD
	ArgoCDNamespace       = "openshift-gitops"
	ArgoCDInstanceName    = "openshift-gitops"
	ArgoCDApplicationName = "deployer-tekton-tasks"
	ArgoCDRepoURL         = "https://github.ibm.com/itz-content/deployer-tekton-tasks.git"
	ArgoCDSecretName      = "ibm-connection-auth"

	// Pipeline / RBAC
	PipelineServiceAccount = "pipeline"
	PipelineNamespace      = "default"
	GitHubSecretName       = "github-ibm-pat"
	GitHubUsername         = "x-oauth-basic"
	IBMSecretName          = "ibm-secret"
	IBMSecretNamespace     = "kube-system"
)

type OperatorConfig struct {
	SecretsManagerURL      string
	SecretsManagerSecretID string
}

func LoadFromConfigMap(ctx context.Context, c client.Client) (OperatorConfig, error) {
	namespace, err := getOperatorNamespace()
	if err != nil {
		return OperatorConfig{}, fmt.Errorf("failed to determine operator namespace: %w", err)
	}

	cm := &corev1.ConfigMap{}
	err = c.Get(ctx, types.NamespacedName{
		Name:      "itz-deployer-config",
		Namespace: namespace,
	}, cm)
	if err != nil {
		return OperatorConfig{}, fmt.Errorf("failed to get operator configmap: %w", err)
	}
	return OperatorConfig{
		SecretsManagerURL:      cm.Data["secretsManagerURL"],
		SecretsManagerSecretID: cm.Data["secretsManagerSecretID"],
	}, nil
}

func getOperatorNamespace() (string, error) {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to read namespace file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
