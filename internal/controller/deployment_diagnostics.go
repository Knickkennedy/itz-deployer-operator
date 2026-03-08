package controller

import (
	"context"
	"fmt"
	"io"
	"strings"

	tektonv1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	techzonev1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
)

const (
	// logTailLines is how many lines of container log we capture at failure time.
	// Enough to see the error without storing megabytes in etcd.
	logTailLines = int64(50)
)

// diagnosticsCollector fetches structured failure information from Tekton or
// Ansible resources and writes it into the Deployment status. It requires a
// rest.Config to build a typed clientset for the pod logs API, which
// controller-runtime's client does not expose.
type diagnosticsCollector struct {
	c          client.Client
	restConfig *rest.Config
}

// collectTektonDiagnostics populates diagnostics from a failed PipelineRun.
// It walks childReferences to find failed TaskRuns, extracts step-level exit
// codes and reasons, and fetches the log tail of each failed step's pod.
func (d *diagnosticsCollector) collectTektonDiagnostics(
	ctx context.Context,
	deployment *techzonev1alpha1.Deployment,
	pr *tektonv1.PipelineRun,
) {
	logger := logf.FromContext(ctx).WithName("diagnostics")

	clientset, err := kubernetes.NewForConfig(d.restConfig)
	if err != nil {
		logger.Error(err, "Failed to build clientset for log fetching")
		deployment.Status.Diagnostics = append(deployment.Status.Diagnostics, techzonev1alpha1.DiagnosticEntry{
			Component: "Operator",
			Message:   fmt.Sprintf("Failed to initialise log client: %v", err),
		})
		return
	}

	if pr == nil {
		return
	}

	// Walk each child TaskRun reference.
	for _, ref := range pr.Status.ChildReferences {
		tr := &tektonv1.TaskRun{}
		if err := d.c.Get(ctx, types.NamespacedName{
			Name:      ref.Name,
			Namespace: pr.Namespace,
		}, tr); err != nil {
			logger.Error(err, "Failed to get TaskRun", "name", ref.Name)
			continue
		}

		// Only collect diagnostics for failed TaskRuns.
		succeeded := tr.Status.GetCondition("Succeeded")
		if succeeded == nil || succeeded.Status != "False" {
			continue
		}

		// Walk steps within the TaskRun.
		for _, step := range tr.Status.Steps {
			if step.Terminated == nil || step.Terminated.ExitCode == 0 {
				continue
			}

			exitCode := step.Terminated.ExitCode
			entry := techzonev1alpha1.DiagnosticEntry{
				Component: "Tekton",
				TaskName:  ref.DisplayName,
				StepName:  step.Name,
				ExitCode:  &exitCode,
				Reason:    step.Terminated.Reason,
				Message:   step.Terminated.Message,
				RemediationHint: remediationHintForStep(
					step.Name,
					step.Terminated.Reason,
					step.Terminated.ExitCode,
				),
			}

			// Fetch log tail from the TaskRun's pod.
			if tr.Status.PodName != "" {
				entry.LogTail = fetchLogTail(
					ctx, clientset,
					pr.Namespace, tr.Status.PodName,
					"step-"+step.Name,
				)
			}

			deployment.Status.Diagnostics = append(deployment.Status.Diagnostics, entry)
		}

		// If no step-level failures were found but the TaskRun itself failed,
		// capture the TaskRun-level message as a fallback.
		if len(deployment.Status.Diagnostics) == 0 {
			deployment.Status.Diagnostics = append(deployment.Status.Diagnostics, techzonev1alpha1.DiagnosticEntry{
				Component: "Tekton",
				TaskName:  ref.DisplayName,
				Message:   succeeded.Message,
				RemediationHint: "Check the TaskRun logs for more details: " +
					fmt.Sprintf("oc logs -n %s %s --all-containers", pr.Namespace, tr.Status.PodName),
			})
		}
	}

	deployment.Status.FailureCategory = techzonev1alpha1.FailureCategoryStepFailed
}

