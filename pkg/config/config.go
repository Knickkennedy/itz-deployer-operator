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
	GitBaseURL             string
	ArgoCDRepoURL          string
	AnsibleRunnerImage     string
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
	cfg := OperatorConfig{
		SecretsManagerURL:      cm.Data["secretsManagerURL"],
		SecretsManagerSecretID: cm.Data["secretsManagerSecretID"],
		GitBaseURL:             cm.Data["gitBaseURL"],
		ArgoCDRepoURL:          cm.Data["argoCDRepoURL"],
		AnsibleRunnerImage:     cm.Data["ansibleRunnerImage"],
	}

	required := map[string]string{
		"secretsManagerURL":      cfg.SecretsManagerURL,
		"secretsManagerSecretID": cfg.SecretsManagerSecretID,
		"gitBaseURL":             cfg.GitBaseURL,
		"argoCDRepoURL":          cfg.ArgoCDRepoURL,
		"ansibleRunnerImage":     cfg.AnsibleRunnerImage,
	}
	for key, val := range required {
		if val == "" {
			return OperatorConfig{}, fmt.Errorf("operator configmap is missing required key %q", key)
		}
	}
	return cfg, nil
}

func getOperatorNamespace() (string, error) {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to read namespace file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
