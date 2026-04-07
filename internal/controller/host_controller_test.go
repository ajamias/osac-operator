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

var _ = Describe("HostLease Controller", func() {
	var (
		reconciler        *HostLeaseReconciler
		mockInvClient     *mockInventoryClient
		mockLock          *mockLocker
		mockK8sClient     *mockClient
		testHostLease     *v1alpha1.HostLease
		testNamespace     string
		testHostLeaseName string
		testPool          *v1alpha1.BareMetalPool
		testPoolName      string
	)

	// Common setup for ALL tests
	BeforeEach(func() {
		testNamespace = "default"
		testPoolName = "test-pool-for-host-tests"
		mockInvClient = &mockInventoryClient{}
		mockLock = &mockLocker{}
		mockK8sClient = &mockClient{Client: k8sClient}

		// Create a real BareMetalPool for owner references
		testPool = &v1alpha1.BareMetalPool{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "osac.openshift.io/v1alpha1",
				Kind:       "BareMetalPool",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPoolName,
				Namespace: testNamespace,
			},
			Spec: v1alpha1.BareMetalPoolSpec{
				HostSets: []v1alpha1.BareMetalHostSet{
					{
						HostType: "fc430",
						Replicas: 1,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, testPool)).To(Succeed())

		// Retrieve the pool to get the UID assigned by Kubernetes
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      testPoolName,
			Namespace: testNamespace,
		}, testPool)).To(Succeed())

		reconciler = &HostLeaseReconciler{
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

		if testHostLeaseName != "" && testNamespace != "" {
			hostLease := &v1alpha1.HostLease{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, hostLease)
			if err == nil {
				// Remove finalizer and delete
				hostLease.Finalizers = []string{}
				_ = k8sClient.Update(ctx, hostLease)
				_ = k8sClient.Delete(ctx, hostLease)
			}
		}

		// Clean up the BareMetalPool
		if testPoolName != "" && testNamespace != "" {
			pool := &v1alpha1.BareMetalPool{}
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      testPoolName,
				Namespace: testNamespace,
			}, pool)
			if err == nil {
				pool.Finalizers = []string{}
				_ = k8sClient.Update(ctx, pool)
				_ = k8sClient.Delete(ctx, pool)
			}
		}
	})

	Context("When reconciling a completely new HostLease without finalizer", func() {
		BeforeEach(func() {
			testHostLeaseName = "test-host-new"

			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
					Labels: map[string]string{
						"pool-id": string(testPool.UID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType:     "fc430",
					ExternalID:   "",
					ExternalName: "test-host",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
		})

		It("should add finalizer on first reconciliation", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Finalizers).To(ContainElement(HostLeaseInventoryFinalizer))
		})

		It("should handle finalizer update error", func() {
			// Mock Update to fail
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("update failed"))
		})
	})

	Context("When reconciling a new HostLease with finalizer", func() {
		BeforeEach(func() {
			testHostLeaseName = "test-host-with-finalizer"

			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(testPool.UID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType:     "fc430",
					ExternalID:   "",
					ExternalName: "test-host",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
		})

		It("should find a free host from inventory and set status id", func() {
			mockInvClient.findFreeHostFunc = func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
				Expect(matchExpressions["hostType"]).To(Equal("fc430"))
				Expect(matchExpressions["managedBy"]).To(Equal("baremetal"))
				Expect(matchExpressions["provisionState"]).To(Equal("available"))
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					Name:            "physical-host-1",
					HostType:        "fc430",
					HostClass:       "inventory-class",
				}, nil
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
		})

		It("should requeue when no free hosts are available", func() {
			mockInvClient.findFreeHostFunc = func(ctx context.Context, matchExpressions map[string]string) (*inventory.Host, error) {
				return nil, nil
			}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
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
					Name:      testHostLeaseName,
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
					HostType:        "fc430",
					HostClass:       "inventory-class",
				}, nil
			}

			// Mock Status().Update to fail
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("status update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("status update failed"))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(BeEmpty())
			Expect(updatedHostLease.Spec.HostClass).To(BeEmpty())
		})
	})

	Context("When reconciling a HostLease with ExternalID but no HostClass", func() {
		BeforeEach(func() {
			testHostLeaseName = "test-host-with-id"

			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(testPool.UID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
			retrievedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostLeaseName, Namespace: testNamespace}, retrievedHostLease)).To(Succeed())
			retrievedHostLease.Spec.ExternalID = "inv-host-123" // nolint
			Expect(k8sClient.Update(ctx, retrievedHostLease)).To(Succeed())
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
				Expect(bareMetalPoolID).To(Equal(string(testPool.UID)))
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					HostClass:       "ironic-mgmt",
				}, nil
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(lockAcquired).To(BeTrue())
			Expect(lockReleased).To(BeTrue())

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(Equal("ironic-mgmt"))
		})

		It("should requeue when lock cannot be acquired", func() {
			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return false, nil
			}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(TryLockFailRequeueFrequency))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(BeEmpty())
		})

		It("should handle lock acquisition error", func() {
			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return false, errors.New("redis connection error")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("redis connection error"))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(BeEmpty())
		})

		It("should delete host when it is already assigned to different CR", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return nil, nil
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Host should be deleted since it was assigned to a different CR
			deletedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, deletedHostLease)).To(Succeed())

			Expect(deletedHostLease.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("should handle AssignHost error", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return nil, errors.New("assignment failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("assignment failed"))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(BeEmpty())
		})

		It("should handle delete error when host is already assigned to different CR", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return nil, nil // Host already assigned to different CR
			}

			// Mock Delete to fail
			mockK8sClient.deleteFunc = func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
				return errors.New("delete failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("delete failed"))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(BeEmpty())
		})

		It("should handle status update error when setting HostClass", func() {
			mockInvClient.assignHostFunc = func(ctx context.Context, inventoryHostID string, bareMetalPoolID string, bareMetalPoolHostID string, labels map[string]string) (*inventory.Host, error) {
				return &inventory.Host{
					InventoryHostID: "inv-host-123",
					HostClass:       "ironic-mgmt",
				}, nil
			}

			// Mock Status().Update to fail when setting HostClass
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("status update failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("status update failed"))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(BeEmpty())
		})
	})

	Context("When reconciling a fully acquired HostLease", func() {
		BeforeEach(func() {
			testHostLeaseName = "test-host-acquired"

			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
					Labels: map[string]string{
						"pool-id": string(testPool.UID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
			retrievedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostLeaseName, Namespace: testNamespace}, retrievedHostLease)).To(Succeed())
			retrievedHostLease.Spec.ExternalID = "inv-host-123"
			retrievedHostLease.Spec.HostClass = "ironic-mgmt" //nolint
			Expect(k8sClient.Update(ctx, retrievedHostLease)).To(Succeed())
		})

		It("should do nothing when host is already acquired", func() {
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeZero())

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.Spec.ExternalID).To(Equal("inv-host-123"))
			Expect(updatedHostLease.Spec.HostClass).To(Equal("ironic-mgmt"))
		})
	})

	Context("When reconciling an orphaned HostLease", func() {
		BeforeEach(func() {
			testHostLeaseName = "test-host-orphaned"

			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels:     map[string]string{},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
		})

		It("should delete orphaned host without pool ID", func() {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.DeletionTimestamp.IsZero()).To(BeFalse())
		})

		It("should handle delete error for orphaned host", func() {
			// Mock Delete to fail
			mockK8sClient.deleteFunc = func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
				return errors.New("delete failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("delete failed"))

			updatedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      testHostLeaseName,
				Namespace: testNamespace,
			}, updatedHostLease)).To(Succeed())

			Expect(updatedHostLease.DeletionTimestamp.IsZero()).To(BeTrue())
		})
	})

	Context("When deleting a HostLease", func() {
		BeforeEach(func() {
			testHostLeaseName = "test-host-delete"
		})

		It("should unassign host from inventory and remove finalizer", func() {
			poolUID := testPool.UID
			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			unassignCalled := false
			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				Expect(inventoryHostID).To(Equal("inv-host-123"))
				unassignCalled = true
				return nil
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())

			testHostLease.Spec.ExternalID = "inv-host-123"
			testHostLease.Spec.HostClass = "ironic-mgmt"
			Expect(k8sClient.Update(ctx, testHostLease)).To(Succeed())

			Expect(k8sClient.Delete(ctx, testHostLease)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(unassignCalled).To(BeTrue())

			Eventually(func() bool {
				deletedHostLease := &v1alpha1.HostLease{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				}, deletedHostLease)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})

		It("should remove finalizer without unassigning if host was never assigned", func() {
			poolUID := testPool.UID
			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					HostType: "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			unassignCalled := false
			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				unassignCalled = true
				return nil
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHostLease)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(unassignCalled).To(BeFalse())

			Eventually(func() bool {
				deletedHostLease := &v1alpha1.HostLease{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				}, deletedHostLease)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})

		It("should remove finalizer without unassigning if host lease has ExternalID but no HostClass", func() {
			poolUID := testPool.UID
			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "inv-host-123",
					HostType:   "fc430",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			unassignCalled := false
			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				unassignCalled = true
				return nil
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
			retrievedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostLeaseName, Namespace: testNamespace}, retrievedHostLease)).To(Succeed())
			retrievedHostLease.Spec.ExternalID = "inv-host-123"
			Expect(k8sClient.Update(ctx, retrievedHostLease)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHostLease)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(unassignCalled).To(BeFalse())

			Eventually(func() bool {
				deletedHostLease := &v1alpha1.HostLease{}
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				}, deletedHostLease)
				return apierrors.IsNotFound(err)
			}, 5*time.Second).Should(BeTrue())
		})

		It("should requeue when lock cannot be acquired during deletion", func() {
			poolUID := testPool.UID
			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "inv-host-123",
					HostType:   "fc430",
					HostClass:  "ironic-mgmt",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			mockLock.tryLockFunc = func(ctx context.Context, key string) (bool, error) {
				return false, nil
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
			retrievedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostLeaseName, Namespace: testNamespace}, retrievedHostLease)).To(Succeed())
			retrievedHostLease.Spec.ExternalID = "inv-host-123"
			retrievedHostLease.Spec.HostClass = "ironic-mgmt"
			Expect(k8sClient.Update(ctx, retrievedHostLease)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHostLease)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
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
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle unassign error during deletion", func() {
			uniqueHostLeaseName := "test-host-delete-error"
			poolUID := testPool.UID
			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       uniqueHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "inv-host-123",
					HostType:   "fc430",
					HostClass:  "ironic-mgmt",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "ironic",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				return errors.New("unassignment failed")
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())
			retrievedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: uniqueHostLeaseName, Namespace: testNamespace}, retrievedHostLease)).To(Succeed())
			retrievedHostLease.Spec.ExternalID = "inv-host-123"
			retrievedHostLease.Spec.HostClass = "ironic-mgmt"
			Expect(k8sClient.Update(ctx, retrievedHostLease)).To(Succeed())
			Expect(k8sClient.Delete(ctx, testHostLease)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      uniqueHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unassignment failed"))

			// Manually remove finalizer since unassignment failed
			hostLeaseToClean := &v1alpha1.HostLease{}
			if k8sClient.Get(ctx, types.NamespacedName{Name: uniqueHostLeaseName, Namespace: testNamespace}, hostLeaseToClean) == nil {
				controllerutil.RemoveFinalizer(hostLeaseToClean, HostLeaseInventoryFinalizer)
				_ = k8sClient.Update(ctx, hostLeaseToClean)
			}
		})

		It("should handle finalizer removal error during deletion", func() {
			poolUID := testPool.UID
			testHostLease = &v1alpha1.HostLease{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "osac.openshift.io/v1alpha1",
					Kind:       "HostLease",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:       testHostLeaseName,
					Namespace:  testNamespace,
					Finalizers: []string{HostLeaseInventoryFinalizer},
					Labels: map[string]string{
						"pool-id": string(poolUID),
					},
				},
				Spec: v1alpha1.HostLeaseSpec{
					ExternalID: "inv-host-123",
					HostType:   "fc430",
					HostClass:  "ironic-mgmt",
					Selector: v1alpha1.HostSelectorSpec{
						HostSelector: map[string]string{
							"managedBy":      "baremetal",
							"provisionState": "available",
						},
					},
					TemplateID: "default",
					PoweredOn:  false,
				},
			}

			mockInvClient.unassignHostFunc = func(ctx context.Context, inventoryHostID string, labels []string) error {
				return nil
			}
			Expect(controllerutil.SetControllerReference(testPool, testHostLease, k8sClient.Scheme())).NotTo(HaveOccurred())

			Expect(k8sClient.Create(ctx, testHostLease)).To(Succeed())

			// Set status
			retrievedHostLease := &v1alpha1.HostLease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testHostLeaseName, Namespace: testNamespace}, retrievedHostLease)).To(Succeed())
			retrievedHostLease.Spec.ExternalID = "inv-host-123"
			retrievedHostLease.Spec.HostClass = "ironic-mgmt"
			Expect(k8sClient.Update(ctx, retrievedHostLease)).To(Succeed())

			// Delete the host
			Expect(k8sClient.Delete(ctx, testHostLease)).To(Succeed())

			// Mock Update to fail when removing finalizer
			mockK8sClient.updateFunc = func(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("finalizer removal failed")
			}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      testHostLeaseName,
					Namespace: testNamespace,
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("finalizer removal failed"))

			// Manually clean up since finalizer removal failed
			hostLeaseToClean := &v1alpha1.HostLease{}
			if k8sClient.Get(ctx, types.NamespacedName{Name: testHostLeaseName, Namespace: testNamespace}, hostLeaseToClean) == nil {
				controllerutil.RemoveFinalizer(hostLeaseToClean, HostLeaseInventoryFinalizer)
				_ = k8sClient.Update(ctx, hostLeaseToClean)
			}
		})
	})

	Context("When host lease resource doesn't exist", func() {
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
