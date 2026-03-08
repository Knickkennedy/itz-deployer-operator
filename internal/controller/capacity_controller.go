package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	itzmetrics "github.ibm.com/itz-content/itz-deployer-operator/pkg/metrics"
)

const (
	capacityConfigMapName = "itz-cluster-capacity-peak"
	capacityPollInterval  = 5 * time.Minute
)

// CapacityPeak holds the highest utilization ratios ever observed.
// Stored as JSON in a ConfigMap so it survives operator restarts.
type CapacityPeak struct {
	CPURatio    float64 `json:"cpuRatio"`
	MemoryRatio float64 `json:"memoryRatio"`
}

// CapacityReconciler runs on a schedule and tracks the cluster's peak
// resource utilization high watermark.
type CapacityReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Namespace string
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update;patch

func (r *CapacityReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithName("CapacityReconciler")

	cpuRatio, memRatio, err := r.computeUtilization(ctx)
	if err != nil {
		logger.Error(err, "Failed to compute cluster utilization")
		return ctrl.Result{RequeueAfter: capacityPollInterval}, nil
	}

	logger.V(1).Info("Cluster utilization snapshot",
		"cpu_ratio", fmt.Sprintf("%.1f%%", cpuRatio*100),
		"memory_ratio", fmt.Sprintf("%.1f%%", memRatio*100),
	)

	itzmetrics.CurrentRequestedCPURatio.Set(cpuRatio)
	itzmetrics.CurrentRequestedMemoryRatio.Set(memRatio)

	peak, err := r.loadPeak(ctx)
	if err != nil {
		logger.Error(err, "Failed to load capacity peak ConfigMap")
		return ctrl.Result{RequeueAfter: capacityPollInterval}, nil
	}

	updated := false
	if cpuRatio > peak.CPURatio {
		logger.V(1).Info("New CPU peak recorded",
			"previous", fmt.Sprintf("%.1f%%", peak.CPURatio*100),
			"new", fmt.Sprintf("%.1f%%", cpuRatio*100),
		)
		peak.CPURatio = cpuRatio
		updated = true
	}
	if memRatio > peak.MemoryRatio {
		logger.V(1).Info("New memory peak recorded",
			"previous", fmt.Sprintf("%.1f%%", peak.MemoryRatio*100),
			"new", fmt.Sprintf("%.1f%%", memRatio*100),
		)
		peak.MemoryRatio = memRatio
		updated = true
	}

	if updated {
		if err := r.savePeak(ctx, peak); err != nil {
			logger.Error(err, "Failed to persist capacity peak")
			return ctrl.Result{RequeueAfter: capacityPollInterval}, nil
		}
	}

	itzmetrics.PeakRequestedCPURatio.Set(peak.CPURatio)
	itzmetrics.PeakRequestedMemoryRatio.Set(peak.MemoryRatio)

	return ctrl.Result{RequeueAfter: capacityPollInterval}, nil
}

// computeUtilization calculates cluster-wide resource request ratios.
//
// Effective pod request follows the Kubernetes scheduler formula:
//
//	effective = max(sum(container requests), max(init container requests))
//
// This matches what the scheduler actually reserves when placing pods.
func (r *CapacityReconciler) computeUtilization(ctx context.Context) (cpuRatio, memRatio float64, err error) {
	// --- Node allocatable capacity ---
	nodeList := &corev1.NodeList{}
	if err := r.APIReader.List(ctx, nodeList); err != nil {
		return 0, 0, fmt.Errorf("failed to list nodes: %w", err)
	}

	var totalCPU, totalMemory resource.Quantity
	for _, node := range nodeList.Items {
		if isMasterNode(node) {
			continue
		}
		if cpu, ok := node.Status.Allocatable[corev1.ResourceCPU]; ok {
			totalCPU.Add(cpu)
		}
		if mem, ok := node.Status.Allocatable[corev1.ResourceMemory]; ok {
			totalMemory.Add(mem)
		}
	}

	if totalCPU.IsZero() || totalMemory.IsZero() {
		return 0, 0, fmt.Errorf("no worker node capacity found")
	}

	// --- Pod requests ---
	// NOTE: status.phase is NOT a supported server-side field selector in most
	// Kubernetes versions — it is evaluated client-side only. We fetch all pods
	// and filter manually to avoid silent full-list fallback behaviour, but we
	// limit memory pressure by only retaining the fields we need (requests) and
	// discarding the rest immediately in the loop.
	podList := &corev1.PodList{}
	if err := r.APIReader.List(ctx, podList); err != nil {
		return 0, 0, fmt.Errorf("failed to list pods: %w", err)
	}

	var requestedCPU, requestedMemory resource.Quantity
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		podCPU, podMem := effectivePodRequests(pod)
		requestedCPU.Add(podCPU)
		requestedMemory.Add(podMem)

		// Nil out the pod spec immediately after processing to allow GC to
		// reclaim memory before the next iteration on large clusters.
		pod.Spec = corev1.PodSpec{}
		pod.Status = corev1.PodStatus{}
	}

	cpuRatio = float64(requestedCPU.MilliValue()) / float64(totalCPU.MilliValue())
	memRatio = float64(requestedMemory.Value()) / float64(totalMemory.Value())

	return cpuRatio, memRatio, nil
}

