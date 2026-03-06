package utils

import (
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

// NewClient builds a controller-runtime client using the provided scheme.
// Intended for packages (argocd, rbac) that run before the manager starts
// and therefore cannot use the manager's injected client.
func NewClient(scheme *runtime.Scheme) (client.Client, error) {
	return client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
}

func Int32(v int32) *int32 {
	return &v
}

func StrPrt(s string) *string {
	return &s
}
