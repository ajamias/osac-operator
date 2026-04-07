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
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/inventory"
	"github.com/osac-project/osac-operator/internal/lock"
)

// HostLeaseReconciler reconciles a HostLease object
type HostLeaseReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	InventoryClient inventory.Client
	Locker          lock.Locker
}

const HostLeaseInventoryFinalizer = "osac.openshift.io/inventory"
const NoFreeHostsRequeueFrequency = 30 * time.Second
const TryLockFailRequeueFrequency = 1 * time.Second

// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hostleases/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the pool closer to the desired state.
func (r *HostLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	hostLease := &v1alpha1.HostLease{}
	err := r.Get(ctx, req.NamespacedName, hostLease)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !hostLease.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, hostLease)
	}

	return r.handleUpdate(ctx, hostLease)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HostLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.HostLease{}).
		Named("hostlease").
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldHostLease, ok := e.ObjectOld.(*v1alpha1.HostLease)
				if !ok {
					return false
				}
				newHostLease, ok := e.ObjectNew.(*v1alpha1.HostLease)
				if !ok {
					return false
				}

				if !newHostLease.DeletionTimestamp.IsZero() {
					return true
				}

				return oldHostLease.Spec.HostClass == ""
			},
		}).
		Complete(r)
}

// handleUpdate assigns an inventory node to the HostLease CR and marks it as acquired.
func (r *HostLeaseReconciler) handleUpdate(ctx context.Context, hostLease *v1alpha1.HostLease) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Updating HostLease")

	if controllerutil.AddFinalizer(hostLease, HostLeaseInventoryFinalizer) {
		if err := r.Update(ctx, hostLease); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		log.Info("Added finalizer")
		return ctrl.Result{}, nil
	}

	poolID, ok := hostLease.GetPoolID()
	if !ok {
		log.Info("HostLease is orphaned so delete it")
		if err := r.Delete(ctx, hostLease); err != nil {
			log.Error(err, "Failed to delete HostLease")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if hostLease.Spec.ExternalID != "" && hostLease.Spec.HostClass != "" {
		log.Info("HostLease is already fulfilled, nothing to do")
		return ctrl.Result{}, nil
	}

	if hostLease.Spec.ExternalID == "" {
		matchExpressions := maps.Clone(hostLease.Spec.Selector.HostSelector)
		matchExpressions["hostType"] = hostLease.Spec.HostType

		inventoryHost, err := r.InventoryClient.FindFreeHost(ctx, matchExpressions)
		if err != nil {
			log.Error(err, "Failed to find a free host", "matchExpressions", matchExpressions)
			return ctrl.Result{}, err
		}
		if inventoryHost == nil {
			log.Info("No matching hosts available", "matchExpressions", matchExpressions)
			return ctrl.Result{RequeueAfter: NoFreeHostsRequeueFrequency}, nil
		}

		hostLease.Spec.ExternalID = inventoryHost.InventoryHostID
		if err := r.Update(ctx, hostLease); err != nil {
			log.Error(err, "Failed to update HostLease CR with ExternalID", "InventoryHostID", inventoryHost.InventoryHostID)
			return ctrl.Result{}, err
		}

		log.Info("Successfully updated HostLease with inventory host id")
		return ctrl.Result{}, nil
	}

	lockKey := hostLease.Spec.ExternalID
	acquiredLock, err := r.Locker.TryLock(ctx, lockKey)
	if err != nil {
		log.Error(err, "Failed to lock host", "InventoryHostID", lockKey)
		return ctrl.Result{}, err
	}
	if !acquiredLock {
		log.Info("Lock for " + lockKey + " is currently held, retrying...")
		return ctrl.Result{RequeueAfter: TryLockFailRequeueFrequency}, nil
	}
	defer func() {
		// assume that Unlock has a ttl
		_ = r.Locker.Unlock(ctx, lockKey)
	}()

	inventoryHost, err := r.InventoryClient.AssignHost(
		ctx,
		hostLease.Spec.ExternalID,
		poolID,
		string(hostLease.UID),
		nil, // labels
	)
	if err != nil {
		log.Error(err, "Failed to assign host", "InventoryHostID", hostLease.Spec.ExternalID)
		return ctrl.Result{}, err
	}
	if inventoryHost == nil {
		log.Info("Host " + lockKey + " is acquired by a different HostLease, deleting and letting it be recreated...")
		if err = r.Delete(ctx, hostLease); err != nil {
			log.Error(err, "Failed to delete HostLease CR")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	hostLease.Spec.HostClass = inventoryHost.HostClass
	if err = r.Update(ctx, hostLease); err != nil {
		log.Error(err, "Failed to update HostLease CR with HostClass", "HostClass", inventoryHost.HostClass)
		return ctrl.Result{}, err
	}

	log.Info("Successfully fulfilled HostLease", "InventoryHostID", hostLease.Spec.ExternalID)
	return ctrl.Result{}, nil
}

// handleDeletion frees the host in the inventory and removes the finalizer.
func (r *HostLeaseReconciler) handleDeletion(ctx context.Context, hostLease *v1alpha1.HostLease) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Deleting HostLease")

	if !controllerutil.ContainsFinalizer(hostLease, HostLeaseInventoryFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only free in inventory if an inventory host is marked
	if hostLease.Spec.HostClass != "" && hostLease.Spec.ExternalID != "" {
		log.Info("Unassigning host from inventory", "InventoryHostID", hostLease.Spec.ExternalID)

		acquiredLock, err := r.Locker.TryLock(ctx, hostLease.Spec.ExternalID)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !acquiredLock {
			log.Info("Could not acquire lock for host", "InventoryHostID", hostLease.Spec.ExternalID)
			return ctrl.Result{RequeueAfter: TryLockFailRequeueFrequency}, nil
		}
		defer func() {
			// assume that Unlock has a ttl
			_ = r.Locker.Unlock(ctx, hostLease.Spec.ExternalID)
		}()

		err = r.InventoryClient.UnassignHost(ctx, hostLease.Spec.ExternalID, nil)
		if err != nil {
			log.Error(err, "Failed to unassign host in inventory")
			return ctrl.Result{}, err
		}
	}

	if controllerutil.RemoveFinalizer(hostLease, HostLeaseInventoryFinalizer) {
		if err := r.Update(ctx, hostLease); err != nil {
			log.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
	}

	log.Info("Successfully un-fulfilled HostLease")
	return ctrl.Result{}, nil
}