// effectivePodRequests returns the effective CPU and memory requests for a pod
// using the Kubernetes scheduler formula:
//
//	effective = max(sum(container requests), max(init container requests))
func effectivePodRequests(pod *corev1.Pod) (cpu, memory resource.Quantity) {
	// Sum all regular container requests.
	var containerCPU, containerMemory resource.Quantity
	for _, c := range pod.Spec.Containers {
		if v := c.Resources.Requests.Cpu(); v != nil {
			containerCPU.Add(*v)
		}
		if v := c.Resources.Requests.Memory(); v != nil {
			containerMemory.Add(*v)
		}
	}

	// Find the maximum single init container request.
	var maxInitCPU, maxInitMemory resource.Quantity
	for _, c := range pod.Spec.InitContainers {
		if v := c.Resources.Requests.Cpu(); v != nil && v.Cmp(maxInitCPU) > 0 {
			maxInitCPU = v.DeepCopy()
		}
		if v := c.Resources.Requests.Memory(); v != nil && v.Cmp(maxInitMemory) > 0 {
			maxInitMemory = v.DeepCopy()
		}
	}

	// effective = max(containers, max(init containers))
	if maxInitCPU.Cmp(containerCPU) > 0 {
		cpu = maxInitCPU
	} else {
		cpu = containerCPU
	}
	if maxInitMemory.Cmp(containerMemory) > 0 {
		memory = maxInitMemory
	} else {
		memory = containerMemory
	}
	return cpu, memory
}

// isMasterNode returns true if the node carries any control-plane taint or label.
func isMasterNode(node corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/master" ||
			taint.Key == "node-role.kubernetes.io/control-plane" {
			return true
		}
	}
	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		return true
	}
	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}
	return false
}

// loadPeak reads the persisted CapacityPeak from the operator ConfigMap.
// Returns a zero-value peak if the ConfigMap doesn't exist yet.
func (r *CapacityReconciler) loadPeak(ctx context.Context) (CapacityPeak, error) {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      capacityConfigMapName,
		Namespace: r.Namespace,
	}, cm)

	if k8serrors.IsNotFound(err) {
		return CapacityPeak{}, nil
	}
	if err != nil {
		return CapacityPeak{}, fmt.Errorf("failed to get peak ConfigMap: %w", err)
	}

	raw, ok := cm.Data["peak"]
	if !ok {
		return CapacityPeak{}, nil
	}

	var peak CapacityPeak
	if err := json.Unmarshal([]byte(raw), &peak); err != nil {
		return CapacityPeak{}, fmt.Errorf("failed to unmarshal peak data: %w", err)
	}
	return peak, nil
}

// savePeak persists the updated CapacityPeak to the operator ConfigMap,
// creating it if it doesn't exist.
func (r *CapacityReconciler) savePeak(ctx context.Context, peak CapacityPeak) error {
	raw, err := json.Marshal(peak)
	if err != nil {
		return fmt.Errorf("failed to marshal peak data: %w", err)
	}

	cm := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      capacityConfigMapName,
		Namespace: r.Namespace,
	}, cm)

	if k8serrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      capacityConfigMapName,
				Namespace: r.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "itz-deployer-operator",
				},
			},
			Data: map[string]string{"peak": string(raw)},
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("failed to get peak ConfigMap for update: %w", err)
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data["peak"] = string(raw)
	return r.Update(ctx, cm)
}

// OperatorNamespace returns the namespace the operator is running in,
// read from the serviceaccount file injected by Kubernetes.
func OperatorNamespace() (string, error) {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to read operator namespace: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// SetupWithManager registers the CapacityReconciler. The controller watches
// the operator's own Deployment to trigger the first reconcile at startup.
// Subsequent runs are driven purely by the RequeueAfter in Reconcile.
// GenerationChangedPredicate is intentionally NOT used — we want the initial
// cache sync event (generation unchanged) to fire so the loop starts.
func (r *CapacityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("capacity").
		For(&appsv1.Deployment{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetName() == "itz-deployer-operator-controller-manager" &&
				obj.GetNamespace() == r.Namespace
		}))).
		Complete(r)
}
