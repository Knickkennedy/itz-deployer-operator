package argocd

import (
	"context"
	"errors"
	"time"

	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

func argoApplication(client client.Client) error {
	isDeployed, _ := getApplication(client)
	if !isDeployed {
		createApplication(client)
	}
	err := waitForTasks(client)
	if err != nil {
		return err
	}
	return nil
}

var applicationName = "deployer-tekton-tasks"

func getApplication(kclient client.Client) (bool, error) {
	isFound := false
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("argoproj.io/v1alpha1")
	u.SetKind("Application")
	err := kclient.Get(context.Background(), client.ObjectKey{
		Namespace: "openshift-gitops",
		Name:      applicationName,
	}, u)
	if err != nil {
		argoCDLog.Error(err, err.Error())
		return isFound, err
	}
	argoCDLog.Info("Application: " + u.GetName())
	appName := u.GetName()
	isFound = (appName == applicationName)
	argoCDLog.Info("Application Found: " + strconv.FormatBool(isFound))
	return isFound, nil
}

func createApplication(kclient client.Client) {
	application := &unstructured.Unstructured{}
	application.Object = map[string]any{
		"metadata": map[string]any{
			"name":      "deployer-tekton-tasks",
			"namespace": "openshift-gitops",
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
				"repoURL":        "https://github.com/itz-public/deployer-tekton-tasks.git",
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
	err := kclient.Create(context.TODO(), application)
	if err != nil {
		argoCDLog.Error(err, err.Error())
	}
}

func waitForTasks(kclient client.Client) error {
	timeout := time.After(600 * time.Second)
	// Check every 5 seconds
	check := time.Tick(10 * time.Second)
	// delete the application and reinstall it after a min
	delete := time.Tick(60 * time.Second)
	// Keeps track of installed csvs
	// Keep trying until we're timed out or got a result or got an error
	for {
		select {
		// Got a timeout! fail with a timeout error
		case <-timeout:
			return errors.New("tasks creation took too long to install")
		case <-delete:
			u := &unstructured.Unstructured{}
			u.SetName("deployer-tekton-tasks")
			u.SetNamespace("openshift-gitops")
			u.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "argoproj.io",
				Kind:    "Application",
				Version: "v1beta1",
			})
			err := kclient.Delete(context.Background(), u)
			if err != nil {
				argoCDLog.Error(err, err.Error())
			}
			argoCDLog.Info("Deleted application")
			createApplication(kclient)
		case <-check:
			u := &unstructured.Unstructured{}
			u.SetAPIVersion("argoproj.io/v1alpha1")
			u.SetKind("Application")
			err := kclient.Get(context.Background(), client.ObjectKey{
				Namespace: "openshift-gitops",
				Name:      applicationName,
			}, u)
			if err != nil {
				argoCDLog.Error(err, err.Error())
			}

			status, _ := u.Object["status"].(map[string]any)
			syncStatus, _ := status["sync"].(map[string]any)
			syncCode, _ := syncStatus["status"].(string)
			healthStatus, _ := status["health"].(map[string]any)

			healthCode, _ := healthStatus["status"].(string)
			if syncCode == "Synced" && healthCode == "Healthy" {
				return nil
			}

			// t := &unstructured.Unstructured{}
			// t.SetAPIVersion("tekton.dev/v1beta1")
			// t.SetKind("Task")
			// err := kclient.Get(context.Background(), client.ObjectKey{
			// 	Namespace: "default",
			// 	Name:      "ibm-pak",
			// }, t)
			// if err != nil {
			// 	argoCDLog.Error(err, err.Error())
			// }
			// if t.GetName() == "ibm-pak" {
			// 	// Yay tasks are there, good to breakout
			// 	return nil
			// }
			argoCDLog.Info("deployer-tekton-task application health is not Synced and Healthy. Waiting 10 seconds and trying again.")
		}
	}
}
