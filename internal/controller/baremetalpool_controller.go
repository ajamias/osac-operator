/*
Copyright 2026.

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
	"sort"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/utils/v2/openstack/clientconfig"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/osac-project/osac-operator/api/v1alpha1"
)

// BareMetalPoolReconciler reconciles a BareMetalPool object
type BareMetalPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	BareMetalPoolFinalizer = "osac.openshift.io/bare-metal-pool"
	BareMetalPoolLabelKey  = "osac.openshift.io/bare-metal-pool-id"
	HostTypeLabelKey       = "osac.openshift.io/host-type"
)

// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases,verbs=get;list;watch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the pool closer to the desired state.
func (r *BareMetalPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	bareMetalPool := &v1alpha1.BareMetalPool{}
	err := r.Get(ctx, req.NamespacedName, bareMetalPool)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	oldstatus := bareMetalPool.Status.DeepCopy()

	var result ctrl.Result
	if !bareMetalPool.DeletionTimestamp.IsZero() {
		err = r.handleDeletion(ctx, bareMetalPool)
		result = ctrl.Result{}
	} else {
		result, err = r.handleUpdate(ctx, bareMetalPool)
	}

	if !equality.Semantic.DeepEqual(bareMetalPool.Status, *oldstatus) {
		statusErr := r.Status().Update(ctx, bareMetalPool)
		if statusErr != nil {
			return result, statusErr
		}
	}

	return result, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *BareMetalPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.BareMetalPool{}).
		Owns(
			&v1alpha1.HostLease{},
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool {
					return false
				},
				UpdateFunc: func(e event.UpdateEvent) bool {
					return false
				},
				DeleteFunc: func(e event.DeleteEvent) bool {
					return true
				},
			}),
		).
		Named("baremetalpool").
		Complete(r)
}

// handleUpdate processes BareMetalPool creation or specification updates.
func (r *BareMetalPoolReconciler) handleUpdate(ctx context.Context, bareMetalPool *v1alpha1.BareMetalPool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Updating BareMetalPool")

	bareMetalPool.InitializeStatusConditions()

	if controllerutil.AddFinalizer(bareMetalPool, BareMetalPoolFinalizer) {
		if err := r.Update(ctx, bareMetalPool); err != nil {
			log.Error(err, "Failed to add finalizer")
			bareMetalPool.SetStatusCondition(
				v1alpha1.BareMetalPoolConditionTypeReady,
				metav1.ConditionFalse,
				"Failed to add finalizer",
				v1alpha1.BareMetalPoolReasonFailed,
			)
			return ctrl.Result{}, err
		}
		log.Info("Added finalizer")
		return ctrl.Result{}, nil
	}

	networkName := bareMetalPool.Name + "-network"
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.createPrivateNetwork(ctx, networkName)
	}()

	if bareMetalPool.Status.HostSets == nil {
		bareMetalPool.Status.HostSets = []v1alpha1.BareMetalHostSet{}
	}

	// List all HostLease CRs owned by this BareMetalPool
	hostLeaseList := &v1alpha1.HostLeaseList{}
	err := r.List(ctx, hostLeaseList,
		client.InNamespace(bareMetalPool.Namespace),
		client.MatchingLabels{BareMetalPoolLabelKey: string(bareMetalPool.UID)},
	)
	if err != nil {
		log.Error(err, "Failed to list HostLease CRs")
		bareMetalPool.SetStatusCondition(
			v1alpha1.BareMetalPoolConditionTypeReady,
			metav1.ConditionFalse,
			"Failed to list HostLease CRs",
			v1alpha1.BareMetalPoolReasonFailed,
		)
		return ctrl.Result{}, err
	}

	// Group current host leases per hostType, sorted by name for deterministic scale-down
	currentHostLeases := map[string][]*v1alpha1.HostLease{}
	for i := range hostLeaseList.Items {
		hostType := hostLeaseList.Items[i].Spec.HostType
		currentHostLeases[hostType] = append(currentHostLeases[hostType], &hostLeaseList.Items[i])
	}
	for hostType := range currentHostLeases {
		sort.Slice(currentHostLeases[hostType], func(i, j int) bool {
			return currentHostLeases[hostType][i].Name < currentHostLeases[hostType][j].Name
		})
	}

	// Build a map of desired replicas for easier lookup
	desiredReplicas := map[string]int32{}
	for _, hostSet := range bareMetalPool.Spec.HostSets {
		desiredReplicas[hostSet.HostType] = hostSet.Replicas
	}

	// Scale up or down for each desired hostType
	defer r.updateStatusHostSets(bareMetalPool, currentHostLeases)
	for hostType, replicas := range desiredReplicas {
		delta := replicas - int32(len(currentHostLeases[hostType]))
		if delta > 0 {
			for range delta {
				if err := r.createHostLeaseCR(ctx, bareMetalPool, hostType, networkName); err != nil {
					log.Error(err, "Failed to create HostLease CR")
					bareMetalPool.SetStatusCondition(
						v1alpha1.BareMetalPoolConditionTypeReady,
						metav1.ConditionFalse,
						"Failed to create HostLease CR",
						v1alpha1.BareMetalPoolReasonFailed,
					)
					return ctrl.Result{}, err
				}
				currentHostLeases[hostType] = append(currentHostLeases[hostType], nil)
			}
		} else if delta < 0 {
			for int32(len(currentHostLeases[hostType])) > replicas {
				lastIdx := len(currentHostLeases[hostType]) - 1
				hostLeaseToDelete := currentHostLeases[hostType][lastIdx]
				if err := r.Delete(ctx, hostLeaseToDelete); client.IgnoreNotFound(err) != nil {
					log.Error(err, "Failed to delete HostLease CR", "hostLease", hostLeaseToDelete.Name)
					bareMetalPool.SetStatusCondition(
						v1alpha1.BareMetalPoolConditionTypeReady,
						metav1.ConditionFalse,
						"Failed to delete HostLease CR",
						v1alpha1.BareMetalPoolReasonFailed,
					)
					return ctrl.Result{}, err
				}
				currentHostLeases[hostType] = currentHostLeases[hostType][:lastIdx]
			}
		}
	}

	// Delete host leases for hostTypes no longer in spec
	for hostType := range currentHostLeases {
		if _, ok := desiredReplicas[hostType]; ok {
			continue
		}
		for len(currentHostLeases[hostType]) > 0 {
			lastIdx := len(currentHostLeases[hostType]) - 1
			hostLeaseToDelete := currentHostLeases[hostType][lastIdx]
			if err := r.Delete(ctx, hostLeaseToDelete); client.IgnoreNotFound(err) != nil {
				log.Error(err, "Failed to delete HostLease CR", "hostLease", hostLeaseToDelete.Name)
				bareMetalPool.SetStatusCondition(
					v1alpha1.BareMetalPoolConditionTypeReady,
					metav1.ConditionFalse,
					"Failed to delete HostLease CR",
					v1alpha1.BareMetalPoolReasonFailed,
				)
				return ctrl.Result{}, err
			}
			currentHostLeases[hostType] = currentHostLeases[hostType][:lastIdx]
		}
		delete(currentHostLeases, hostType)
	}

	if err := <-errCh; err != nil {
		log.Error(err, "Failed to create network", "networkName", networkName)
		bareMetalPool.SetStatusCondition(
			v1alpha1.BareMetalPoolConditionTypeReady,
			metav1.ConditionFalse,
			"Failed to create network",
			v1alpha1.BareMetalPoolReasonFailed,
		)
		return ctrl.Result{}, err
	}

	bareMetalPool.SetStatusCondition(
		v1alpha1.BareMetalPoolConditionTypeReady,
		metav1.ConditionTrue,
		"Successfully reconciled host leases",
		v1alpha1.BareMetalPoolReasonReady,
	)

	log.Info("Successfully updated BareMetalPool")
	return ctrl.Result{}, nil
}

// handleDeletion handles the cleanup when a BareMetalPool is being deleted
func (r *BareMetalPoolReconciler) handleDeletion(ctx context.Context, bareMetalPool *v1alpha1.BareMetalPool) error {
	log := logf.FromContext(ctx)
	log.Info("Deleting BareMetalPool")

	bareMetalPool.SetStatusCondition(
		v1alpha1.BareMetalPoolConditionTypeReady,
		metav1.ConditionFalse,
		"BareMetalPool is being torn down",
		v1alpha1.BareMetalPoolReasonDeleting,
	)

	// TODO: add profile (teardown) logic

	networkName := bareMetalPool.Name + "-network"
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.deletePrivateNetwork(ctx, networkName)
	}()

	hostLeaseList := &v1alpha1.HostLeaseList{}
	err := r.List(ctx, hostLeaseList,
		client.InNamespace(bareMetalPool.Namespace),
		client.MatchingLabels{BareMetalPoolLabelKey: string(bareMetalPool.UID)},
	)
	if err != nil {
		log.Error(err, "Failed to list HostLease CRs during deletion")
		return err
	}

	// Move all HostLeases to idle-agents-network
	for i := range hostLeaseList.Items {
		hostLease := &hostLeaseList.Items[i]
		if len(hostLease.Spec.NetworkInterfaces) > 0 &&
			hostLease.Spec.NetworkInterfaces[0].Network != "idle-agents-network" {
			hostLease.Spec.NetworkInterfaces[0].Network = "idle-agents-network"
			if err := r.Update(ctx, hostLease); err != nil {
				log.Error(err, "Failed to update HostLease network to idle-agents-network", "hostLease", hostLease.Name)
				return err
			}
			log.Info("Updated HostLease network to idle-agents-network", "hostLease", hostLease.Name)
		}
	}

	if err := <-errCh; err != nil {
		log.Error(err, "Failed to delete network", "networkName", networkName)
		return err
	}

	if controllerutil.RemoveFinalizer(bareMetalPool, BareMetalPoolFinalizer) {
		if err := r.Update(ctx, bareMetalPool); err != nil {
			log.Info("Failed to remove finalizer")
			return err
		}
	}

	log.Info("Successfully deleted BareMetalPool")
	return nil
}

// createHostLeaseCR creates a new HostLease CR owned by this BareMetalPool
func (r *BareMetalPoolReconciler) createHostLeaseCR(
	ctx context.Context,
	bareMetalPool *v1alpha1.BareMetalPool,
	hostType string,
	networkName string,
) error {
	log := logf.FromContext(ctx)

	hostLeaseName := fmt.Sprintf("%s-host-%s-%s", bareMetalPool.Name, hostType, rand.String(5))
	namespace := bareMetalPool.Namespace
	labels := map[string]string{
		BareMetalPoolLabelKey: string(bareMetalPool.UID),
		HostTypeLabelKey:      hostType,
	}

	selector := v1alpha1.HostSelectorSpec{
		HostSelector: map[string]string{ // TODO: add selectors from profile
			"managedBy":      "agent",
			"provisionState": "active",
		},
	}
	templateID := "default_template"
	templateParameters := ""
	if bareMetalPool.Spec.Profile != nil {
		if bareMetalPool.Spec.Profile.Name != "" {
			templateID = "other_tempate" // TODO: get template id from profile
		}
		templateParameters = bareMetalPool.Spec.Profile.TemplateParameters
	}

	hostLeaseCR := &v1alpha1.HostLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hostLeaseName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1alpha1.HostLeaseSpec{
			HostType:           hostType,
			ExternalID:         "",
			ExternalName:       "",
			Selector:           selector,
			TemplateID:         templateID,
			TemplateParameters: templateParameters,
			PoweredOn:          false,
			NetworkInterfaces: []v1alpha1.NetworkInterfaceSpec{
				{
					Network: networkName,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(bareMetalPool, hostLeaseCR, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference", "hostLease", hostLeaseName)
		return err
	}
	if err := r.Create(ctx, hostLeaseCR); client.IgnoreAlreadyExists(err) != nil {
		log.Error(err, "Failed to create HostLease CR", "hostLease", hostLeaseName)
		return err
	}

	log.Info("Created HostLease CR", "hostLease", hostLeaseName)
	return nil
}

// updateStatusHostSets updates status.HostSets from the current host leases map.
func (r *BareMetalPoolReconciler) updateStatusHostSets(bareMetalPool *v1alpha1.BareMetalPool, currentHostLeases map[string][]*v1alpha1.HostLease) {
	updatedHostSets := []v1alpha1.BareMetalHostSet{}
	for hostType, hostLeases := range currentHostLeases {
		if len(hostLeases) > 0 {
			updatedHostSets = append(updatedHostSets, v1alpha1.BareMetalHostSet{
				HostType: hostType,
				Replicas: int32(len(hostLeases)),
			})
		}
	}
	bareMetalPool.Status.HostSets = updatedHostSets
}

func (r *BareMetalPoolReconciler) createPrivateNetwork(ctx context.Context, networkName string) error {
	log := logf.FromContext(ctx)

	// Create OpenStack client using clouds.yaml
	clientOpts := &clientconfig.ClientOpts{
		Cloud: "ajamias",
	}

	provider, err := clientconfig.AuthenticatedClient(ctx, clientOpts)
	if err != nil {
		return err
	}

	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		return err
	}

	// Check if network already exists
	listOpts := networks.ListOpts{
		Name: networkName,
	}
	allPages, err := networks.List(networkClient, listOpts).AllPages(ctx)
	if err != nil {
		return err
	}
	allNetworks, err := networks.ExtractNetworks(allPages)
	if err != nil {
		return err
	}
	if len(allNetworks) > 0 {
		log.Info("Private network already exists", "networkName", networkName, "networkID", allNetworks[0].ID)
		return nil
	}

	// Create the private network
	adminStateUp := true
	createOpts := networks.CreateOpts{
		Name:         networkName,
		AdminStateUp: &adminStateUp,
	}
	result := networks.Create(ctx, networkClient, createOpts)
	network, err := result.Extract()
	if err != nil {
		return err
	}

	log.Info("Successfully created private network", "networkName", networkName, "networkID", network.ID)
	return nil
}

func (r *BareMetalPoolReconciler) deletePrivateNetwork(ctx context.Context, networkName string) error {
	log := logf.FromContext(ctx)

	// Create OpenStack client using clouds.yaml
	clientOpts := &clientconfig.ClientOpts{
		Cloud: "ajamias",
	}

	provider, err := clientconfig.AuthenticatedClient(ctx, clientOpts)
	if err != nil {
		return err
	}

	networkClient, err := openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{})
	if err != nil {
		return err
	}

	// Find the network by name
	listOpts := networks.ListOpts{
		Name: networkName,
	}
	allPages, err := networks.List(networkClient, listOpts).AllPages(ctx)
	if err != nil {
		return err
	}
	allNetworks, err := networks.ExtractNetworks(allPages)
	if err != nil {
		return err
	}
	if len(allNetworks) == 0 {
		log.Info("Private network does not exist, nothing to delete", "networkName", networkName)
		return nil
	}

	// Delete the network
	networkID := allNetworks[0].ID
	err = networks.Delete(ctx, networkClient, networkID).ExtractErr()
	if err != nil {
		return err
	}

	log.Info("Successfully deleted private network", "networkName", networkName, "networkID", networkID)
	return nil
}
