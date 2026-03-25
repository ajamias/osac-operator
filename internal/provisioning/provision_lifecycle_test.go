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

package provisioning

import (
	"context"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

var ctx = context.Background()

var _ = ginkgo.Describe("EvaluateAction", func() {
	noAPIServerJob := func() bool { return false }
	apiServerHasJob := func() bool { return true }

	ginkgo.DescribeTable("returns the correct action",
		func(jobs []v1alpha1.JobStatus, desiredVersion string, reconciledVersion string, checkAPIServer func() bool, expectedAction Action) {
			state := &State{
				Jobs:                    &jobs,
				DesiredConfigVersion:    desiredVersion,
				ReconciledConfigVersion: reconciledVersion,
			}
			action, _ := EvaluateAction(state, checkAPIServer)
			Expect(action).To(Equal(expectedAction))
		},

		ginkgo.Entry("no jobs, versions match -> skip",
			[]v1alpha1.JobStatus{},
			"v1", "v1",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("no jobs, versions differ -> trigger",
			[]v1alpha1.JobStatus{},
			"v2", "v1",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("no jobs, versions differ, API server has job -> requeue",
			[]v1alpha1.JobStatus{},
			"v2", "v1",
			apiServerHasJob,
			Requeue,
		),

		ginkgo.Entry("running job -> poll",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateRunning, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			Poll,
		),

		ginkgo.Entry("succeeded job with matching config version -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("failed job with matching config version -> backoff",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "",
			noAPIServerJob,
			Backoff,
		),

		ginkgo.Entry("failed job with different config version -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, ConfigVersion: "v1", Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("terminal job without config version, versions match -> skip",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v1", "v1",
			noAPIServerJob,
			Skip,
		),

		ginkgo.Entry("terminal job without config version, versions differ -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "100", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateSucceeded, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "v1",
			noAPIServerJob,
			Trigger,
		),

		ginkgo.Entry("job with empty ID (trigger failed) and versions differ -> trigger",
			[]v1alpha1.JobStatus{
				{JobID: "", Type: v1alpha1.JobTypeProvision, State: v1alpha1.JobStateFailed, Timestamp: metav1.NewTime(time.Now())},
			},
			"v2", "v1",
			noAPIServerJob,
			Trigger,
		),
	)
})

var _ = ginkgo.Describe("ComputeDesiredConfigVersion", func() {
	ginkgo.It("produces consistent hashes for the same input", func() {
		spec := map[string]string{"key": "value"}
		v1, err := ComputeDesiredConfigVersion(spec)
		Expect(err).NotTo(HaveOccurred())
		v2, err := ComputeDesiredConfigVersion(spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).To(Equal(v2))
	})

	ginkgo.It("produces different hashes for different inputs", func() {
		v1, err := ComputeDesiredConfigVersion(map[string]string{"key": "a"})
		Expect(err).NotTo(HaveOccurred())
		v2, err := ComputeDesiredConfigVersion(map[string]string{"key": "b"})
		Expect(err).NotTo(HaveOccurred())
		Expect(v1).NotTo(Equal(v2))
	})
})

var _ = ginkgo.Describe("SyncReconciledConfigVersion", func() {
	ginkgo.It("returns the annotation value when present", func() {
		annotations := map[string]string{"osac.openshift.io/reconciled-config-version": "v1"}
		Expect(SyncReconciledConfigVersion(ctx, annotations, "osac.openshift.io/reconciled-config-version")).To(Equal("v1"))
	})

	ginkgo.It("returns empty string when annotation is absent", func() {
		Expect(SyncReconciledConfigVersion(ctx, map[string]string{}, "osac.openshift.io/reconciled-config-version")).To(BeEmpty())
	})

	ginkgo.It("returns empty string when annotations map is nil", func() {
		Expect(SyncReconciledConfigVersion(ctx, nil, "osac.openshift.io/reconciled-config-version")).To(BeEmpty())
	})
})
