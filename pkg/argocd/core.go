package argocd

import (
	"github.com/go-logr/logr"
	utils "github.ibm.com/itz-content/itz-deployer-operator/pkg"
	ctrl "sigs.k8s.io/controller-runtime"
)

var argoCDLog logr.Logger

func initLogger() {
	argoCDLog = ctrl.Log.WithName("ArgoCD")
	println(">>>> ArgoCD Run() called") // raw stdout
}

func Run() {
	defer func() {
		if r := recover(); r != nil {
			println("PANIC in Run():", r)
			argoCDLog.Error(nil, "Recovered from panic", "panic", r)
		}
	}()

	initLogger()
	argoCDLog.Info("Initializing ArgoCD setup...")

	client, _ := utils.CreateClient()
	err := argoDeployment(client)
	if err != nil {
		argoCDLog.Error(err, err.Error())
	}
	argoApplication(client)
	argoCDLog.Info("ArgoCD instance created successfully!")
}
