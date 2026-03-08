/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"knative.dev/pkg/apis"

	console "github.com/openshift/api/console/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	techzonev1alpha1 "github.ibm.com/itz-content/itz-deployer-operator/api/v1alpha1"
	"github.ibm.com/itz-content/itz-deployer-operator/pkg/config"
)

// ============================================================================
// Fakes
// ============================================================================

type fakeConfigLoader struct {
	cfg config.OperatorConfig
	err error
}

func (f fakeConfigLoader) Load(_ context.Context, _ client.Client) (config.OperatorConfig, error) {
	return f.cfg, f.err
}

type fakeCredentialProvider struct {
	pat string
	err error
}

func (f fakeCredentialProvider) GetGitHubPAT(_ context.Context, _ config.OperatorConfig) (string, error) {
	return f.pat, f.err
}

// fakeRepoCloner writes minimal Tekton YAML to a temp dir so reconcileTektonResources
// has real files to parse without touching GitHub.
type fakeRepoCloner struct {
	err error
}

func (f fakeRepoCloner) Clone(_ context.Context, _, _, _ string) (string, func(), error) {
	if f.err != nil {
		return "", func() {}, f.err
	}

	dir, err := os.MkdirTemp("", "fake-repo-*")
	if err != nil {
		return "", func() {}, err
	}

	pipeline := `apiVersion: tekton.dev/v1
kind: Pipeline
metadata:
  name: fake-pipeline
spec:
  tasks: []
`
	pipelineRun := `apiVersion: tekton.dev/v1
kind: PipelineRun
metadata:
  name: fake-pipelinerun
spec:
  pipelineRef:
    name: fake-pipeline
`
	if err := os.WriteFile(filepath.Join(dir, pipelineFileName), []byte(pipeline), 0600); err != nil {
		return "", func() {}, err
	}
	if err := os.WriteFile(filepath.Join(dir, pipelineRunFileName), []byte(pipelineRun), 0600); err != nil {
		return "", func() {}, err
	}

	return dir, func() { os.RemoveAll(dir) }, nil
}

type fakeRouteResolver struct {
	url string
	err error
}

func (f fakeRouteResolver) GetExternalClusterBaseURL(_ context.Context) (string, error) {
	return f.url, f.err
}

// ============================================================================
// Helpers
// ============================================================================

func newTestReconciler(c client.Client) *DeploymentReconciler {
	return &DeploymentReconciler{
		Client: c,
		Scheme: c.Scheme(),
		Loader: fakeConfigLoader{cfg: config.OperatorConfig{
			SecretsManagerURL:      "https://fake-sm.example.com",
			SecretsManagerSecretID: "fake-secret-id",
		}},
		Creds:    fakeCredentialProvider{pat: "fake-pat"},
		Cloner:   fakeRepoCloner{},
		Resolver: fakeRouteResolver{url: "https://console.example.com"},
	}
}

func reconcileDeployment(ctx context.Context, r *DeploymentReconciler, name, namespace string) (reconcile.Result, error) {
	return r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
}

func getCondition(deployment *techzonev1alpha1.Deployment, condType string) *metav1.Condition {
	for i := range deployment.Status.Conditions {
		if deployment.Status.Conditions[i].Type == condType {
			return &deployment.Status.Conditions[i]
		}
	}
	return nil
}

func makeKnativeCond(status corev1.ConditionStatus, message string) *apis.Condition {
	return &apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  status,
		Message: message,
	}
}

// ============================================================================
// Tests
// ============================================================================

