/*
Copyright (c) 2025 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package controller

import (
	"context"

	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

// SubnetFeedbackReconciler sends updates to the fulfillment service.
type SubnetFeedbackReconciler struct {
	hubClient           clnt.Client
	subnetsClient       privatev1.SubnetsClient
	networkingNamespace string
}

// subnetFeedbackReconcilerTask contains data that is used for the reconciliation of a specific subnet, so there is less
// need to pass around as function parameters that and other related objects.
type subnetFeedbackReconcilerTask struct {
	r      *SubnetFeedbackReconciler
	object *v1alpha1.Subnet
	subnet *privatev1.Subnet
}

// NewSubnetFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about subnets.
func NewSubnetFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *SubnetFeedbackReconciler {
	return &SubnetFeedbackReconciler{
		hubClient:           hubClient,
		subnetsClient:       privatev1.NewSubnetsClient(grpcConn),
		networkingNamespace: networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *SubnetFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("subnet-feedback").
		For(&v1alpha1.Subnet{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *SubnetFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.Subnet{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Get the identifier of the subnet from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	subnetID, ok := object.Labels[osacSubnetIDLabel]
	if !ok {
		log.Info(
			"There is no label containing the subnet identifier, will ignore it",
			"label", osacSubnetIDLabel,
		)
		return
	}

	// Check if the Subnet is being deleted before fetching from fulfillment service
	if !object.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info(
			"Subnet is being deleted, skipping feedback reconciliation",
		)
		return
	}

	// Fetch the subnet:
	subnet, err := r.fetchSubnet(ctx, subnetID)
	if err != nil {
		return
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &subnetFeedbackReconcilerTask{
		r:      r,
		object: object,
		subnet: clone(subnet),
	}

	t.handleUpdate(ctx)

	// Save the objects that have changed:
	err = r.saveSubnet(ctx, subnet, t.subnet)
	if err != nil {
		return
	}
	return
}

func (r *SubnetFeedbackReconciler) fetchSubnet(ctx context.Context, id string) (subnet *privatev1.Subnet, err error) {
	response, err := r.subnetsClient.Get(ctx, privatev1.SubnetsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	subnet = response.GetObject()
	if !subnet.HasSpec() {
		subnet.SetSpec(&privatev1.SubnetSpec{})
	}
	if !subnet.HasStatus() {
		subnet.SetStatus(&privatev1.SubnetStatus{})
	}
	return
}

func (r *SubnetFeedbackReconciler) saveSubnet(ctx context.Context, before, after *privatev1.Subnet) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating subnet",
			"before", before,
			"after", after,
		)
		_, err := r.subnetsClient.Update(ctx, privatev1.SubnetsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *subnetFeedbackReconcilerTask) handleUpdate(ctx context.Context) {
	t.syncPhase(ctx)
	t.syncBackendNetworkID()
}

func (t *subnetFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.SubnetPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.SubnetPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.SubnetPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.SubnetPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

func (t *subnetFeedbackReconcilerTask) syncPhaseProgressing() {
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_PENDING)
}

func (t *subnetFeedbackReconcilerTask) syncPhaseFailed() {
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_FAILED)
}

func (t *subnetFeedbackReconcilerTask) syncPhaseReady() {
	subnetStatus := t.subnet.GetStatus()
	subnetStatus.SetState(privatev1.SubnetState_SUBNET_STATE_READY)
}

func (t *subnetFeedbackReconcilerTask) syncPhaseDeleting() {
	// Deleting state maps to PENDING as deletion is in progress
	t.subnet.GetStatus().SetState(privatev1.SubnetState_SUBNET_STATE_PENDING)
}

func (t *subnetFeedbackReconcilerTask) syncBackendNetworkID() {
	if t.object.Status.BackendNetworkID != "" {
		t.subnet.GetStatus().SetMessage(t.object.Status.BackendNetworkID)
	}
}
