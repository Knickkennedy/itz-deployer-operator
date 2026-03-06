package argocd

import (
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

var argoCDLog logr.Logger

func initLogger() {
	argoCDLog = ctrl.Log.WithName("ArgoCD")
	println(">>>> ArgoCD Run() called") // raw stdout
}

func Run(c client.Client) {
	defer func() {
		if r := recover(); r != nil {
			println("PANIC in Run():", r)
			argoCDLog.Error(nil, "Recovered from panic", "panic", r)
		}
	}()

	initLogger()
	argoCDLog.Info("Initializing ArgoCD setup...")

	if err := argoDeployment(c); err != nil {
		argoCDLog.Error(err, "Failed to deploy ArgoCD")
		return
	}

	if err := argoApplication(c); err != nil {
		argoCDLog.Error(err, "Failed to install ArgoCD application")
		return
	}

	argoCDLog.Info("ArgoCD setup completed successfully!")
}
