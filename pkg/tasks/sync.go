package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/ibm"
	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const retryMaxElapsed = 360 * time.Second

func retry[T any](ctx context.Context, op func() (T, error)) (T, error) {
	b := backoff.NewExponentialBackOff()
	opts := []backoff.RetryOption{
		backoff.WithBackOff(b),
		backoff.WithMaxElapsedTime(retryMaxElapsed),
	}
	return backoff.Retry(ctx, op, opts...)
}

var log = ctrl.Log.WithName("tasks")

type treeResponse struct {
	Tree []treeEntry `json:"tree"`
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // "blob" or "tree"
	URL  string `json:"url"`
}

func Sync(ctx context.Context, c client.Client, cfg config.OperatorConfig) error {
	pat, err := fetchPAT(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to fetch PAT: %w", err)
	}

	repoURL := cfg.ArgoCDRepoURL // e.g. https://github.ibm.com/itz-content/deployer-tekton-tasks.git
	owner, repo, host, err := parseRepoURL(repoURL)
	if err != nil {
		return fmt.Errorf("failed to parse repo URL: %w", err)
	}

	apiBase := fmt.Sprintf("https://%s/api/v3", host)
	treeURL := fmt.Sprintf("%s/repos/%s/%s/git/trees/main?recursive=1", apiBase, owner, repo)

	entries, err := fetchTree(ctx, treeURL, pat)
	if err != nil {
		return fmt.Errorf("failed to fetch repo tree: %w", err)
	}

	rawBase := fmt.Sprintf("https://raw.%s/%s/%s/main", host, owner, repo)
	applied := 0
	for _, entry := range entries {
		if entry.Type != "blob" || !strings.HasSuffix(entry.Path, ".yaml") {
			continue
		}
		rawURL := fmt.Sprintf("%s/%s", rawBase, entry.Path)
		if err := applyFile(ctx, c, rawURL, pat); err != nil {
			log.Error(err, "Failed to apply task file", "path", entry.Path)
			continue
		}
		applied++
	}

	log.Info("Tekton tasks synced", "applied", applied)
	return nil
}

func fetchPAT(ctx context.Context, cfg config.OperatorConfig) (string, error) {
	apiKey, err := retry(ctx, func() (string, error) {
		log.Info("Attempting to retrieve IBM API Key.")
		return ibm.GetIBMAPIKey(ctx)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get IBM API key: %w", err)
	}

	smClient, err := retry(ctx, func() (ibm.SecretsManagerClient, error) {
		log.Info("Attempting to create IBM Secrets Manager client.")
		return ibm.CreateSecretsManagerClient(apiKey, cfg)
	})
	if err != nil {
		return "", fmt.Errorf("failed to create Secrets Manager client: %w", err)
	}

	pat, err := retry(ctx, func() (string, error) {
		log.Info("Attempting to retrieve GitHub token.")
		return ibm.GetGitHubPAT(smClient, cfg.SecretsManagerSecretID)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get GitHub PAT: %w", err)
	}
	return pat, nil
}

func parseRepoURL(repoURL string) (owner, repo, host string, err error) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", "", err
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("unexpected repo URL format: %s", repoURL)
	}
	return parts[0], parts[1], u.Host, nil
}

func fetchTree(ctx context.Context, treeURL, pat string) ([]treeEntry, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", treeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+pat)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tree API returned %d", resp.StatusCode)
	}
	var tree treeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return nil, err
	}
	return tree.Tree, nil
}

func applyFile(ctx context.Context, c client.Client, rawURL, pat string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+pat)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s returned %d", rawURL, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	docReader := yaml.NewDocumentDecoder(io.NopCloser(bytes.NewReader(body)))
	defer docReader.Close()

	buf := make([]byte, 1024*1024)
	for {
		n, err := docReader.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading document: %w", err)
		}
		chunk := bytes.TrimSpace(buf[:n])
		if len(chunk) == 0 {
			continue
		}
		var rawObj interface{}
		if err := yamlv3.Unmarshal(chunk, &rawObj); err != nil {
			return fmt.Errorf("decode error: %w", err)
		}
		jsonBytes, err := json.Marshal(rawObj)
		if err != nil {
			return fmt.Errorf("json marshal: %w", err)
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(jsonBytes, &obj.Object); err != nil {
			return fmt.Errorf("json unmarshal: %w", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetNamespace() == "" {
			obj.SetNamespace("default")
		}
		if err := c.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner("itz-deployer-operator")); err != nil {
			return fmt.Errorf("apply failed for %s: %w", obj.GetName(), err)
		}
	}
	return nil
}
