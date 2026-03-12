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

// VirtualNetworkFeedbackReconciler sends updates to the fulfillment service.
type VirtualNetworkFeedbackReconciler struct {
	hubClient             clnt.Client
	virtualNetworksClient privatev1.VirtualNetworksClient
	networkingNamespace   string
}

// virtualNetworkFeedbackReconcilerTask contains data that is used for the reconciliation of a specific virtual network, so there is less
// need to pass around as function parameters that and other related objects.
type virtualNetworkFeedbackReconcilerTask struct {
	r              *VirtualNetworkFeedbackReconciler
	object         *v1alpha1.VirtualNetwork
	virtualNetwork *privatev1.VirtualNetwork
}

// NewVirtualNetworkFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about virtual networks.
func NewVirtualNetworkFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *VirtualNetworkFeedbackReconciler {
	return &VirtualNetworkFeedbackReconciler{
		hubClient:             hubClient,
		virtualNetworksClient: privatev1.NewVirtualNetworksClient(grpcConn),
		networkingNamespace:   networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *VirtualNetworkFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("virtualnetwork-feedback").
		For(&v1alpha1.VirtualNetwork{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *VirtualNetworkFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.VirtualNetwork{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Get the identifier of the virtual network from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	virtualNetworkID, ok := object.Labels[osacVirtualNetworkIDLabel]
	if !ok {
		log.Info(
			"There is no label containing the virtual network identifier, will ignore it",
			"label", osacVirtualNetworkIDLabel,
		)
		return
	}

	// Check if the VirtualNetwork is being deleted before fetching from fulfillment service
	if !object.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info(
			"VirtualNetwork is being deleted, skipping feedback reconciliation",
		)
		return
	}

	// Fetch the virtual network:
	virtualNetwork, err := r.fetchVirtualNetwork(ctx, virtualNetworkID)
	if err != nil {
		return
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &virtualNetworkFeedbackReconcilerTask{
		r:              r,
		object:         object,
		virtualNetwork: clone(virtualNetwork),
	}

	t.handleUpdate(ctx)

	// Save the objects that have changed:
	err = r.saveVirtualNetwork(ctx, virtualNetwork, t.virtualNetwork)
	if err != nil {
		return
	}
	return
}

func (r *VirtualNetworkFeedbackReconciler) fetchVirtualNetwork(ctx context.Context, id string) (virtualNetwork *privatev1.VirtualNetwork, err error) {
	response, err := r.virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	virtualNetwork = response.GetObject()
	if !virtualNetwork.HasSpec() {
		virtualNetwork.SetSpec(&privatev1.VirtualNetworkSpec{})
	}
	if !virtualNetwork.HasStatus() {
		virtualNetwork.SetStatus(&privatev1.VirtualNetworkStatus{})
	}
	return
}

func (r *VirtualNetworkFeedbackReconciler) saveVirtualNetwork(ctx context.Context, before, after *privatev1.VirtualNetwork) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating virtual network",
			"before", before,
			"after", after,
		)
		_, err := r.virtualNetworksClient.Update(ctx, privatev1.VirtualNetworksUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *virtualNetworkFeedbackReconcilerTask) handleUpdate(ctx context.Context) {
	t.syncPhase(ctx)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.VirtualNetworkPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.VirtualNetworkPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.VirtualNetworkPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.VirtualNetworkPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseProgressing() {
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_PENDING)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseFailed() {
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_FAILED)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseReady() {
	virtualNetworkStatus := t.virtualNetwork.GetStatus()
	virtualNetworkStatus.SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_READY)
}

func (t *virtualNetworkFeedbackReconcilerTask) syncPhaseDeleting() {
	// Deleting state maps to PENDING as deletion is in progress
	t.virtualNetwork.GetStatus().SetState(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_PENDING)
}
