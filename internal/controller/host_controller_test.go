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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac-operator/internal/inventory"
)

// mockInventoryClient implements inventory.Client for testing
type mockInventoryClient struct {
	findFreeHostFunc func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error)
	assignHostFunc   func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error)
	unassignHostFunc func(ctx context.Context, inventoryHostID string, labels []string) error
}

func (m *mockInventoryClient) FindFreeHost(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
	if m.findFreeHostFunc != nil {
		return m.findFreeHostFunc(ctx, matchExpressions)
	}
	return nil, nil
}

func (m *mockInventoryClient) AssignHost(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
	if m.assignHostFunc != nil {
		return m.assignHostFunc(ctx, inventoryHostID, bareMetalPoolID, bareMetalPoolHostID, labels)
	}
	return nil, nil
}

func (m *mockInventoryClient) UnassignHost(ctx context.Context, inventoryHostID string, labels []string) error {
	if m.unassignHostFunc != nil {
		return m.unassignHostFunc(ctx, inventoryHostID, labels)
	}
	return nil
}

// mockLocker implements lock.Locker for testing
type mockLocker struct {
	tryLockFunc    func(ctx context.Context, key string) (bool, error)
	unlockFunc     func(ctx context.Context, key string) error
	extendLockFunc func(ctx context.Context, key string) error
}

func (m *mockLocker) TryLock(ctx context.Context, key string) (bool, error) {
	if m.tryLockFunc != nil {
		return m.tryLockFunc(ctx, key)
	}
	return true, nil
}

func (m *mockLocker) Unlock(ctx context.Context, key string) error {
	if m.unlockFunc != nil {
		return m.unlockFunc(ctx, key)
	}
	return nil
}

func (m *mockLocker) ExtendLock(ctx context.Context, key string) error {
	if m.extendLockFunc != nil {
		return m.extendLockFunc(ctx, key)
	}
	return nil
}

