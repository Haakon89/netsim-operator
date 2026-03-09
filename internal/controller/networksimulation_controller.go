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
	"github.com/go-logr/logr"
)

const (
	simFinalizer = "sim.example.local/finalizer"
	simLabelKey  = "sim.example.local/name"
	roleLabelKey = "sim.example.local/role"
	PhasePending = "Pending"
	PhaseRunning = "Running"
	PhaseReady   = "Ready"
	RequeueTimer = 2 * time.Second
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
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create

func (r *NetworkSimulationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sim simv1alpha1.NetworkSimulation
	if err := r.Get(ctx, req.NamespacedName, &sim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if handled, result, err := r.handleDeletion(ctx, logger, &sim); handled {
		return result, err
	}
	if handled, result, err := r.ensureFinalizer(ctx, &sim); handled {
		return result, err
	}

	simNS := generateNamespace(&sim)

	if err := r.reconcileInfrastructure(ctx, logger, &sim, simNS); err != nil {
		return ctrl.Result{}, err
	}

	phase, readyCount, result, err := r.reconcileReadiness(ctx, &sim, simNS)
	if err != nil || !result.IsZero() {
		return result, err
	}

	jobName, err := r.reconcileTraffic(ctx, logger, &sim, simNS)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, logger, &sim, simNS, phase, jobName); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciled simulation", "namespace", simNS, "phase", phase, "ready", readyCount)
	return ctrl.Result{}, nil
}

func (r *NetworkSimulationReconciler) handleDeletion(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
) (bool, ctrl.Result, error) {
	if sim.DeletionTimestamp.IsZero() {
		return false, ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(sim, simFinalizer) {
		return true, ctrl.Result{}, nil
	}

	if sim.Status.Namespace != "" {
		var ns corev1.Namespace
		err := r.Get(ctx, types.NamespacedName{Name: sim.Status.Namespace}, &ns)
		if err == nil {
			if ns.DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "Failed to delete simulation namespace", "namespace", sim.Status.Namespace)
					return true, ctrl.Result{}, err
				}
				logger.Info("Requested deletion of simulation namespace", "namespace", sim.Status.Namespace)
			}

			return true, ctrl.Result{RequeueAfter: RequeueTimer}, nil
		}

		if !apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(sim, simFinalizer)
	if err := r.Update(ctx, sim); err != nil {
		return true, ctrl.Result{}, err
	}

	return true, ctrl.Result{}, nil
}

func (r *NetworkSimulationReconciler) ensureFinalizer(
	ctx context.Context,
	sim *simv1alpha1.NetworkSimulation,
) (bool, ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(sim, simFinalizer) {
		return false, ctrl.Result{}, nil
	}

	controllerutil.AddFinalizer(sim, simFinalizer)
	if err := r.Update(ctx, sim); err != nil {
		return true, ctrl.Result{}, err
	}

	return true, ctrl.Result{RequeueAfter: RequeueTimer}, nil
}

func generateNamespace(sim *simv1alpha1.NetworkSimulation) string {
	if sim.Status.Namespace != "" {
		return sim.Status.Namespace
	}

	uid := string(sim.UID)
	short := uid
	if len(uid) > 8 {
		short = uid[:8]
	}

	name := strings.ToLower(sim.Name)
	ns := fmt.Sprintf("netsim-%s-%s", name, short)
	if len(ns) > 63 {
		ns = ns[:63]
	}
	return strings.TrimRight(ns, "-")
}

func (r *NetworkSimulationReconciler) reconcileInfrastructure(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
) error {

	if err := r.ensureNamespace(ctx, logger, sim, namespace); err != nil {
		return err
	}

	if err := r.ensureDevicePods(ctx, logger, sim, namespace); err != nil {
		return err
	}

	if err := r.ensureDeviceService(ctx, logger, sim, namespace); err != nil {
		return err
	}

	return nil
}

func (r *NetworkSimulationReconciler) ensureNamespace(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
) error {
	var ns corev1.Namespace
	err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	ns = corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
			Labels: map[string]string{
				"sim.example.local/owner": sim.Name,
			},
		},
	}

	if err := r.Create(ctx, &ns); err != nil {
		logger.Error(err, "Failed to create simulation namespace", "namespace", namespace)
		return err
	}

	logger.Info("Created simulation namespace", "namespace", namespace)
	return nil
}

func (r *NetworkSimulationReconciler) ensureDevicePods(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
) error {
	desired := desiredDeviceCount(sim)

	for i := 0; i < desired; i++ {
		podName := devicePodName(i)
		if err := r.ensureDevicePod(ctx, logger, sim, namespace, podName); err != nil {
			return err
		}
	}
	return nil
}

func (r *NetworkSimulationReconciler) ensureDeviceService(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
) error {
	const svcName = "devices"

	var svc corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: namespace}, &svc)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	svc = corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: namespace,
			Labels: map[string]string{
				simLabelKey: sim.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector: map[string]string{
				roleLabelKey: "device",
			},
			PublishNotReadyAddresses: true,
		},
	}

	if err := r.Create(ctx, &svc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		logger.Error(err, "Failed to create headless service", "service", svcName, "namespace", namespace)
		return err
	}

	logger.Info("Created headless service", "service", svcName, "namespace", namespace)
	return nil
}

