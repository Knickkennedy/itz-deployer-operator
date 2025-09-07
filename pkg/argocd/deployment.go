package argocd

import (
	"context"
	"errors"
	"fmt"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	utils "github.ibm.com/itz-content/itz-deployer-operator/pkg"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// resource exclusions for the ArgoCD CR.
type ArgoCDResource struct {
	APIGroups []string `json:"apiGroups"`
	Kinds     []string `json:"kinds"`
	Clusters  []string `json:"clusters"`
}

var name = "openshift-gitops"

func argoDeployment(client client.Client) error {
	u, err := getDeployment(client, name, "openshift-gitops")
	if err != nil {
		argoCDLog.Error(err, err.Error())
	}

	isDeployed := (u.GetName() == name)
	if !isDeployed {
		argoCDLog.Info("ArgoCD deployment not found... deploying now.")
		createDeployment(client)
	}
	argoCDLog.Info("Waiting for ArgoCD Deployment to be ready...")
	err = waitForDeployment(client)
	if err != nil {
		argoCDLog.Error(err, err.Error())
		return err
	}
	argoCDLog.Info("ArgoCD ready! Now installing application")
	return nil
}

func getDeployment(kclient client.Client, name, namespace string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("argoproj.io/v1beta1")
	u.SetKind("ArgoCD")

	err := kclient.Get(context.Background(), client.ObjectKey{
		Name:      name,
		Namespace: namespace,
	}, u)
	if err != nil {
		return u, err
	}
	return u, nil
}

func createDeployment(kclient client.Client) error {
	// Prepare the resource exclusions YAML string
	resources, err := yaml.Marshal([]ArgoCDResource{
		{
			APIGroups: []string{"internal.open-cluster-management.io"},
			Kinds:     []string{"ManagedClusterInfo"},
			Clusters:  []string{"*"},
		},
		{
			APIGroups: []string{"clusterview.open-cluster-management.io"},
			Kinds:     []string{"ManagedClusterInfo"},
			Clusters:  []string{"*"},
		},
		{
			APIGroups: []string{"view.open-cluster-management.io"},
			Kinds:     []string{"ManagedClusterView"},
			Clusters:  []string{"*"},
		},
		{
			APIGroups: []string{"tekton.dev"},
			Kinds:     []string{"TaskRun", "PipelineRun"},
			Clusters:  []string{"*"},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to marshal ArgoCDResource slice to YAML: %w", err)
	}

	argoDeployment := ArgoCD{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ArgoCD",
			APIVersion: "argoproj.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openshift-gitops-otp",
			Namespace: "openshift-gitops",
		},
		Spec: ArgoCDSpec{
			ApplicationSet: &ArgoCDApplicationSet{
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("1Gi"),
						corev1.ResourceCPU:    resource.MustParse("2"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("512Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
				},
			},
			Controller: ArgoCDApplicationControllerSpec{
				Processors: ArgoCDApplicationControllerProcessorsSpec{},
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("2048Mi"),
						corev1.ResourceCPU:    resource.MustParse("2000m"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("1024Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
				},
				Sharding: ArgoCDApplicationControllerShardSpec{},
			},
			Grafana: ArgoCDGrafanaSpec{
				Enabled: false,
				Ingress: ArgoCDIngressSpec{Enabled: false},
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("256Mi"),
						corev1.ResourceCPU:    resource.MustParse("500m"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
				},
				Route: ArgoCDRouteSpec{
					Enabled: false,
					TLS: &routev1.TLSConfig{
						InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
						Termination:                   routev1.TLSTerminationEdge,
					},
				},
				Size: utils.Int32(1),
			},
			HA: ArgoCDHASpec{
				Enabled: false,
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("256Mi"),
						corev1.ResourceCPU:    resource.MustParse("500m"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
				},
			},
			InitialSSHKnownHosts:  SSHHostsSpec{},
			KustomizeBuildOptions: "--enable-alpha-plugins",
			Prometheus: ArgoCDPrometheusSpec{
				Enabled: false,
				Ingress: ArgoCDIngressSpec{Enabled: false},
				Route: ArgoCDRouteSpec{
					Enabled: false,
					TLS: &routev1.TLSConfig{
						InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
						Termination:                   routev1.TLSTerminationEdge,
					},
				},
			},
			RBAC: ArgoCDRBACSpec{
				DefaultPolicy: utils.StrPrt("role:admin"),
				Policy:        utils.StrPrt(`g, system:cluster-admins, role:admin`),
				Scopes:        utils.StrPrt("[groups]"),
			},
			Redis: ArgoCDRedisSpec{
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("256Mi"),
						corev1.ResourceCPU:    resource.MustParse("500m"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
				},
			},
			Repo: ArgoCDRepoSpec{
				Env: []corev1.EnvVar{
					{Name: "KUSTOMIZE_PLUGIN_HOME", Value: "/etc/kustomize/plugin"},
				},
				InitContainers: []corev1.Container{
					{
						Args: []string{
							"-c",
							"cp /etc/kustomize/plugin/policy.open-cluster-management.io/v1/policygenerator/PolicyGenerator /policy-generator/PolicyGenerator",
						},
						Command: []string{"/bin/bash"},
						Image:   "registry.redhat.io/rhacm2/multicluster-operators-subscription-rhel8:v2.7",
						Name:    "policy-generator-install",
						VolumeMounts: []corev1.VolumeMount{
							{MountPath: "/policy-generator", Name: "policy-generator"},
						},
					},
				},
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("1Gi"),
						corev1.ResourceCPU:    resource.MustParse("1000m"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("256Mi"),
						corev1.ResourceCPU:    resource.MustParse("250m"),
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						MountPath: "/etc/kustomize/plugin/policy.open-cluster-management.io/v1/policygenerator",
						Name:      "policy-generator",
					},
				},
				Volumes: []corev1.Volume{
					{Name: "policy-generator"},
				},
			},
			ResourceExclusions: string(resources),
			Server: ArgoCDServerSpec{
				Autoscale: ArgoCDServerAutoscaleSpec{Enabled: false},
				GRPC: ArgoCDServerGRPCSpec{
					Ingress: ArgoCDIngressSpec{Enabled: false},
				},
				Ingress:  ArgoCDIngressSpec{Enabled: false},
				Insecure: true,
				Replicas: utils.Int32(1),
				Resources: &corev1.ResourceRequirements{
					Limits: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("256Mi"),
						corev1.ResourceCPU:    resource.MustParse("500m"),
					},
					Requests: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceMemory: resource.MustParse("128Mi"),
						corev1.ResourceCPU:    resource.MustParse("125m"),
					},
				},
				Route: ArgoCDRouteSpec{
					Enabled: true,
					TLS: &routev1.TLSConfig{
						InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
						Termination:                   routev1.TLSTerminationEdge,
					},
				},
				Service: ArgoCDServerServiceSpec{Type: ""},
			},
			SSO: &ArgoCDSSOSpec{
				Dex: &ArgoCDDexSpec{
					OpenShiftOAuth: true,
					Resources: &corev1.ResourceRequirements{
						Limits: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
							corev1.ResourceCPU:    resource.MustParse("500m"),
						},
						Requests: map[corev1.ResourceName]resource.Quantity{
							corev1.ResourceMemory: resource.MustParse("128Mi"),
							corev1.ResourceCPU:    resource.MustParse("250m"),
						},
					},
				},
				Provider: SSOProviderTypeDex,
			},
			TLS: ArgoCDTLSSpec{
				CA: ArgoCDCASpec{},
			},
		},
	}

	// Convert the typed ArgoCD struct into an unstructured map
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&argoDeployment)
	if err != nil {
		return fmt.Errorf("failed to convert ArgoCD struct to unstructured: %w", err)
	}

	u := &unstructured.Unstructured{Object: unstructuredMap}

	// Create the ArgoCD resource in Kubernetes cluster
	if err := kclient.Create(context.TODO(), u); err != nil {
		return fmt.Errorf("failed to create ArgoCD resource: %w", err)
	}

	return nil
}

func waitForDeployment(kclient client.Client) error {
	timeout := time.After(600 * time.Second)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return errors.New("deployment took too long to become ready")

		case <-ticker.C:
			u, err := getDeployment(kclient, name, "openshift-gitops")
			if err != nil {
				argoCDLog.Error(err, "Failed to get ArgoCD Deployment")
				continue // Don't fail immediately; keep retrying
			}

			argoCDStatus := u.Object["status"]

			if statuses, ok := argoCDStatus.(map[string]any); ok {
				for key, value := range statuses {
					if key != "server" {
						continue
					}

					if val, ok := value.(string); ok {
						if val == "Running" {
							argoCDLog.Info("ArgoCD instance is running.")
							return nil
						}
					}
				}
			}

			argoCDLog.Info("Waiting for ArgoCD Deployment to be ready...")
		}
	}
}
