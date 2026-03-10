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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	simv1alpha1 "github.com/Haakon89/netsim-operator/api/v1alpha1"
)

var _ = Describe("NetworkSimulation Controller", func() {
	Context("When reconciling a resource", func() {
		var resourceName string
		var typeNamespacedName types.NamespacedName

		BeforeEach(func() {
			resourceName = fmt.Sprintf("test-resource-%d", time.Now().UnixNano())
			typeNamespacedName = types.NamespacedName{
				Name:      resourceName,
				Namespace: "default",
			}
		})

		AfterEach(func() {
			resource := &simv1alpha1.NetworkSimulation{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				By("Cleanup the specific resource instance NetworkSimulation")
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
		})
		It("adds the finalizer on first reconcile", func() {
			resource := &simv1alpha1.NetworkSimulation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			controllerReconciler := &NetworkSimulationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &simv1alpha1.NetworkSimulation{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(simFinalizer))
		})
		It("creates namespace, service, and device pods", func() {
			resource := &simv1alpha1.NetworkSimulation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: simv1alpha1.NetworkSimulationSpec{
					DeviceCount: 2,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := &NetworkSimulationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			updated := &simv1alpha1.NetworkSimulation{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Namespace).NotTo(BeEmpty())

			simNS := updated.Status.Namespace

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: simNS}, ns)).To(Succeed())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "devices",
				Namespace: simNS,
			}, svc)).To(Succeed())
			Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))

			podList := &corev1.PodList{}
			Expect(k8sClient.List(ctx, podList, client.InNamespace(simNS))).To(Succeed())

			devicePods := 0
			for _, pod := range podList.Items {
				if pod.Labels[roleLabelKey] == "device" {
					devicePods++
				}
			}
			Expect(devicePods).To(Equal(2))
		})
		It("updates status and creates a traffic job", func() {
			resource := &simv1alpha1.NetworkSimulation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: simv1alpha1.NetworkSimulationSpec{
					DeviceCount: 1,
					Traffic: simv1alpha1.Traffic{
						Steps: []simv1alpha1.TrafficStep{
							{
								Type:  "ping",
								From:  "device-0.devices",
								To:    "device-0.devices",
								Count: 1,
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			controllerReconciler := &NetworkSimulationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			updated := &simv1alpha1.NetworkSimulation{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())

			Expect(updated.Status.Namespace).NotTo(BeEmpty())
			Expect(updated.Status.Phase).To(Or(
				Equal(PhasePending),
				Equal(PhaseRunning),
				Equal(PhaseReady),
			))
			Expect(updated.Status.TrafficJobName).NotTo(BeEmpty())

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      updated.Status.TrafficJobName,
				Namespace: updated.Status.Namespace,
			}, job)).To(Succeed())
		})
	})
})