var _ = Describe("Host Controller", func() {
	var (
		reconciler    *HostReconciler
		mockInvClient *mockInventoryClient
		mockLock      *mockLocker
		mockK8sClient *mockClient
		testHost      *v1alpha1.Host
		testNamespace string
		testHostName  string
		poolUID       types.UID
	)

	// Common setup for ALL tests
	BeforeEach(func() {
		testNamespace = "default"
		mockInvClient = &mockInventoryClient{}
		mockLock = &mockLocker{}
		mockK8sClient = &mockClient{Client: k8sClient}

		reconciler = &HostReconciler{
			Client:          mockK8sClient,
			Scheme:          k8sClient.Scheme(),
			InventoryClient: mockInvClient,
			Locker:          mockLock,
		}
	})

	// Common cleanup for ALL tests
	AfterEach(func() {
		// Reset all mock functions
		mockK8sClient.updateFunc = nil
		mockK8sClient.deleteFunc = nil
		mockK8sClient.statusUpdateFunc = nil
		mockInvClient.findFreeHostFunc = nil
		mockInvClient.assignHostFunc = nil
		mockInvClient.unassignHostFunc = nil
		mockLock.tryLockFunc = nil
		mockLock.unlockFunc = nil
		mockLock.extendLockFunc = nil

		if testHostName != "" && testNamespace != "" {
			host := &v1alpha1.Host{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, host)
			if err == nil {
				// Remove finalizer and delete
				host.Finalizers = []string{}
				_ = k8sClient.Update(ctx, host)
				_ = k8sClient.Delete(ctx, host)
			}
		}
	})

	Context("When reconciling a completely new Host without finalizer", func() {
		BeforeEach(func() {
			testHostName = "test-host-new"
			poolUID = types.UID("01234567-89ab-cdef-0123-456789abcdef")

			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      testHostName,
					Namespace: testNamespace,
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
		})

		It("should add finalizer on first reconciliation", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Finalizers).To(ContainElement(HostInventoryFinalizer))
		})

		It("should handle finalizer update error", func() {
			// Mock Update to fail
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("update failed"))
		})
	})

	Context("When reconciling a new Host with finalizer", func() {
		BeforeEach(func() {
			testHostName = "test-host-with-finalizer"
			poolUID = types.UID("01234567-89ab-cdef-0123-456789abcdef")

			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
		})

		It("should find a free host from inventory and set status id", func() {
			mockInvClient.findFreeHostFunc = func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
				Expect(matchExpressions["hostClass"]).To(Equal("fc430"))
				Expect(matchExpressions["managedBy"]).To(Equal("baremetal"))
				Expect(matchExpressions["provisionState"]).To(Equal("available"))
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					Name:            "physical-host-1",
					ManagementClass: "inventory-class",
					HostClass:       "fc430",
				}, nil
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
		})

		It("should requeue when no free hosts are available", func() {
			mockInvClient.findFreeHostFunc = func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
				return nil, nil
			}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(NoFreeHostsRequeueFrequency))
		})

		It("should handle FindFreeHost error", func() {
			mockInvClient.findFreeHostFunc = func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
				return nil, errors.New("inventory service unavailable")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("inventory service unavailable"))
		})

		It("should handle status update error after finding host", func() {
			mockInvClient.findFreeHostFunc = func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					Name:            "physical-host-1",
					ManagementClass: "inventory-class",
					HostClass:       "fc430",
				}, nil
			}

			// Mock Status().Update to fail
			mockK8sClient.statusUpdateFunc = func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return errors.New("status update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("status update failed"))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(BeEmpty())
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})
	})

	Context("When reconciling a Host with ID but no ManagementClass", func() {
		BeforeEach(func() {
			testHostName = "test-host-with-id"
			poolUID = types.UID("test-pool-123")

			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
			retrieved := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostName, Namespace: testNamespace}, retrieved)).To(Succeed())
			retrieved.Spec.ID = "inv-host-123" // nolint
			Expect(k8sClient.Status().Patch(ctx, retrieved, client.Merge)).To(Succeed())
		})

		It("should acquire lock and assign the host", func() {
			lockAcquired := false
			lockReleased := false

			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				Expect(key).To(Equal("inv-host-123"))
				lockAcquired = true
				return true, nil
			}

			mockLock.unlockFunc = func(ctx context.Context, key string) error {
				Expect(key).To(Equal("inv-host-123"))
				lockReleased = true
				return nil
			}

			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				Expect(inventoryHostID).To(Equal("inv-host-123"))
				Expect(bareMetalPoolID).To(Equal("test-pool-123"))
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					ManagementClass: "ironic-mgmt",
				}, nil
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lockAcquired).To(BeTrue())
			Expect(lockReleased).To(BeTrue())

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(Equal("ironic-mgmt"))
		})

		It("should requeue when lock cannot be acquired", func() {
			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return false, nil
			}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(TryLockFailRequeueFrequency))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})

		It("should handle lock acquisition error", func() {
			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return false, errors.New("redis connection error")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("redis connection error"))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})

		It("should reset ID when host is already assigned to different CR", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return nil, nil
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(BeEmpty())
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})

		It("should handle AssignHost error", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return nil, errors.New("assignment failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("assignment failed"))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})

		It("should handle status update error when resetting ID for already assigned host", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return nil, nil // Host already assigned to different CR
			}

			// Mock Status().Update to fail when resetting ID
			mockK8sClient.statusUpdateFunc = func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return errors.New("status update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("status update failed"))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})

		It("should handle status update error when setting ManagementClass", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					ManagementClass: "ironic-mgmt",
				}, nil
			}

			// Mock Status().Update to fail when setting ManagementClass
			mockK8sClient.statusUpdateFunc = func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return errors.New("status update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("status update failed"))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(BeEmpty())
		})
	})

	Context("When reconciling a fully acquired Host", func() {
		BeforeEach(func() {
			testHostName = "test-host-acquired"
			poolUID = types.UID("test-pool-123")

			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      testHostName,
					Namespace: testNamespace,
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
			retrieved := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostName, Namespace: testNamespace}, retrieved)).To(Succeed())
			retrieved.Spec.ID = "inv-host-123"
			retrieved.Spec.HostManagementClass = "ironic-mgmt" //nolint
			Expect(k8sClient.Status().Patch(ctx, retrieved, client.Merge)).To(Succeed())
		})

		It("should do nothing when host is already acquired", func() {
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.Spec.ID).To(Equal("inv-host-123"))
			Expect(updatedHost.Spec.HostManagementClass).To(Equal("ironic-mgmt"))
		})
	})

	Context("When reconciling an orphaned Host", func() {
		BeforeEach(func() {
			testHostName = "test-host-orphaned"

			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels:     map[string]string{},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
		})

		It("should delete orphaned host without pool ID", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("should handle delete error for orphaned host", func() {
			// Mock Delete to fail
			mockK8sClient.deleteFunc = func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
				return errors.New("delete failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("delete failed"))

			updatedHost := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostName,
				Namespace: testNamespace,
			}, updatedHost)).To(Succeed())

			Expect(updatedHost.DeletionTimestamp.IsZero()).To(BeTrue())
		})
	})

	Context("When deleting a Host", func() {
		BeforeEach(func() {
			testHostName = "test-host-delete"
		})

		It("should unassign host from inventory and remove finalizer", func() {
			poolUID := types.UID("test-pool-123")
			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			unassignCalled := false
			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				Expect(inventoryHostID).To(Equal("inv-host-123"))
				unassignCalled = true
				return nil
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())

			testHost.Spec.ID = "inv-host-123"
			testHost.Spec.HostManagementClass = "ironic-mgmt"
			Expect(k8sClient.Status().Patch(ctx, testHost, client.Merge)).To(Succeed())

			Expect(k8sClient.Delete(ctx, testHost)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(unassignCalled).To(BeTrue())

			Eventually(func() bool {
				deletedHost := &v1alpha1.Host{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				}, deletedHost)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})

		It("should remove finalizer without unassigning if host was never assigned", func() {
			poolUID := types.UID("test-pool-123")
			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			unassignCalled := false
			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				unassignCalled = true
				return nil
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHost)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(unassignCalled).To(BeFalse())

			Eventually(func() bool {
				deletedHost := &v1alpha1.Host{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				}, deletedHost)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})

		It("should remove finalizer without unassigning if host has ID but no ManagementClass", func() {
			poolUID := types.UID("test-pool-123")
			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					ID:        "inv-host-123",
					HostClass: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			unassignCalled := false
			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				unassignCalled = true
				return nil
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
			retrieved := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostName, Namespace: testNamespace}, retrieved)).To(Succeed())
			retrieved.Spec.ID = "inv-host-123"
			Expect(k8sClient.Status().Patch(ctx, retrieved, client.Merge)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHost)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(unassignCalled).To(BeFalse())

			Eventually(func() bool {
				deletedHost := &v1alpha1.Host{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				}, deletedHost)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})

		It("should requeue when lock cannot be acquired during deletion", func() {
			poolUID := types.UID("test-pool-123")
			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					ID:                  "inv-host-123",
					HostClass:           "fc430",
					HostManagementClass: "ironic-mgmt",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return false, nil
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
			retrieved := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostName, Namespace: testNamespace}, retrieved)).To(Succeed())
			retrieved.Spec.ID = "inv-host-123"
			retrieved.Spec.HostManagementClass = "ironic-mgmt"
			Expect(k8sClient.Status().Patch(ctx, retrieved, client.Merge)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHost)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(TryLockFailRequeueFrequency))

			// test over, so now actually delete it
			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return true, nil
			}

			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle unassign error during deletion", func() {
			uniqueHostName := "test-host-delete-error"
			poolUID := types.UID("test-pool-123")
			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       uniqueHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					ID:                  "inv-host-123",
					HostClass:           "fc430",
					HostManagementClass: "ironic-mgmt",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
				},
			}

			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				return errors.New("unassignment failed")
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())
			retrieved := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uniqueHostName, Namespace: testNamespace}, retrieved)).To(Succeed())
			retrieved.Spec.ID = "inv-host-123"
			retrieved.Spec.HostManagementClass = "ironic-mgmt"
			Expect(k8sClient.Status().Patch(ctx, retrieved, client.Merge)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHost)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      uniqueHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unassignment failed"))

			// Manually remove finalizer since unassignment failed
			hostToClean := &v1alpha1.Host{}
			if k8sClient.Get(ctx, types.NamespacedName{Name: uniqueHostName, Namespace: testNamespace}, hostToClean) == nil {
				controllerutil.RemoveFinalizer(hostToClean, HostInventoryFinalizer)
				_ = k8sClient.Update(ctx, hostToClean)
			}
		})

		It("should handle finalizer removal error during deletion", func() {
			poolUID := types.UID("test-pool-123")
			trueVal := true
			testHost = &v1alpha1.Host{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "Host",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostName,
					Namespace:  testNamespace,
					Finalizers: []string{HostInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion:         "osac.openshift.io/v1alpha1",
							Kind:               "BareMetalPool",
							Name:               "test-pool",
							UID:                poolUID,
							Controller:         &trueVal,
							BlockOwnerDeletion: &trueVal,
						},
					},
				},
				Spec: v1alpha1.HostSpec{
					ID:                  "inv-host-123",
					HostClass:           "fc430",
					HostManagementClass: "ironic-mgmt",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
				},
			}

			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				return nil
			}

			Expect(k8sClient.Create(ctx, testHost)).To(Succeed())

			// Set status
			retrieved := &v1alpha1.Host{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostName, Namespace: testNamespace}, retrieved)).To(Succeed())
			retrieved.Spec.ID = "inv-host-123"
			retrieved.Spec.HostManagementClass = "ironic-mgmt"
			Expect(k8sClient.Status().Patch(ctx, retrieved, client.Merge)).To(Succeed())

			// Delete the host
			Expect(k8sClient.Delete(ctx, testHost)).To(Succeed())

			// Mock Update to fail when removing finalizer
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("finalizer removal failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("finalizer removal failed"))

			// Manually clean up since finalizer removal failed
			hostToClean := &v1alpha1.Host{}
			if k8sClient.Get(ctx, types.NamespacedName{Name: testHostName, Namespace: testNamespace}, hostToClean) == nil {
				controllerutil.RemoveFinalizer(hostToClean, HostInventoryFinalizer)
				_ = k8sClient.Update(ctx, hostToClean)
			}
		})
	})

	Context("When host resource doesn't exist", func() {
		It("should handle not found error gracefully", func() {
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "non-existent-host",
					Namespace: "test-namespace",
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})
})
