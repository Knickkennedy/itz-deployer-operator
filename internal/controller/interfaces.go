package controller

import (
	"context"
	"fmt"
	"os"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	routev1 "github.com/openshift/api/route/v1"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
	ibm "github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

// errEmptyRouteHost is returned when the OpenShift console Route has no host.
var errEmptyRouteHost = fmt.Errorf("console route spec.host is empty")

// realRepoCloner clones via go-git.
type realRepoCloner struct{}

func (realRepoCloner) Clone(ctx context.Context, repoURL, release, pat string) (string, func(), error) {
	logger := logf.FromContext(ctx)

	dir, err := os.MkdirTemp("", "git-repo")
	if err != nil {
		return "", func() {}, fmt.Errorf("failed to create temp directory: %w", err)
	}

	logger.Info("Cloning repository", "URL", repoURL, "tag", release)

	_, err = git.PlainClone(dir, false, &git.CloneOptions{
		URL:           repoURL,
		ReferenceName: plumbing.ReferenceName(fmt.Sprintf("refs/tags/%s", release)),
		SingleBranch:  true,
		Auth:          &githttp.BasicAuth{Username: config.GitHubUsername, Password: pat},
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", func() {}, fmt.Errorf("failed to clone %s at tag %s: %w", repoURL, release, err)
	}

	return dir, func() { os.RemoveAll(dir) }, nil
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

// ============================================================================
// Exported constructors for use in main.go
// ============================================================================

// NewRealConfigLoader returns the production ConfigLoader.
func NewRealConfigLoader() ConfigLoader {
	return realConfigLoader{}
}

// NewRealCredentialProvider returns the production CredentialProvider.
func NewRealCredentialProvider() CredentialProvider {
	return realCredentialProvider{}
}

// NewRealRepoCloner returns the production RepoCloner.
func NewRealRepoCloner() RepoCloner {
	return realRepoCloner{}
}

// NewRealRouteResolver returns the production RouteResolver.
func NewRealRouteResolver(c client.Client) RouteResolver {
	return realRouteResolver{c: c}
}
