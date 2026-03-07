package controller

import (
	"context"

	routev1 "github.com/openshift/api/route/v1"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ibm "github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
)

// ConfigLoader abstracts loading OperatorConfig so tests can inject a fake
// without needing the serviceaccount namespace file on disk.
type ConfigLoader interface {
	Load(ctx context.Context, c client.Client) (config.OperatorConfig, error)
}

// CredentialProvider abstracts the IBM Secrets Manager chain:
// in-cluster API key → Secrets Manager client → GitHub PAT.
type CredentialProvider interface {
	GetGitHubPAT(ctx context.Context, cfg config.OperatorConfig) (string, error)
}

// RepoCloner abstracts cloning a git repository to a local temp directory.
// The returned cleanup func removes the directory and should always be deferred.
type RepoCloner interface {
	Clone(ctx context.Context, repoURL, release, pat string) (dir string, cleanup func(), err error)
}

// RouteResolver abstracts fetching the external cluster base URL from the
// OpenShift console Route.
type RouteResolver interface {
	GetExternalClusterBaseURL(ctx context.Context) (string, error)
}

// ============================================================================
// Production implementations
// ============================================================================

// realConfigLoader calls config.LoadFromConfigMap, which reads the operator
// namespace from the serviceaccount file.
type realConfigLoader struct{}

func (realConfigLoader) Load(ctx context.Context, c client.Client) (config.OperatorConfig, error) {
	return config.LoadFromConfigMap(ctx, c)
}

// realCredentialProvider calls the IBM package directly.
type realCredentialProvider struct{}

func (realCredentialProvider) GetGitHubPAT(ctx context.Context, cfg config.OperatorConfig) (string, error) {
	apiKey, err := ibm.GetIBMAPIKey(ctx)
	if err != nil {
		return "", err
	}
	smClient, err := ibm.CreateSecretsManagerClient(apiKey, cfg)
	if err != nil {
		return "", err
	}
	return ibm.GetGitHubPAT(smClient, cfg.SecretsManagerSecretID)
}

// realRepoCloner clones via go-git.
type realRepoCloner struct{}

func (realRepoCloner) Clone(ctx context.Context, repoURL, release, pat string) (string, func(), error) {
	dir, err := cloneRepoImpl(ctx, repoURL, release, pat)
	if err != nil {
		return "", func() {}, err
	}
	return dir, func() {
		_ = removeAll(dir)
	}, nil
}

// realRouteResolver fetches the console Route from the cluster.
type realRouteResolver struct {
	c client.Client
}

func (r realRouteResolver) GetExternalClusterBaseURL(ctx context.Context) (string, error) {
	consoleRoute := &routev1.Route{}
	if err := r.c.Get(ctx, types.NamespacedName{
		Namespace: "openshift-console",
		Name:      "console",
	}, consoleRoute); err != nil {
		return "", err
	}
	if consoleRoute.Spec.Host == "" {
		return "", errEmptyRouteHost
	}
	return "https://" + consoleRoute.Spec.Host, nil
}
