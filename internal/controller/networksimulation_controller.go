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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	simv1alpha1 "github.com/Haakon89/netsim-operator/api/v1alpha1"
)

const (
	simFinalizer = "sim.example.local/finalizer"
	simLabelKey  = "sim.example.local/name"
	roleLabelKey = "sim.example.local/role"
)

// NetworkSimulationReconciler reconciles a NetworkSimulation object
type NetworkSimulationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sim.example.local,resources=networksimulations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sim.example.local,resources=networksimulations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sim.example.local,resources=networksimulations/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the NetworkSimulation object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/reconcile
func (r *NetworkSimulationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sim simv1alpha1.NetworkSimulation
	if err := r.Get(ctx, req.NamespacedName, &sim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 1) Handle deletion first (finalizer path)
	if !sim.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&sim, simFinalizer) {
			// Delete the simulation namespace (if we created one)
			if sim.Status.Namespace != "" {
				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sim.Status.Namespace}}
				if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "Failed to delete simulation namespace", "namespace", sim.Status.Namespace)
					return ctrl.Result{}, err
				}
				logger.Info("Requested deletion of simulation namespace", "namespace", sim.Status.Namespace)
			}

			// Remove finalizer so the CR can be deleted
			controllerutil.RemoveFinalizer(&sim, simFinalizer)
			if err := r.Update(ctx, &sim); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 2) Ensure finalizer exists
	if !controllerutil.ContainsFinalizer(&sim, simFinalizer) {
		controllerutil.AddFinalizer(&sim, simFinalizer)
		if err := r.Update(ctx, &sim); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	}

	// 3) Decide namespace name (simple deterministic naming for MVP)
	simNS := sim.Status.Namespace
	if simNS == "" {
		// Option A: human-readable (may collide if names reused)
		// simNS = fmt.Sprintf("netsim-%s", sim.Name)

		// Option B (safer): include short uid to avoid collisions
		uid := string(sim.UID)
		short := uid
		if len(uid) > 8 {
			short = uid[:8]
		}
		simNS = fmt.Sprintf("netsim-%s-%s", sim.Name, short)
	}

	// 4) Ensure namespace exists
	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: simNS}, &ns)
	if err != nil {
		if apierrors.IsNotFound(err) {
			ns = corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: simNS,
					Labels: map[string]string{
						"sim.example.local/owner": sim.Name,
					},
				},
			}
			if err := r.Create(ctx, &ns); err != nil {
				logger.Error(err, "Failed to create simulation namespace", "namespace", simNS)
				return ctrl.Result{}, err
			}
			logger.Info("Created simulation namespace", "namespace", simNS)
		} else {
			return ctrl.Result{}, err
		}
	}

	// 5) Update status (phase + namespace)
	// Only update if needed to avoid hot loops.
	desiredPhase := sim.Status.Phase
	if desiredPhase == "" {
		desiredPhase = "Pending"
	}
	if sim.Status.Namespace != simNS || sim.Status.Phase != desiredPhase {
		sim.Status.Namespace = simNS
		sim.Status.Phase = desiredPhase
		if err := r.Status().Update(ctx, &sim); err != nil {
			logger.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		logger.Info("Updated status", "phase", sim.Status.Phase, "namespace", sim.Status.Namespace)

	}

	// 6) Ensure device pods exist
	desired := sim.Spec.DeviceCount
	if desired < 0 {
		desired = 0
	}
	for i := 0; i < desired; i++ {
		podName := fmt.Sprintf("device-%d", i)

		var pod corev1.Pod
		err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: simNS}, &pod)
		if err != nil {
			if apierrors.IsNotFound(err) {
				newPod := corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: simNS,
						Labels: map[string]string{
							simLabelKey:  sim.Name,
							roleLabelKey: "device",
						},
					},
					Spec: corev1.PodSpec{
						Hostname:  podName,
						Subdomain: "devices",
						Containers: []corev1.Container{
							{
								Name:    "device",
								Image:   "ghcr.io/nicolaka/netshoot:latest",
								Command: []string{"sh", "-c", "sleep 365d"},
							},
						},
						RestartPolicy: corev1.RestartPolicyAlways,
					},
				}

				// Optional: set owner reference so garbage collection cleans pods
				// NOTE: Pods are in a different namespace than the CR, so OwnerReferences
				// do NOT work across namespaces. We'll rely on namespace deletion instead.

				if err := r.Create(ctx, &newPod); err != nil {
					logger.Error(err, "Failed to create device pod", "pod", podName, "namespace", simNS)
					return ctrl.Result{}, err
				}
			}
		}
	}

	// 6.5) Ensure headless service exists
	svcName := "devices"
	var svc corev1.Service
	err = r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: simNS}, &svc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			svc = corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      svcName,
					Namespace: simNS,
					Labels: map[string]string{
						simLabelKey: sim.Name,
					},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: corev1.ClusterIPNone, // headless
					Selector: map[string]string{
						roleLabelKey: "device",
					},
					PublishNotReadyAddresses: true, // optional; helps if you want DNS before Ready
				},
			}
			if err := r.Create(ctx, &svc); err != nil {
				if apierrors.IsAlreadyExists(err) {
					//fine
				} else {
					logger.Error(err, "Failed to create headless service", "service", svcName, "namespace", simNS)
					return ctrl.Result{}, err
				}
			}
			logger.Info("Created headless service", "service", svcName, "namespace", simNS)
		} else {
			return ctrl.Result{}, err
		}
	}
	// 7) Check readiness of device pods
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(simNS), client.MatchingLabels{roleLabelKey: "device"}); err != nil {
		return ctrl.Result{}, err
	}

	readyCount := 0
	for _, p := range podList.Items {
		if isPodReady(&p) {
			readyCount++
		}
	}

	newPhase := sim.Status.Phase
	if newPhase == "" {
		newPhase = "Pending"
	}
	if desired > 0 {
		newPhase = "Running"
		if readyCount == desired {
			newPhase = "Ready"
		}
	}

	// Update status if changed (phase + namespace)
	if sim.Status.Namespace != simNS || sim.Status.Phase != newPhase {
		sim.Status.Namespace = simNS
		sim.Status.Phase = newPhase
		if err := r.Status().Update(ctx, &sim); err != nil {
			if apierrors.IsConflict(err) {
				logger.Info("Status update conflict, requeueing")
				return ctrl.Result{Requeue: true}, nil
			}
			logger.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
		logger.Info("Updated status", "phase", sim.Status.Phase, "namespace", sim.Status.Namespace, "ready", readyCount, "desired", desired)
	}

	// If not ready yet, requeue soon
	if desired > 0 && readyCount < desired {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	script := buildTrafficScripts(&sim)
	jobName := trafficJobName(&sim)
	backoff := int32(0)

	if sim.Status.TrafficJobName != jobName {
		sim.Status.TrafficJobName = jobName
		if err := r.Status().Update(ctx, &sim); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: 5}, nil
			}
			logger.Error(err, "Failed to update traffic job name in status")
			return ctrl.Result{}, err
		}
	}

	var job batchv1.Job
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: simNS}, &job)
	if err != nil {
		if apierrors.IsNotFound(err) {
			job = batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      jobName,
					Namespace: simNS,
					Labels: map[string]string{
						simLabelKey: sim.Name,
					},
				},
				Spec: batchv1.JobSpec{
					BackoffLimit: &backoff,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{
								{
									Name:    "traffick",
									Image:   "ghcr.io/nicolaka/netshoot:latest",
									Command: []string{"sh", "-c", script},
								},
							},
						},
					},
				},
			}
			if err := r.Create(ctx, &job); err != nil {
				if apierrors.IsAlreadyExists(err) {
					logger.Info("Traffic job already exists", "job", jobName, "namespace", simNS)
				} else {
					logger.Error(err, "Failed to create traffic job", "job", jobName, "namespace", simNS)
					return ctrl.Result{}, err
				}
			} else {
				logger.Info("Created traffic job", "job", jobName, "namespace", simNS)
			}
		} else {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func buildTrafficScripts(sim *simv1alpha1.NetworkSimulation) string {
	var commands []string
	for _, step := range sim.Spec.Traffic.Steps {
		switch step.Type {
		case "ping":
			count := step.Count
			if count == 0 {
				count = 3
			}

			commands = append(commands,
				fmt.Sprintf(
					`echo "Pinging %s from %s"; ping -c %d %s`,
					step.To,
					step.From,
					count,
					step.To,
				),
			)
		default:
			commands = append(commands,
				fmt.Sprintf(`echo "Skipping unsupported traffic type: %s"`, step.Type),
			)
		}
	}

	if len(commands) == 0 {
		return `echo "No traffic steps defined"`
	}
	return strings.Join(commands, "; ")
}

func trafficJobName(sim *simv1alpha1.NetworkSimulation) string {
	script := buildTrafficScripts(sim)

	sum := sha256.Sum256([]byte(script))
	hash := hex.EncodeToString(sum[:])[:8]

	return fmt.Sprintf("traffic.%s", hash)
}

func isPodReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkSimulationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simv1alpha1.NetworkSimulation{}).
		Named("networksimulation").
		Complete(r)
}
