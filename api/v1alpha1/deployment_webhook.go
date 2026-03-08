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

package v1alpha1

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// invalidGitTagChars are characters that are illegal in git tag names per
// git-check-ref-format(1). A release field containing any of these will
// always cause a clone failure, so we reject early.
const invalidGitTagChars = " \t~^:?*[\\"

// SetupWebhookWithManager registers the validating webhook with the manager.
func (d *Deployment) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(d).
		WithValidator(&DeploymentValidator{}).
		Complete()
}

// DeploymentValidator implements webhook.CustomValidator.
type DeploymentValidator struct{}

var _ webhook.CustomValidator = &DeploymentValidator{}

// ValidateCreate validates a new Deployment CR on creation.
func (v *DeploymentValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	d, ok := obj.(*Deployment)
	if !ok {
		return nil, fmt.Errorf("expected a Deployment but got %T", obj)
	}
	return validateDeploymentSpec(d, nil)
}

// ValidateUpdate validates changes to an existing Deployment CR.
func (v *DeploymentValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldD, ok := oldObj.(*Deployment)
	if !ok {
		return nil, fmt.Errorf("expected a Deployment but got %T", oldObj)
	}
	newD, ok := newObj.(*Deployment)
	if !ok {
		return nil, fmt.Errorf("expected a Deployment but got %T", newObj)
	}
	return validateDeploymentSpec(newD, oldD)
}

// ValidateDelete is a no-op — we have no deletion constraints beyond the
// finalizer the controller already manages.
func (v *DeploymentValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// ============================================================================
// Core validation logic
// ============================================================================

// validateDeploymentSpec runs all validation rules. oldD is nil on create.
func validateDeploymentSpec(d *Deployment, oldD *Deployment) (admission.Warnings, error) {
	var allErrs field.ErrorList
	var warnings admission.Warnings

	specPath := field.NewPath("spec")

	// --- Required fields ---
	if strings.TrimSpace(d.Spec.RepoName) == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("repoName"),
			"repoName is required and must not be empty",
		))
	}

	if strings.TrimSpace(d.Spec.Release) == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("release"),
			"release is required and must not be empty",
		))
	} else if err := validateGitTag(d.Spec.Release, specPath.Child("release")); err != nil {
		allErrs = append(allErrs, err)
	}

	// --- PostDeployment consistency ---
	// postDeployment.repoName without postDeployment.release is always a
	// misconfiguration — the reconciler will create a broken child CR.
	if strings.TrimSpace(d.Spec.PostDeployment.RepoName) != "" &&
		strings.TrimSpace(d.Spec.PostDeployment.Release) == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("postDeployment", "release"),
			"postDeployment.release is required when postDeployment.repoName is set",
		))
	}
	// Inverse: release without repoName is equally broken.
	if strings.TrimSpace(d.Spec.PostDeployment.Release) != "" &&
		strings.TrimSpace(d.Spec.PostDeployment.RepoName) == "" {
		allErrs = append(allErrs, field.Required(
			specPath.Child("postDeployment", "repoName"),
			"postDeployment.repoName is required when postDeployment.release is set",
		))
	}
	// Validate the post-deployment release tag format if provided.
	if strings.TrimSpace(d.Spec.PostDeployment.Release) != "" {
		if err := validateGitTag(d.Spec.PostDeployment.Release, specPath.Child("postDeployment", "release")); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	// --- Immutability (updates only) ---
	if oldD != nil {
		if d.Spec.RepoName != oldD.Spec.RepoName {
			allErrs = append(allErrs, field.Forbidden(
				specPath.Child("repoName"),
				fmt.Sprintf("repoName is immutable after creation (was %q, got %q)", oldD.Spec.RepoName, d.Spec.RepoName),
			))
		}
		if d.Spec.Release != oldD.Spec.Release {
			allErrs = append(allErrs, field.Forbidden(
				specPath.Child("release"),
				fmt.Sprintf("release is immutable after creation (was %q, got %q)", oldD.Spec.Release, d.Spec.Release),
			))
		}
	}

	// --- Advisory warnings (non-blocking) ---
	if strings.HasPrefix(d.Spec.Parameters, "/") {
		warnings = append(warnings,
			"spec.parameters looks like an absolute path — it should be relative to the repository root",
		)
	}

	if len(allErrs) > 0 {
		return warnings, allErrs.ToAggregate()
	}
	return warnings, nil
}

// validateGitTag returns a field.Error if the value contains characters that
// are illegal in git tag names per git-check-ref-format(1).
func validateGitTag(tag string, path *field.Path) *field.Error {
	for _, ch := range invalidGitTagChars {
		if strings.ContainsRune(tag, ch) {
			return field.Invalid(
				path,
				tag,
				fmt.Sprintf("release contains invalid git tag character %q — see git-check-ref-format(1)", string(ch)),
			)
		}
	}
	if strings.HasPrefix(tag, ".") || strings.HasPrefix(tag, "-") {
		return field.Invalid(path, tag, "release must not begin with '.' or '-'")
	}
	if strings.HasSuffix(tag, ".lock") {
		return field.Invalid(path, tag, "release must not end with '.lock'")
	}
	if strings.Contains(tag, "..") {
		return field.Invalid(path, tag, "release must not contain '..'")
	}
	if strings.Contains(tag, "@{") {
		return field.Invalid(path, tag, "release must not contain '@{'")
	}
	return nil
}
