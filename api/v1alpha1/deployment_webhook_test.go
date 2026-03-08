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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newDeployment(repoName, release string) *Deployment {
	return &Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-deployment",
			Namespace: "default",
		},
		Spec: DeploymentSpec{
			RepoName: repoName,
			Release:  release,
		},
	}
}

func TestValidateCreate_Valid(t *testing.T) {
	d := newDeployment("cp4d", "v1.2.3")
	_, err := validateDeploymentSpec(d, nil)
	if err != nil {
		t.Errorf("expected valid deployment to pass, got: %v", err)
	}
}

func TestValidateCreate_MissingRepoName(t *testing.T) {
	d := newDeployment("", "v1.0.0")
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected error for missing repoName, got nil")
	}
}

func TestValidateCreate_MissingRelease(t *testing.T) {
	d := newDeployment("cp4d", "")
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected error for missing release, got nil")
	}
}

func TestValidateCreate_WhitespaceOnlyRepoName(t *testing.T) {
	d := newDeployment("   ", "v1.0.0")
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected error for whitespace-only repoName, got nil")
	}
}

func TestValidateCreate_InvalidReleaseChars(t *testing.T) {
	invalidTags := []string{
		"v1.0 0",
		"v1.0~0",
		"v1.0^0",
		"v1.0:0",
		"v1.0?0",
		"v1.0*0",
		"v1.0[0",
		"v1.0\\0",
		".v1.0.0",
		"-v1.0.0",
		"v1.0.lock",
		"v1..0",
		"v1@{0",
	}
	for _, tag := range invalidTags {
		t.Run(tag, func(t *testing.T) {
			d := newDeployment("cp4d", tag)
			_, err := validateDeploymentSpec(d, nil)
			if err == nil {
				t.Errorf("expected error for invalid release tag %q, got nil", tag)
			}
		})
	}
}

func TestValidateCreate_ValidReleaseTags(t *testing.T) {
	validTags := []string{
		"v1.0.0",
		"v1.0.0-rc1",
		"v1.0.0-beta.1",
		"release/v1.0.0",
		"2025-03-01",
		"1.0",
	}
	for _, tag := range validTags {
		t.Run(tag, func(t *testing.T) {
			d := newDeployment("cp4d", tag)
			_, err := validateDeploymentSpec(d, nil)
			if err != nil {
				t.Errorf("expected valid release tag %q to pass, got: %v", tag, err)
			}
		})
	}
}

func TestValidateCreate_PostDeployment_RepoWithoutRelease(t *testing.T) {
	d := newDeployment("cp4d", "v1.0.0")
	d.Spec.PostDeployment.RepoName = "post-repo"
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected error when postDeployment.repoName set without postDeployment.release")
	}
}

func TestValidateCreate_PostDeployment_ReleaseWithoutRepo(t *testing.T) {
	d := newDeployment("cp4d", "v1.0.0")
	d.Spec.PostDeployment.Release = "v2.0.0"
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected error when postDeployment.release set without postDeployment.repoName")
	}
}

func TestValidateCreate_PostDeployment_BothSet_Valid(t *testing.T) {
	d := newDeployment("cp4d", "v1.0.0")
	d.Spec.PostDeployment.RepoName = "post-repo"
	d.Spec.PostDeployment.Release = "v2.0.0"
	_, err := validateDeploymentSpec(d, nil)
	if err != nil {
		t.Errorf("expected valid post-deployment spec to pass, got: %v", err)
	}
}

func TestValidateCreate_PostDeployment_InvalidReleaseTag(t *testing.T) {
	d := newDeployment("cp4d", "v1.0.0")
	d.Spec.PostDeployment.RepoName = "post-repo"
	d.Spec.PostDeployment.Release = "bad tag with spaces"
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected error for invalid postDeployment release tag")
	}
}

func TestValidateUpdate_RepoNameImmutable(t *testing.T) {
	old := newDeployment("cp4d", "v1.0.0")
	updated := newDeployment("cp4i", "v1.0.0")
	_, err := validateDeploymentSpec(updated, old)
	if err == nil {
		t.Error("expected error when repoName changed after creation")
	}
}

func TestValidateUpdate_ReleaseImmutable(t *testing.T) {
	old := newDeployment("cp4d", "v1.0.0")
	updated := newDeployment("cp4d", "v1.0.1")
	_, err := validateDeploymentSpec(updated, old)
	if err == nil {
		t.Error("expected error when release changed after creation")
	}
}

func TestValidateUpdate_OtherFieldsMutable(t *testing.T) {
	old := newDeployment("cp4d", "v1.0.0")
	updated := newDeployment("cp4d", "v1.0.0")
	updated.Spec.Parameters = "params.yaml"
	updated.Spec.Ansible.AnsiblePlaybook = "playbooks/install.yml"
	_, err := validateDeploymentSpec(updated, old)
	if err != nil {
		t.Errorf("expected non-immutable field changes to be allowed, got: %v", err)
	}
}

func TestValidateUpdate_NoChanges(t *testing.T) {
	old := newDeployment("cp4d", "v1.0.0")
	updated := newDeployment("cp4d", "v1.0.0")
	_, err := validateDeploymentSpec(updated, old)
	if err != nil {
		t.Errorf("expected no-op update to pass, got: %v", err)
	}
}

func TestValidateCreate_AbsoluteParametersPath_Warning(t *testing.T) {
	d := newDeployment("cp4d", "v1.0.0")
	d.Spec.Parameters = "/absolute/path/params.yaml"
	warnings, err := validateDeploymentSpec(d, nil)
	if err != nil {
		t.Errorf("expected no hard error for absolute parameters path, got: %v", err)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning for absolute parameters path, got none")
	}
}

func TestValidateCreate_RelativeParametersPath_NoWarning(t *testing.T) {
	d := newDeployment("cp4d", "v1.0.0")
	d.Spec.Parameters = "config/params.yaml"
	warnings, err := validateDeploymentSpec(d, nil)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for relative parameters path, got: %v", warnings)
	}
}

func TestValidateCreate_MultipleErrors(t *testing.T) {
	d := newDeployment("", "")
	d.Spec.PostDeployment.RepoName = "post-repo"
	_, err := validateDeploymentSpec(d, nil)
	if err == nil {
		t.Error("expected multiple errors, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "repoName") {
		t.Errorf("expected error to mention repoName, got: %s", msg)
	}
	if !strings.Contains(msg, "release") {
		t.Errorf("expected error to mention release, got: %s", msg)
	}
}
