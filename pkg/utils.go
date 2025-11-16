package utils

import (
	v1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	configv1 "github.com/openshift/api/config/v1"
	consolev1 "github.com/openshift/api/console/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	pipelinev1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	deployerv1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

func CreateClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	v1alpha1.AddToScheme(scheme)
	clientgoscheme.AddToScheme(scheme)
	consolev1.AddToScheme(scheme) // ConsolePlugin
	routev1.AddToScheme(scheme)
	configv1.AddToScheme(scheme)         // ClusterVersion
	deployerv1alpha1.AddToScheme(scheme) // deployer
	v1beta1.AddToScheme(scheme)          // external secrets
	pipelinev1.AddToScheme(scheme)       // pipeline

	kubeconfig := ctrl.GetConfigOrDie()
	kclient, err := client.New(kubeconfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return kclient, nil
}

func Int32(v int32) *int32 {
	return &v
}

func StrPrt(s string) *string {
	return &s
}
