/*
Copyright 2023.

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

package waitutil

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"

	armadav1 "opendev.org/airship/armada-operator/api/v1"
)

type StatusType string

type Status struct {
	StatusType
	Msg string
}

type MinReady struct {
	int32
	Percent bool
}

const (
	Ready   StatusType = "READY"
	Skipped StatusType = "SKIPPED"
	Unready StatusType = "UNREADY"
	Error   StatusType = "ERROR"
)

// WaitOptions phase run command
type WaitOptions struct {
	RestConfig    *rest.Config
	Namespace     string
	LabelSelector string
	ResourceType  string
	Timeout       time.Duration
	MinReady      string
	Logger        logr.Logger
}

func getObjectStatus(obj interface{}, minReady *MinReady) Status {
	switch v := obj.(type) {
	case *corev1.Pod:
		return isPodReady(v)
	case *batchv1.Job:
		return isJobReady(v)
	case *appsv1.Deployment:
		return isDeploymentReady(v, minReady)
	case appsv1.Deployment:
		return isDeploymentReady(&v, minReady)
	case *appsv1.DaemonSet:
		return isDaemonSetReady(v, minReady)
	case *appsv1.StatefulSet:
		return isStatefulSetReady(v, minReady)
	case *armadav1.ArmadaChart:
		return isArmadaChartReady(v)
	default:
		return Status{Error, fmt.Sprintf("Unable to cast an object to any type %s", obj)}
	}
}

func allMatch(logger logr.Logger, store cache.Store, minReady *MinReady, obj runtime.Object) (bool, error) {
	for _, item := range store.List() {
		if obj != nil && item == obj {
			continue
		}
		status := getObjectStatus(item, minReady)
		logger.Info(fmt.Sprintf("object %T status computed: %s %s", item, status.StatusType, status.Msg))
		if status.StatusType != Ready && status.StatusType != Skipped {
			return false, nil
		}
	}
	logger.Info("all objects are ready")
	return true, nil
}

func processEvent(logger logr.Logger, event watch.Event, minReady *MinReady) (StatusType, error) {
	metaObj, err := meta.Accessor(event.Object)
	if err != nil {
		return Error, err
	}
	metaObj.GetResourceVersion()
	logger.Info(fmt.Sprintf("watch event: type=%s, name=%s, namespace=%s, resource_ver %s",
		event.Type, metaObj.GetName(), metaObj.GetNamespace(), metaObj.GetResourceVersion()))

	if event.Type == "ERROR" {
		return Error, errors.New(fmt.Sprintf("resource %s: got error event %s", metaObj.GetName(), event.Object))
	}

	status := getObjectStatus(event.Object, minReady)
	logger.Info(fmt.Sprintf("object type: %T, status: %s", event.Object, status.Msg))
	return status.StatusType, nil
}

func isArmadaChartReady(ac *armadav1.ArmadaChart) Status {
	if ac.Status.ObservedGeneration == ac.Generation {
		for _, cond := range ac.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				return Status{Ready, fmt.Sprintf("armadachart %s ready", ac.GetName())}
			}
		}
	}
	return Status{Unready, fmt.Sprintf("Waiting for armadachart %s to be ready", ac.GetName())}
}

func isPodReady(pod *corev1.Pod) Status {
	if isTestPod(pod) || pod.Status.Phase == "Evicted" || hasOwner(&pod.ObjectMeta, "Job") {
		return Status{Skipped,
			fmt.Sprintf("Excluding Pod %s from wait: either test pod, owned by job or evicted", pod.GetName())}
	}

	phase := pod.Status.Phase
	if phase == "Succeeded" {
		return Status{Ready, fmt.Sprintf("Pod %s succeeded", pod.GetName())}
	}

	if phase == "Running" {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				return Status{Ready, fmt.Sprintf("Pod %s ready", pod.GetName())}
			}
		}
	}
	return Status{Unready, fmt.Sprintf("Waiting for pod %s to be ready", pod.GetName())}
}

func isJobReady(job *batchv1.Job) Status {
	if hasOwner(&job.ObjectMeta, "CronJob") {
		return Status{Skipped, fmt.Sprintf("Excluding Job %s from wait: owned by CronJob", job.GetName())}
	}

	expected := int32(0)
	if job.Spec.Completions != nil {
		expected = *job.Spec.Completions
	}
	completed := job.Status.Succeeded

	if expected != completed {
		return Status{Unready, fmt.Sprintf("Waiting for job %s to be successfully completed...", job.GetName())}
	}

	return Status{Ready, fmt.Sprintf("Job %s successfully completed", job.GetName())}
}

func isDeploymentReady(deployment *appsv1.Deployment, minReady *MinReady) Status {
	name := deployment.GetName()
	spec := deployment.Spec
	status := deployment.Status
	gen := deployment.GetGeneration()
	observed := status.ObservedGeneration

	if gen <= observed {
		for _, cond := range status.Conditions {
			if cond.Type == "Progressing" && cond.Reason == "ProgressDeadlineExceeded" {
				return Status{Unready, fmt.Sprintf("Deployment %s exceeded its progress deadline", name)}
			}
		}
		replicas := int32(0)
		if spec.Replicas != nil {
			replicas = *spec.Replicas
		}
		updated := status.UpdatedReplicas
		available := status.AvailableReplicas
		if updated < replicas {
			return Status{Unready, fmt.Sprintf("Waiting for deployment %s rollout to finish: %d"+
				" out of %d new replicas have been updated...", name, updated, replicas)}
		}
		if replicas > updated {
			pending := replicas - updated
			return Status{Unready, fmt.Sprintf("Waiting for deployment %s rollout to finish: %d old "+
				"replicas are pending termination...", name, pending)}
		}

		if minReady.Percent {
			minReady.int32 = int32(math.Ceil(float64((minReady.int32 / 100) * updated)))
		}

		if available < minReady.int32 {
			return Status{Unready, fmt.Sprintf("Waiting for deployment %s rollout to finish: %d of %d "+
				"updated replicas are available, with min_ready=%d", name, available, updated, minReady.int32)}
		}

		return Status{Ready, fmt.Sprintf("deployment %s successfully rolled out", name)}
	}

	return Status{Unready, fmt.Sprintf("Waiting for deployment %s spec update to be observed...", name)}
}

func isDaemonSetReady(daemonSet *appsv1.DaemonSet, minReady *MinReady) Status {
	name := daemonSet.GetName()
	status := daemonSet.Status
	gen := daemonSet.GetGeneration()
	observed := status.ObservedGeneration

	if gen <= observed {
		updated := status.UpdatedNumberScheduled
		desired := status.DesiredNumberScheduled
		available := status.NumberAvailable

		if updated < desired {
			return Status{Unready, fmt.Sprintf("Waiting for daemon set %s rollout to finish: %d out "+
				"of %d new pods have been updated...", name, updated, desired)}
		}

		if minReady.Percent {
			minReady.int32 = int32(math.Ceil(float64((minReady.int32 / 100) * desired)))
		}
		if available < minReady.int32 {
			return Status{Unready, fmt.Sprintf("Waiting for daemon set %s rollout to finish: %d of %d "+
				"updated pods are available, with min_ready=%d", name, available, desired, minReady.int32)}
		}

		return Status{Ready, fmt.Sprintf("daemon set %s successfully rolled out", name)}
	}
	return Status{Unready, fmt.Sprintf("Waiting for daemon set spec update to be observed...")}
}

func isStatefulSetReady(statefulSet *appsv1.StatefulSet, minReady *MinReady) Status {
	name := statefulSet.GetName()
	spec := statefulSet.Spec
	status := statefulSet.Status
	gen := statefulSet.GetGeneration()
	observed := status.ObservedGeneration
	replicas := int32(0)
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}
	ready := status.ReadyReplicas
	updated := status.UpdatedReplicas
	current := status.CurrentReplicas

	if observed == 0 || gen > observed {
		return Status{Unready, fmt.Sprintf("Waiting for statefulset spec update to be observed...")}
	}

	if minReady.Percent {
		minReady.int32 = int32(math.Ceil(float64((minReady.int32 / 100) * replicas)))
	}

	if replicas > 0 && ready < minReady.int32 {
		return Status{Unready, fmt.Sprintf("Waiting for statefulset %s rollout to finish: %d of %d "+
			"pods are ready, with min_ready=%d", name, ready, replicas, minReady.int32)}
	}

	updateRev := status.UpdateRevision
	currentRev := status.CurrentRevision
	if updateRev != currentRev {
		return Status{Unready, fmt.Sprintf("waiting for statefulset rolling update to complete %d "+
			"pods at revision %s...", updated, updateRev)}
	}

	return Status{Ready, fmt.Sprintf("statefulset rolling update complete %d pods at revision %s...",
		current, currentRev)}
}

func isTestPod(u *corev1.Pod) bool {
	annotations := u.GetAnnotations()
	var testHooks []string
	if len(annotations) > 0 {
		if val, ok := annotations["helm.sh/hook"]; ok {
			hooks := strings.Split(val, ",")
			for _, h := range hooks {
				if h == "test" || h == "test-success" || h == "test-failure" {
					testHooks = append(testHooks, h)
				}
			}
		}
	}
	return len(testHooks) > 0
}

func hasOwner(ometa *metav1.ObjectMeta, kind string) bool {
	ownerRefs := ometa.GetOwnerReferences()
	for _, owner := range ownerRefs {
		if kind == owner.Kind {
			return true
		}
	}
	return false
}

func getClient(resource string, config *rest.Config) (rest.Interface, error) {
	if resource == "armadacharts" {
		return armadav1.NewForConfig(config)
	}

	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	switch resource {
	case "jobs":
		return cs.BatchV1().RESTClient(), nil
	case "pods":
		return cs.CoreV1().RESTClient(), nil
	case "daemonsets", "deployments", "statefulsets":
		return cs.AppsV1().RESTClient(), nil
	}

	return nil, errors.New(fmt.Sprintf("Unable to find a client for resource '%s'", resource))
}

func getMinReady(minReady string) (*MinReady, error) {
	ret := &MinReady{0, false}
	if minReady != "" {
		if strings.HasSuffix(minReady, "%") {
			ret.Percent = true
		}
		//var err error
		val, err := strconv.Atoi(strings.ReplaceAll(minReady, "%", ""))
		if err != nil {
			return nil, err
		}
		ret.int32 = int32(val)

	}
	return ret, nil
}

func (c *WaitOptions) Wait(parent context.Context) error {
	c.Logger.Info(fmt.Sprintf("starting wait for resources: namespace %s labels %s type %s timeout %s", c.Namespace, c.LabelSelector, c.ResourceType, c.Timeout))

	clientSet, err := getClient(c.ResourceType, c.RestConfig)
	if err != nil {
		return err
	}

	ctx, cancelFunc := watchtools.ContextWithOptionalTimeout(parent, c.Timeout)
	defer cancelFunc()

	lw := cache.NewFilteredListWatchFromClient(clientSet, c.ResourceType, c.Namespace, func(options *metav1.ListOptions) {
		options.LabelSelector = c.LabelSelector
	})

	minReady, err := getMinReady(c.MinReady)
	if err != nil {
		return err
	}

	var cacheStore cache.Store

	cpu := func(store cache.Store) (bool, error) {
		cacheStore = store
		c.Logger.Info(fmt.Sprintf("number of objects to watch: %d", len(store.List())))
		if len(store.List()) == 0 {
			c.Logger.Info(fmt.Sprintf("Skipping non-required wait, no resources found"))
			return true, nil
		}
		return allMatch(c.Logger, cacheStore, minReady, nil)
	}

	cfu := func(event watch.Event) (bool, error) {
		if ready, err := processEvent(c.Logger, event, minReady); ready != Ready || err != nil {
			return false, err
		}

		return allMatch(c.Logger, cacheStore, minReady, event.Object)
	}

	_, err = watchtools.UntilWithSync(ctx, lw, nil, cpu, cfu)
	return err
}
