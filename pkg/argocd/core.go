package argocd

import (
	"context"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
)

var argoCDLog logr.Logger

func initLogger() {
	argoCDLog = ctrl.Log.WithName("ArgoCD")
}

func Run(ctx context.Context, c client.Client) {
	defer func() {
		if r := recover(); r != nil {
			argoCDLog.Error(nil, "Recovered from panic", "panic", r)
		}
	}()

	initLogger()
	argoCDLog.Info("Initializing ArgoCD setup...")

	cfg, err := config.LoadFromConfigMap(ctx, c)
	if err != nil {
		argoCDLog.Error(err, "Failed to load operator config")
		return
	}

	if err := argoDeployment(c); err != nil {
		argoCDLog.Error(err, "Failed to deploy ArgoCD")
		return
	}

	if err := argoApplication(ctx, c, cfg); err != nil {
		argoCDLog.Error(err, "Failed to install ArgoCD application")
		return
	}

	argoCDLog.Info("ArgoCD setup completed successfully!")
}
