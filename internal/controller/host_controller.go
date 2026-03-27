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

// HostReconciler reconciles a Host object
type HostReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	InventoryClient inventory.Client
	Locker          lock.Locker
}

const HostInventoryFinalizer = "osac.openshift.io/inventory"
const NoFreeHostsRequeueFrequency = 30 * time.Second
const TryLockFailRequeueFrequency = 1 * time.Second

// +kubebuilder:rbac:groups=osac.openshift.io,resources=hosts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=hosts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the pool closer to the desired state.
func (r *HostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	host := &v1alpha1.Host{}
	err := r.Get(ctx, req.NamespacedName, host)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !host.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, host)
	}

	return r.handleUpdate(ctx, host)
}

// SetupWithManager sets up the controller with the Manager.
func (r *HostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Host{}).
		Named("host").
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldHost, ok := e.ObjectOld.(*v1alpha1.Host)
				if !ok {
					return false
				}
				return oldHost.Spec.HostManagementClass == ""
			},
		}).
		Complete(r)
}

// handleUpdate assigns an inventory node to the Host CR and marks it as acquired.
func (r *HostReconciler) handleUpdate(ctx context.Context, host *v1alpha1.Host) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if controllerutil.AddFinalizer(host, HostInventoryFinalizer) {
		if err := r.Update(ctx, host); err != nil {
			log.Error(err, "Failed to add finalizer", "host", host.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	poolID, ok := host.GetPoolID()
	if !ok {
		log.Info("Host is orphaned so delete it")
		if err := r.Delete(ctx, host); err != nil {
			log.Error(err, "Failed to delete Host", "host", host.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if host.Spec.ID != "" && host.Spec.HostManagementClass != "" {
		log.Info("Host is already acquired, nothing to do")
		return ctrl.Result{}, nil
	}

	if host.Spec.ID == "" {
		matchExpressions := maps.Clone(host.Spec.Selector.HostSelector)
		matchExpressions["hostClass"] = host.Spec.HostClass

		inventoryHost, err := r.InventoryClient.FindFreeHost(ctx, matchExpressions)
		if err != nil {
			log.Error(err, "Failed to find a free host", "host", host.Name, "matchExpressions", matchExpressions)
			return ctrl.Result{}, err
		}
		if inventoryHost == nil {
			log.Info("No matching hosts available", "host", host.Name, "matchExpressions", matchExpressions)
			return ctrl.Result{RequeueAfter: NoFreeHostsRequeueFrequency}, nil
		}

		host.Spec.ID = inventoryHost.InventoryHostID
		if err := r.Status().Update(ctx, host); err != nil {
			log.Error(err, "Failed to update Host CR status with ID", "host", host.Name, "InventoryHostID", inventoryHost.InventoryHostID)
			return ctrl.Result{}, err
		}

		log.Info("Successfully updated host with inventory host id")
		return ctrl.Result{}, nil
	}

	lockKey := host.Spec.ID
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
		host.Spec.ID,
		poolID,
		string(host.UID),
		nil, // labels
	)
	if err != nil {
		log.Error(err, "Failed to assign host", "host", host.Name, "InventoryHostID", host.Spec.ID)
		return ctrl.Result{}, err
	}
	if inventoryHost == nil {
		log.Info("Host " + lockKey + " is acquired by a different CR, resetting...")
		host.Spec.ID = ""
		if err = r.Status().Update(ctx, host); err != nil {
			log.Error(err, "Failed to update Host CR status to remove ID", "host", host.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	host.Spec.HostManagementClass = inventoryHost.ManagementClass
	if err = r.Status().Update(ctx, host); err != nil {
		log.Error(err, "Failed to update Host CR status with ManagementClass", "host", host.Name, "ManagementClass", inventoryHost.ManagementClass)
		return ctrl.Result{}, err
	}

	log.Info("Successfully assigned and acquired host", "host", host.Name, "InventoryHostID", host.Spec.ID)
	return ctrl.Result{}, nil
}

// handleDeletion frees the host in the inventory and removes the finalizer.
func (r *HostReconciler) handleDeletion(ctx context.Context, host *v1alpha1.Host) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(host, HostInventoryFinalizer) {
		return ctrl.Result{}, nil
	}

	// Only free in inventory if an inventory host is marked
	if host.Spec.HostManagementClass != "" && host.Spec.ID != "" {
		log.Info("Unassigning host from inventory", "host", host.Name, "InventoryHostID", host.Spec.ID)

		acquiredLock, err := r.Locker.TryLock(ctx, host.Spec.ID)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !acquiredLock {
			log.Info("Could not acquire lock for host", "host", host.Name, "InventoryHostID", host.Spec.ID)
			return ctrl.Result{RequeueAfter: TryLockFailRequeueFrequency}, nil
		}
		defer func() {
			// assume that Unlock has a ttl
			_ = r.Locker.Unlock(ctx, host.Spec.ID)
		}()

		err = r.InventoryClient.UnassignHost(ctx, host.Spec.ID, nil)
		if err != nil {
			log.Error(err, "Failed to unassign host in inventory", "host", host.Name)
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(host, HostInventoryFinalizer)
	if err := r.Update(ctx, host); err != nil {
		log.Error(err, "Failed to remove finalizer", "host", host.Name)
		return ctrl.Result{}, err
	}

	log.Info("Successfully freed host in inventory", "host", host.Name)
	return ctrl.Result{}, nil
}
