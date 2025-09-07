package utils

import (
	v1beta1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1beta1"
	configv1 "github.com/openshift/api/config/v1"
	consolev1alpha1 "github.com/openshift/api/console/v1alpha1"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	pipelinev1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	deployerv1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

func CreateClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	v1alpha1.AddToScheme(scheme)
	appsv1.AddToScheme(scheme)           // Deployment
	corev1.AddToScheme(scheme)           // Service
	consolev1alpha1.AddToScheme(scheme)  // ConsolePlugin
	configv1.AddToScheme(scheme)         // ClusterVersion
	deployerv1alpha1.AddToScheme(scheme) // deployer
	rbacv1.AddToScheme(scheme)           // rbac
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
