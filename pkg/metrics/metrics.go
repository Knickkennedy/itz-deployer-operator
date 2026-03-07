package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// DeploymentDuration tracks how long a full deployment takes from creation
	// to terminal state (Succeeded or Failed).
	DeploymentDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "itz",
			Subsystem: "deployer",
			Name:      "deployment_duration_seconds",
			Help:      "Time in seconds from Deployment CR creation to terminal state.",
			Buckets:   []float64{60, 300, 600, 900, 1800, 3600, 7200},
		},
		[]string{"repo_name", "release", "phase"},
	)

	// DeploymentPhaseTransitions counts how many times each phase transition
	// has occurred. Useful in aggregate across a fleet.
	DeploymentPhaseTransitions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "itz",
			Subsystem: "deployer",
			Name:      "phase_transitions_total",
			Help:      "Total number of Deployment phase transitions by phase.",
		},
		[]string{"repo_name", "phase"},
	)

	// PeakRequestedCPURatio is the highest observed ratio of total pod CPU
	// requests to total node allocatable CPU across the cluster lifetime.
	PeakRequestedCPURatio = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "itz",
			Subsystem: "cluster",
			Name:      "peak_requested_cpu_ratio",
			Help:      "Highest observed ratio of pod CPU requests to node allocatable CPU.",
		},
	)

	// PeakRequestedMemoryRatio is the highest observed ratio of total pod memory
	// requests to total node allocatable memory across the cluster lifetime.
	PeakRequestedMemoryRatio = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "itz",
			Subsystem: "cluster",
			Name:      "peak_requested_memory_ratio",
			Help:      "Highest observed ratio of pod memory requests to node allocatable memory.",
		},
	)

	// CurrentRequestedCPURatio is the current ratio of pod CPU requests to
	// node allocatable CPU. Useful for real-time dashboards.
	CurrentRequestedCPURatio = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "itz",
			Subsystem: "cluster",
			Name:      "current_requested_cpu_ratio",
			Help:      "Current ratio of pod CPU requests to node allocatable CPU.",
		},
	)

	// CurrentRequestedMemoryRatio is the current ratio of pod memory requests
	// to node allocatable memory.
	CurrentRequestedMemoryRatio = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "itz",
			Subsystem: "cluster",
			Name:      "current_requested_memory_ratio",
			Help:      "Current ratio of pod memory requests to node allocatable memory.",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		DeploymentDuration,
		DeploymentPhaseTransitions,
		PeakRequestedCPURatio,
		PeakRequestedMemoryRatio,
		CurrentRequestedCPURatio,
		CurrentRequestedMemoryRatio,
	)
}
