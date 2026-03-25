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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/helpers"
)

// State points into the resource's status fields used by the provisioning lifecycle.
// Jobs is a pointer so shared functions can modify the slice in place.
type State struct {
	Jobs                    *[]v1alpha1.JobStatus
	DesiredConfigVersion    string
	ReconciledConfigVersion string
}

// EvaluateAction determines the next provisioning action based on job history and config versions.
func EvaluateAction(provState *State, checkAPIServer func() bool) (Action, *v1alpha1.JobStatus) {
	latestJob := v1alpha1.FindLatestJobByType(*provState.Jobs, v1alpha1.JobTypeProvision)

	if !HasJobID(latestJob) {
		if provState.DesiredConfigVersion == provState.ReconciledConfigVersion {
			return Skip, latestJob
		}
	} else if !latestJob.State.IsTerminal() {
		return Poll, latestJob
	} else if latestJob.ConfigVersion != "" {
		if latestJob.ConfigVersion == provState.DesiredConfigVersion {
			if latestJob.State == v1alpha1.JobStateSucceeded {
				return Skip, latestJob
			}
			return Backoff, latestJob
		}
	} else if provState.DesiredConfigVersion == provState.ReconciledConfigVersion {
		return Skip, latestJob
	}

	if checkAPIServer() {
		return Requeue, nil
	}
	return Trigger, latestJob
}

// CheckAPIServerForNonTerminalProvisionJob reads the resource directly from the API server
// and returns true if a non-terminal provision job exists.
func CheckAPIServerForNonTerminalProvisionJob(ctx context.Context, apiReader client.Reader, key client.ObjectKey, fresh client.Object) bool {
	log := ctrllog.FromContext(ctx)
	if err := apiReader.Get(ctx, key, fresh); err != nil {
		return false
	}
	freshJobs := GetJobsFromResource(fresh)
	freshJob := v1alpha1.FindLatestJobByType(freshJobs, v1alpha1.JobTypeProvision)
	if HasJobID(freshJob) && !freshJob.State.IsTerminal() {
		log.Info("skipping provision trigger: non-terminal job found via API server", "jobID", freshJob.JobID, "state", freshJob.State)
		return true
	}
	return false
}

// TriggerJob triggers a new provision job and updates the jobs slice in place via State.
func TriggerJob(ctx context.Context, provider ProvisioningProvider, resource client.Object, provState *State, maxHistory int, pollInterval time.Duration) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("triggering provision job")

	result, err := provider.TriggerProvision(ctx, resource)
	if err != nil {
		if rateLimitErr, ok := AsRateLimitError(err); ok {
			log.Info("provision request rate-limited, requeueing", "retryAfter", rateLimitErr.RetryAfter)
			return ctrl.Result{RequeueAfter: rateLimitErr.RetryAfter}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to trigger provision: %w", err)
	}

	*provState.Jobs = helpers.AppendJob(*provState.Jobs, v1alpha1.JobStatus{
		JobID:         result.JobID,
		Type:          v1alpha1.JobTypeProvision,
		State:         result.InitialState,
		Message:       result.Message,
		Timestamp:     metav1.NewTime(time.Now().UTC()),
		ConfigVersion: provState.DesiredConfigVersion,
	}, maxHistory)

	latestJob := v1alpha1.FindLatestJobByType(*provState.Jobs, v1alpha1.JobTypeProvision)
	log.Info("provision job triggered", "jobID", latestJob.JobID, "configVersion", latestJob.ConfigVersion)
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// PollJob checks the status of an existing provision job and updates the jobs slice in place.
// onFailed is called when the job transitions to Failed state (e.g. to set the resource phase).
func PollJob(ctx context.Context, provider ProvisioningProvider, resource client.Object, provState *State, latestJob *v1alpha1.JobStatus, pollInterval time.Duration, onFailed func(message string)) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("polling provision job status", "jobID", latestJob.JobID, "currentState", latestJob.State)

	status, err := provider.GetProvisionStatus(ctx, resource, latestJob.JobID)
	if err != nil {
		log.Error(err, "failed to get provision status", "jobID", latestJob.JobID)
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	if status.State != latestJob.State || status.Message != latestJob.Message {
		log.Info("provision job status changed", "jobID", latestJob.JobID, "oldState", latestJob.State, "newState", status.State)
		updatedJob := *latestJob
		updatedJob.State = status.State
		updatedJob.Message = status.Message
		helpers.UpdateJob(*provState.Jobs, updatedJob)

		if status.State == v1alpha1.JobStateFailed {
			log.Info("provision job failed", "jobID", latestJob.JobID)
			if onFailed != nil {
				onFailed(status.Message)
			}
		}
	}

	if !status.State.IsTerminal() {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	return ctrl.Result{}, nil
}

// ComputeDesiredConfigVersion computes a hash of the spec and returns it.
func ComputeDesiredConfigVersion(spec interface{}) (string, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("failed to marshal spec to JSON: %w", err)
	}
	hasher := fnv.New64a()
	if _, err := hasher.Write(specJSON); err != nil {
		return "", fmt.Errorf("failed to write to hash: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// SyncReconciledConfigVersion returns the reconciled config version from the given annotation key, or empty string if not set.
func SyncReconciledConfigVersion(ctx context.Context, annotations map[string]string, annotationKey string) string {
	log := ctrllog.FromContext(ctx)
	if version, exists := annotations[annotationKey]; exists {
		log.V(1).Info("copied reconciled config version from annotation", "version", version)
		return version
	}
	return ""
}
