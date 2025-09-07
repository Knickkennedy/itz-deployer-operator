package rbac

import (
	"context"

	utlis "github.ibm.com/itz-content/itz-deployer-operator/pkg"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;create;update

func Run() error {
	client, _ := utlis.CreateClient()
	err := createPipelineRBAC(client)
	if err != nil {
		ctrl.Log.Error(err, err.Error())
		return err
	}
	ctrl.Log.Info("RBAC role created successfully.")
	return nil
}

func createPipelineRBAC(kclient client.Client) error {
	rbac := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind: "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pipeline-clusteradmin-crb",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "pipeline",
				Namespace: "default",
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}

	existingRBAC := &rbacv1.ClusterRoleBinding{}
	err := kclient.Get(context.TODO(), client.ObjectKey{Name: rbac.Name}, existingRBAC)

	if err != nil {
		if errors.IsNotFound(err) {
			// If it doesn't exist, create it.
			return kclient.Create(context.TODO(), rbac)
		}
		// Return other errors.
		return err
	}

	// If it exists, update it if necessary.
	return kclient.Update(context.TODO(), rbac)
}
