package argocd

import (
	"context"
	"errors"
	"time"

	"strconv"

	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
	rbac "github.ibm.com/itz-content/itz-deployer-operator/pkg/rbac"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

func argoApplication(client client.Client) error {
	isDeployed, _ := getApplication(client)
	if !isDeployed {
		if err := createApplication(client); err != nil {
			return err
		}
	}
	return waitForTasks(client)
}

func getApplication(kclient client.Client) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("argoproj.io/v1alpha1")
	u.SetKind("Application")
	err := kclient.Get(context.Background(), client.ObjectKey{
		Namespace: config.ArgoCDNamespace,
		Name:      config.ArgoCDApplicationName,
	}, u)
	if err != nil {
		argoCDLog.Error(err, err.Error())
		return false, err
	}
	argoCDLog.Info("Application: " + u.GetName())
	isFound := u.GetName() == config.ArgoCDApplicationName
	argoCDLog.Info("Application Found: " + strconv.FormatBool(isFound))
	return isFound, nil
}

func createApplication(kclient client.Client) error {
	if err := rbac.CreateArgoCDRBAC(kclient); err != nil {
		return err
	}

	application := &unstructured.Unstructured{}
	application.Object = map[string]any{
		"metadata": map[string]any{
			"name":      config.ArgoCDApplicationName,
			"namespace": config.ArgoCDNamespace,
		},
		"spec": map[string]any{
			"project": "default",
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": "default",
			},
			"source": map[string]any{
				"directory": map[string]any{
					"recurse": true,
					"exclude": "{**/*/catalog-info.yaml,**/*/tests/**}",
				},
				"repoURL":        config.ArgoCDRepoURL,
				"targetRevision": "main",
				"path":           "tasks",
			},
			"syncPolicy": map[string]any{
				"automated": map[string]any{
					"prune":    true,
					"selfHeal": true,
				},
				"syncOptions": []string{
					"CreateNamespace=true",
				},
			},
		},
	}
	application.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "argoproj.io",
		Kind:    "Application",
		Version: "v1alpha1",
	})

	if err := kclient.Create(context.TODO(), application); err != nil {
		argoCDLog.Error(err, err.Error())
		return err
	}
	return nil
}

func waitForTasks(kclient client.Client) error {
	timeout := time.After(600 * time.Second)

	// Check health every 10 seconds
	checkTicker := time.NewTicker(10 * time.Second)
	defer checkTicker.Stop()

	// Delete and reinstall the application every 60 seconds if not healthy
	deleteTicker := time.NewTicker(60 * time.Second)
	defer deleteTicker.Stop()

	for {
		select {
		case <-timeout:
			return errors.New("tasks creation took too long to install")

		case <-deleteTicker.C:
			u := &unstructured.Unstructured{}
			u.SetName(config.ArgoCDApplicationName)
			u.SetNamespace(config.ArgoCDNamespace)
			u.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "argoproj.io",
				Kind:    "Application",
				Version: "v1alpha1", // was incorrectly v1beta1
			})
			if err := kclient.Delete(context.Background(), u); err != nil {
				argoCDLog.Error(err, err.Error())
			}
			argoCDLog.Info("Deleted application, reinstalling...")
			if err := createApplication(kclient); err != nil {
				argoCDLog.Error(err, "Failed to recreate application")
			}

		case <-checkTicker.C:
			u := &unstructured.Unstructured{}
			u.SetAPIVersion("argoproj.io/v1alpha1")
			u.SetKind("Application")
			err := kclient.Get(context.Background(), client.ObjectKey{
				Namespace: config.ArgoCDNamespace,
				Name:      config.ArgoCDApplicationName,
			}, u)
			if err != nil {
				argoCDLog.Error(err, err.Error())
				continue
			}

			status, _ := u.Object["status"].(map[string]any)
			syncStatus, _ := status["sync"].(map[string]any)
			syncCode, _ := syncStatus["status"].(string)
			healthStatus, _ := status["health"].(map[string]any)
			healthCode, _ := healthStatus["status"].(string)

			if syncCode == "Synced" && healthCode == "Healthy" {
				return nil
			}
			argoCDLog.Info("Application not yet Synced and Healthy. Waiting 10 seconds and trying again.")
		}
	}
}
