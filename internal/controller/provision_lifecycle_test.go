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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/osac-project/osac-operator/internal/provisioning"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

var _ = Describe("provisioning.EvaluateAction", func() {
	noAPIServerJob := func() bool { return false }
	apiServerHasJob := func() bool { return true }

	DescribeTable("returns the correct action",
		func(jobs []v1alpha1.JobStatus, desiredVersion string, reconciledVersion string, checkAPIServer func() bool, expectedAction provisioning.Action) {
			state := &provisioning.State{
				Jobs:                    &jobs,
				DesiredConfigVersion:    desiredVersion,
				ReconciledConfigVersion: reconciledVersion,
			}
			action, _ := provisioning.EvaluateAction(state, checkAPIServer)
			Expect(action).To(Equal(expectedAction))
		},

		Entry("no jobs, versions match -> skip",
			[]v1alpha1.JobStatus{},
			"v1", "v1",
			noAPIServerJob,
			provisioning.Skip,
		),

		Entry("no jobs, versions differ -> trigger",
			[]v1alpha1.JobStatus{},
			"v2", "v1",
			noAPIServerJob,
			provisioning.Trigger,
		),

		Entry("no jobs, versions differ, API server has job -> requeue",
			[]v1alpha1.JobStatus{},
			"v2", "v1",
			apiServerHasJob,
			provisioning.Requeue,
		),

		Entry("running job -> poll",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			provisioning.Poll,
		),

		Entry("succeeded job with matching config version -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			provisioning.Skip,
		),

		Entry("failed job with matching config version -> backoff",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			provisioning.Backoff,
		),

		Entry("failed job with different config version -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "",
			noAPIServerJob,
			provisioning.Trigger,
		),

		Entry("terminal job without config version, versions match -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "v1",
			noAPIServerJob,
			provisioning.Skip,
		),

		Entry("terminal job without config version, versions differ -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "v1",
			noAPIServerJob,
			provisioning.Trigger,
		),

		Entry("job with empty ID (trigger failed) and versions differ -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "v1",
			noAPIServerJob,
			provisioning.Trigger,
		),
	)
})

var _ = Describe("provisioning.ComputeDesiredConfigVersion", func() {
	It("produces consistent hashes for the same input", func() {
		spec := map[string]string{"key": "value"}
		v1, err := provisioning.ComputeDesiredConfigVersion(spec)
		Expect(err).NotTo(HaveOccurred())
		v2, err := provisioning.ComputeDesiredConfigVersion(spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).To(Equal(v2))
	})

	It("produces different hashes for different inputs", func() {
		v1, err := provisioning.ComputeDesiredConfigVersion(map[string]string{"key": "a"})
		Expect(err).NotTo(HaveOccurred())
		v2, err := provisioning.ComputeDesiredConfigVersion(map[string]string{"key": "b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).NotTo(Equal(v2))
	})
})

var _ = Describe("provisioning.SyncReconciledConfigVersion", func() {
	It("returns the annotation value when present", func() {
		annotations := map[string]string{osacReconciledConfigVersionAnnotation: "v1"}
		Expect(provisioning.SyncReconciledConfigVersion(ctx, annotations, osacReconciledConfigVersionAnnotation)).To(Equal("v1"))
	})

	It("returns empty string when annotation is absent", func() {
		Expect(provisioning.SyncReconciledConfigVersion(ctx, map[string]string{}, osacReconciledConfigVersionAnnotation)).To(BeEmpty())
	})

	It("returns empty string when annotations map is nil", func() {
		Expect(provisioning.SyncReconciledConfigVersion(ctx, nil, osacReconciledConfigVersionAnnotation)).To(BeEmpty())
	})
})