var _ = Describe("Deployment Controller", func() {
	const namespace = "default"
	ctx := context.Background()

	// -------------------------------------------------------------------------
	// Finalizer registration
	// -------------------------------------------------------------------------
	Context("When a new Deployment CR is created", func() {
		const name = "test-finalizer"

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		})

		AfterEach(func() {
			dep := &techzonev1alpha1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("registers the cleanup finalizer on first reconcile", func() {
			r := newTestReconciler(k8sClient)
			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())

			dep := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())
			Expect(dep.Finalizers).To(ContainElement(notificationFinalizer))
		})
	})

	// -------------------------------------------------------------------------
	// Status conditions — Succeeded
	// -------------------------------------------------------------------------
	Context("When a PipelineRun completes successfully", func() {
		const name = "test-succeeded"

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		})

		AfterEach(func() {
			dep := &techzonev1alpha1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("sets Ready=True, Progressing=False, Degraded=False and clears diagnostics", func() {
			r := newTestReconciler(k8sClient)

			dep := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())

			Expect(r.applyCondition(ctx, dep, makeKnativeCond(corev1.ConditionTrue, "done"), nil, nil)).To(Succeed())

			updated := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal("Succeeded"))
			Expect(updated.Status.Diagnostics).To(BeNil())
			Expect(updated.Status.FailureCategory).To(BeEmpty())

			Expect(getCondition(updated, ConditionReady)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "Succeeded"),
			))
			Expect(getCondition(updated, ConditionProgressing)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionFalse),
			))
			Expect(getCondition(updated, ConditionDegraded)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionFalse),
			))
		})
	})

	// -------------------------------------------------------------------------
	// Status conditions — Failed
	// -------------------------------------------------------------------------
	Context("When a PipelineRun fails", func() {
		const name = "test-failed"
		const failureMsg = "pipeline step 'build' exited with code 1"

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		})

		AfterEach(func() {
			dep := &techzonev1alpha1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("sets Ready=False, Progressing=False, Degraded=True with the failure message", func() {
			r := newTestReconciler(k8sClient)

			dep := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())

			// Pass nil PipelineRun and Job — no real resources to walk in unit tests.
			Expect(r.applyCondition(ctx, dep, makeKnativeCond(corev1.ConditionFalse, failureMsg), nil, nil)).To(Succeed())

			updated := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal("Failed"))
			Expect(updated.Status.Message).To(Equal(failureMsg))

			Expect(getCondition(updated, ConditionReady)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Message", failureMsg),
			))
			Expect(getCondition(updated, ConditionProgressing)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionFalse),
			))
			Expect(getCondition(updated, ConditionDegraded)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Message", failureMsg),
			))
		})
	})

	// -------------------------------------------------------------------------
	// Status conditions — Running
	// -------------------------------------------------------------------------
	Context("When a PipelineRun is in progress", func() {
		const name = "test-running"

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		})

		AfterEach(func() {
			dep := &techzonev1alpha1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("sets Ready=False, Progressing=True, Degraded=False", func() {
			r := newTestReconciler(k8sClient)

			dep := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())

			Expect(r.applyCondition(ctx, dep, makeKnativeCond(corev1.ConditionUnknown, "Running pipeline steps."), nil, nil)).To(Succeed())

			updated := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal("Running"))

			Expect(getCondition(updated, ConditionReady)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "Progressing"),
			))
			Expect(getCondition(updated, ConditionProgressing)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionTrue),
			))
			Expect(getCondition(updated, ConditionDegraded)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionFalse),
			))
		})
	})

	// -------------------------------------------------------------------------
	// ConsoleNotification cleanup on deletion
	// -------------------------------------------------------------------------
	Context("When a Deployment CR is deleted", func() {
		const name = "test-deletion"
		notifName := fmt.Sprintf("%s-status-banner", name)

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			r := newTestReconciler(k8sClient)
			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())

			notification := &console.ConsoleNotification{
				ObjectMeta: metav1.ObjectMeta{Name: notifName},
				Spec: console.ConsoleNotificationSpec{
					Text:     "CR 'test-deletion' is starting...",
					Location: console.BannerTop,
				},
			}
			Expect(k8sClient.Create(ctx, notification)).To(Succeed())
		})

		It("deletes the ConsoleNotification and removes the finalizer", func() {
			r := newTestReconciler(k8sClient)

			dep := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())

			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())

			notif := &console.ConsoleNotification{}
			err = k8sClient.Get(ctx, client.ObjectKey{Name: notifName}, notif)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "ConsoleNotification should have been deleted")

			dep = &techzonev1alpha1.Deployment{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)
			Expect(errors.IsNotFound(err)).To(BeTrue(), "Deployment CR should be fully deleted")
		})
	})

	// -------------------------------------------------------------------------
	// Post-deployment hook
	// -------------------------------------------------------------------------
	Context("When a Deployment succeeds with a PostDeployment spec", func() {
		const name = "test-post-deploy"
		const postRepo = "post-deploy-repo"
		const postRelease = "v2.0.0"
		childName := fmt.Sprintf("%s-post-deploy", name)

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: techzonev1alpha1.DeploymentSpec{
					RepoName: "some-repo",
					Release:  "v1.0.0",
					PostDeployment: techzonev1alpha1.PostDeployment{
						RepoName: postRepo,
						Release:  postRelease,
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			r := newTestReconciler(k8sClient)
			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())

			dep = &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())
			dep.Status.Phase = "Succeeded"
			dep.Status.Message = "Resource completed successfully."
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		})

		AfterEach(func() {
			for _, n := range []string{name, childName} {
				dep := &techzonev1alpha1.Deployment{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: n, Namespace: namespace}, dep); err == nil {
					Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
				}
			}
		})

		It("creates a child Deployment CR with the PostDeployment spec", func() {
			r := newTestReconciler(k8sClient)
			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())

			child := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: childName, Namespace: namespace}, child)).To(Succeed())
			Expect(child.Spec.RepoName).To(Equal(postRepo))
			Expect(child.Spec.Release).To(Equal(postRelease))

			parent := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, parent)).To(Succeed())
			Expect(parent.Status.Message).To(Equal(postDeploymentMessage))
		})

		It("does not create a duplicate child CR on subsequent reconciles", func() {
			r := newTestReconciler(k8sClient)

			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())

			list := &techzonev1alpha1.DeploymentList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())

			count := 0
			for _, item := range list.Items {
				if item.Name == childName {
					count++
				}
			}
			Expect(count).To(Equal(1), "only one child CR should exist")
		})
	})

	// -------------------------------------------------------------------------
	// Clone failure
	// -------------------------------------------------------------------------
	Context("When the repo cannot be cloned", func() {
		const name = "test-clone-fail"

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			r := newTestReconciler(k8sClient)
			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			dep := &techzonev1alpha1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("marks the Deployment as Failed with Degraded=True, CloneError category, and a diagnostic entry", func() {
			r := newTestReconciler(k8sClient)
			r.Cloner = fakeRepoCloner{err: fmt.Errorf("authentication failed")}

			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).To(HaveOccurred())

			updated := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal("Failed"))
			Expect(updated.Status.Message).To(ContainSubstring("authentication failed"))
			Expect(updated.Status.FailureCategory).To(Equal(techzonev1alpha1.FailureCategoryClone))

			Expect(updated.Status.Diagnostics).To(HaveLen(1))
			Expect(updated.Status.Diagnostics[0].Component).To(Equal("Operator"))
			Expect(updated.Status.Diagnostics[0].Message).To(ContainSubstring("authentication failed"))
			Expect(updated.Status.Diagnostics[0].RemediationHint).NotTo(BeEmpty())

			Expect(getCondition(updated, ConditionDegraded)).To(And(
				Not(BeNil()),
				HaveField("Status", metav1.ConditionTrue),
			))
		})
	})

	// -------------------------------------------------------------------------
	// Credential failure
	// -------------------------------------------------------------------------
	Context("When credentials cannot be retrieved", func() {
		const name = "test-cred-fail"

		BeforeEach(func() {
			dep := &techzonev1alpha1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       techzonev1alpha1.DeploymentSpec{RepoName: "some-repo", Release: "v1.0.0"},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			r := newTestReconciler(k8sClient)
			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			dep := &techzonev1alpha1.Deployment{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("marks the Deployment as Failed with CredentialError category and a remediation hint", func() {
			r := newTestReconciler(k8sClient)
			r.Creds = fakeCredentialProvider{err: fmt.Errorf("secrets manager unreachable")}

			_, err := reconcileDeployment(ctx, r, name, namespace)
			Expect(err).To(HaveOccurred())

			updated := &techzonev1alpha1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, updated)).To(Succeed())

			Expect(updated.Status.Phase).To(Equal("Failed"))
			Expect(updated.Status.FailureCategory).To(Equal(techzonev1alpha1.FailureCategoryCredential))

			Expect(updated.Status.Diagnostics).To(HaveLen(1))
			Expect(updated.Status.Diagnostics[0].Component).To(Equal("Operator"))
			Expect(updated.Status.Diagnostics[0].RemediationHint).To(ContainSubstring("SecretsManagerURL"))
		})
	})
})