func (r *NetworkSimulationReconciler) reconcileReadiness(
	ctx context.Context,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
) (phase string, readyCount int, result ctrl.Result, err error) {
	desired := desiredDeviceCount(sim)

	readyCount, err = r.countReadyDevicePods(ctx, namespace)
	if err != nil {
		return "", 0, ctrl.Result{}, err
	}

	phase = calculateSimulationPhase(sim.Status.Phase, desired, readyCount)

	if desired > 0 && readyCount < desired {
		result = ctrl.Result{RequeueAfter: RequeueTimer}
	}

	return phase, readyCount, result, nil
}

func desiredDeviceCount(sim *simv1alpha1.NetworkSimulation) int {
	if sim.Spec.DeviceCount < 0 {
		return 0
	}
	return sim.Spec.DeviceCount
}

func (r *NetworkSimulationReconciler) countReadyDevicePods(
	ctx context.Context,
	namespace string,
) (int, error) {
	var podList corev1.PodList
	if err := r.List(
		ctx,
		&podList,
		client.InNamespace(namespace),
		client.MatchingLabels{roleLabelKey: "device"},
	); err != nil {
		return 0, err
	}

	readyCount := 0
	for _, p := range podList.Items {
		if isPodReady(&p) {
			readyCount++
		}
	}

	return readyCount, nil
}

func calculateSimulationPhase(currentPhase string, desired, ready int) string {
	phase := currentPhase
	if phase == "" {
		phase = PhasePending
	}

	if desired > 0 {
		phase = PhaseRunning
		if ready == desired {
			phase = PhaseReady
		}
	}

	return phase
}

func (r *NetworkSimulationReconciler) reconcileTraffic(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
) (string, error) {

	script := buildTrafficScripts(sim)
	jobName := trafficJobName(sim)

	if err := r.ensureTrafficJob(ctx, logger, sim, namespace, jobName, script); err != nil {
		return "", err
	}

	return jobName, nil
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

func (r *NetworkSimulationReconciler) ensureTrafficJob(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
	jobName string,
	script string,
) error {
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, &job)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	job = buildTrafficJob(sim, namespace, jobName, script)

	if err := r.Create(ctx, &job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			logger.Info("Traffic job already exists", "job", jobName, "namespace", namespace)
			return nil
		}
		logger.Error(err, "Failed to create traffic job", "job", jobName, "namespace", namespace)
		return err
	}

	logger.Info("Created traffic job", "job", jobName, "namespace", namespace)
	return nil
}

func devicePodName(i int) string {
	return fmt.Sprintf("device-%d", i)
}

func buildDevicePod(sim *simv1alpha1.NetworkSimulation, namespace, podName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
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
}

func (r *NetworkSimulationReconciler) ensureDevicePod(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace, podName string,
) error {
	var pod corev1.Pod
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return err
	}

	newPod := buildDevicePod(sim, namespace, podName)

	if err := r.Create(ctx, &newPod); err != nil {
		logger.Error(err, "Failed to create device pod", "pod", podName, "namespace", namespace)
		return err
	}

	logger.Info("Created device pod", "pod", podName, "namespace", namespace)
	return nil
}

func (r *NetworkSimulationReconciler) reconcileStatus(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
	phase string,
	trafficJobName string,
) error {
	changed := false

	if sim.Status.Namespace != namespace {
		sim.Status.Namespace = namespace
		changed = true
	}
	if sim.Status.Phase != phase {
		sim.Status.Phase = phase
		changed = true
	}
	if sim.Status.TrafficJobName != trafficJobName {
		sim.Status.TrafficJobName = trafficJobName
		changed = true
	}

	if !changed {
		return nil
	}

	if err := r.Status().Update(ctx, sim); err != nil {
		logger.Error(err, "Failed to update status")
		return err
	}

	logger.Info("Updated simulation status",
		"namespace", sim.Status.Namespace,
		"phase", sim.Status.Phase,
		"trafficJobName", sim.Status.TrafficJobName,
	)
	return nil
}

func (r *NetworkSimulationReconciler) updateSimulationStatus(
	ctx context.Context,
	logger logr.Logger,
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
	phase string,
) error {
	if sim.Status.Namespace == namespace && sim.Status.Phase == phase {
		return nil
	}

	sim.Status.Namespace = namespace
	sim.Status.Phase = phase

	if err := r.Status().Update(ctx, sim); err != nil {
		logger.Error(err, "Failed to update status")
		return err
	}

	logger.Info("Updated status",
		"phase", sim.Status.Phase,
		"namespace", sim.Status.Namespace,
	)

	return nil
}

func buildTrafficJob(
	sim *simv1alpha1.NetworkSimulation,
	namespace string,
	jobName string,
	script string,
) batchv1.Job {
	backoff := int32(0)

	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
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
							Name:    "traffic",
							Image:   "ghcr.io/nicolaka/netshoot:latest",
							Command: []string{"sh", "-c", script},
						},
					},
				},
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkSimulationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simv1alpha1.NetworkSimulation{}).
		Named("networksimulation").
		Complete(r)
}