// collectAnsibleDiagnostics populates diagnostics from a failed Kubernetes Job.
func (d *diagnosticsCollector) collectAnsibleDiagnostics(
	ctx context.Context,
	deployment *techzonev1alpha1.Deployment,
	job *batchv1.Job,
) {
	logger := logf.FromContext(ctx).WithName("diagnostics")

	if job == nil {
		return
	}

	clientset, err := kubernetes.NewForConfig(d.restConfig)
	if err != nil {
		logger.Error(err, "Failed to build clientset for log fetching")
		return
	}

	// Find the failed Job condition message.
	var jobMessage, jobReason string
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed {
			jobMessage = cond.Message
			jobReason = cond.Reason
			break
		}
	}

	// Find the most recently failed pod for this job.
	podList := &corev1.PodList{}
	if err := d.c.List(ctx, podList,
		client.InNamespace(job.Namespace),
		client.MatchingLabels(job.Spec.Selector.MatchLabels),
	); err != nil {
		logger.Error(err, "Failed to list Job pods")
	}

	entry := techzonev1alpha1.DiagnosticEntry{
		Component:       "AnsibleJob",
		Reason:          jobReason,
		Message:         jobMessage,
		RemediationHint: remediationHintForAnsible(jobReason),
	}

	// Fetch log tail from the ansible-runner container of the most recent pod.
	latestPod := mostRecentFailedPod(podList)
	if latestPod != "" {
		entry.LogTail = fetchLogTail(
			ctx, clientset,
			job.Namespace, latestPod,
			"ansible-runner",
		)
	}

	deployment.Status.Diagnostics = append(deployment.Status.Diagnostics, entry)
	deployment.Status.FailureCategory = techzonev1alpha1.FailureCategoryJobFailed
}

// ============================================================================
// Helpers
// ============================================================================

// fetchLogTail retrieves the last logTailLines lines from a container's log.
// Returns an empty string on any error — diagnostics are best-effort.
func fetchLogTail(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace, podName, containerName string,
) string {
	logger := logf.FromContext(ctx).WithName("diagnostics")

	tailLines := logTailLines
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		logger.V(1).Info("Could not fetch log tail",
			"pod", podName, "container", containerName, "err", err)
		return ""
	}
	defer stream.Close()

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, stream); err != nil {
		return ""
	}
	return buf.String()
}

// mostRecentFailedPod returns the name of the most recently started failed pod
// from the list, or empty string if none found.
func mostRecentFailedPod(podList *corev1.PodList) string {
	var latest *corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodFailed {
			continue
		}
		if latest == nil || pod.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = pod
		}
	}
	if latest == nil {
		return ""
	}
	return latest.Name
}

// remediationHintForStep returns a human+AI readable remediation suggestion
// based on the step name and exit code. This is heuristic — extend as patterns
// emerge from real failures.
func remediationHintForStep(stepName, reason string, exitCode int32) string {
	// OOMKilled is always the same fix regardless of step.
	if reason == "OOMKilled" {
		return "The container was OOMKilled. Increase memory limits in the pipeline task definition."
	}

	stepLower := strings.ToLower(stepName)

	switch {
	case strings.Contains(stepLower, "push") || strings.Contains(stepLower, "image"):
		return "Image push failed. Verify registry credentials are present in the pipeline workspace secret and the destination repository exists."
	case strings.Contains(stepLower, "clone") || strings.Contains(stepLower, "git"):
		return "Git operation failed. Verify the repository URL, tag, and that the GitHub PAT has read access."
	case strings.Contains(stepLower, "test"):
		return "Test step failed (exit code " + fmt.Sprintf("%d", exitCode) + "). Review the test output in the log tail above."
	case strings.Contains(stepLower, "build") || strings.Contains(stepLower, "compile"):
		return "Build step failed. Review compilation errors in the log tail above."
	case strings.Contains(stepLower, "deploy") || strings.Contains(stepLower, "apply"):
		return "Deployment step failed. Verify cluster permissions and that target namespaces exist."
	default:
		return fmt.Sprintf("Step %q failed with exit code %d. Review the log tail above for the root cause.", stepName, exitCode)
	}
}

// remediationHintForAnsible returns a remediation suggestion for Ansible job
// failures based on the Job condition reason.
func remediationHintForAnsible(reason string) string {
	switch reason {
	case "DeadlineExceeded":
		return "The Ansible job exceeded its deadline. The playbook may be hanging on a task. Review the log tail for the last executed task."
	case "BackoffLimitExceeded":
		return "The Ansible job exhausted its retry limit. Review the log tail for the recurring failure."
	default:
		return "Review the ansible-runner container log tail above for the failing task and error message."
	}
}
