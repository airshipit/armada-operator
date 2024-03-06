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

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"net/http"
	"os"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/hashicorp/go-retryablehttp"
	//"golang.org/x/exp/maps"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	armadav1 "opendev.org/airship/armada-operator/api/v1"
	"opendev.org/airship/armada-operator/pkg/kube"
	"opendev.org/airship/armada-operator/pkg/runner"
	"opendev.org/airship/armada-operator/pkg/waitutil"
)

// ArmadaChartReconciler reconciles a ArmadaChart object
type ArmadaChartReconciler struct {
	client.Client

	ControllerName string

	Scheme     *runtime.Scheme
	httpClient *retryablehttp.Client
}

//+kubebuilder:rbac:groups=armada.airshipit.org,resources=armadacharts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=armada.airshipit.org,resources=armadacharts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=armada.airshipit.org,resources=armadacharts/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ArmadaChartReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()
	log := ctrl.LoggerFrom(ctx)

	log.Info("reconciling has started")

	// Retrieve the custom resource
	var ac armadav1.ArmadaChart
	if err := r.Get(ctx, req.NamespacedName, &ac); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Add our finalizer if it does not exist
	if !controllerutil.ContainsFinalizer(&ac, armadav1.ArmadaChartFinalizer) {
		patch := client.MergeFrom(ac.DeepCopy())
		controllerutil.AddFinalizer(&ac, armadav1.ArmadaChartFinalizer)
		if err := r.Patch(ctx, &ac, patch); err != nil {
			log.Error(err, "unable to register finalizer")
			return ctrl.Result{}, err
		}
	}

	// Examine if the object is under deletion
	if !ac.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("object is under deletion, uninstalling corresponding helm release")
		return r.reconcileDelete(ctx, &ac)
	}

	// Perform reconciliation
	ac, result, err := r.reconcile(ctx, ac)
	if updateStatusErr := r.patchStatus(ctx, &ac); updateStatusErr != nil {
		log.Error(updateStatusErr, "unable to update status after reconciliation")
		return ctrl.Result{Requeue: false}, updateStatusErr
	}

	// Log reconciliation duration
	durationMsg := fmt.Sprintf("reconciliation finished in %s", time.Since(start).String())
	if result.RequeueAfter > 0 {
		durationMsg = fmt.Sprintf("%s, next run in %s", durationMsg, result.RequeueAfter.String())
	}
	log.Info(durationMsg)

	return result, err
}

func (r *ArmadaChartReconciler) reconcile(ctx context.Context, ac armadav1.ArmadaChart) (armadav1.ArmadaChart, ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Observe HelmRelease generation.
	if ac.Status.ObservedGeneration != ac.Generation {
		ac.Status.ObservedGeneration = ac.Generation
		ac = armadav1.ArmadaChartProgressing(ac)
		if updateStatusErr := r.patchStatus(ctx, &ac); updateStatusErr != nil {
			log.Error(updateStatusErr, "unable to update status after generation update")
			return ac, ctrl.Result{Requeue: true}, updateStatusErr
		}
	}

	// Prepare values
	var vals map[string]interface{}
	if ac.Spec.Values != nil {
		var valsErr error
		vals, valsErr = chartutil.ReadValues(ac.Spec.Values.Raw)
		if valsErr != nil {
			return armadav1.ArmadaChartNotReady(ac, "InitFailed", valsErr.Error()), ctrl.Result{}, valsErr
		}
	}
	// Load chart from artifact
	chrt, err := r.loadHelmChart(ctx, ac, ac.Spec.Source.Location)
	if err != nil {
		return armadav1.ArmadaChartNotReady(ac, "ArtifactFailed", err.Error()), ctrl.Result{}, err
	}

	reconAc, reconErr := r.reconcileChart(ctx, *ac.DeepCopy(), chrt, vals)

	return reconAc, ctrl.Result{}, reconErr
}

func (r *ArmadaChartReconciler) reconcileChart(ctx context.Context,
	ac armadav1.ArmadaChart, chrt *chart.Chart, vals chartutil.Values) (armadav1.ArmadaChart, error) {

	log := ctrl.LoggerFrom(ctx)
	gettr, err := r.buildRESTClientGetter(ctx, ac)
	if err != nil {
		return armadav1.ArmadaChartNotReady(ac, "InitFailed", err.Error()), err
	}
	restCfg, err := gettr.ToRESTConfig()
	if err != nil {
		return armadav1.ArmadaChartNotReady(ac, "InitFailed", err.Error()), err
	}

	run, err := runner.NewRunner(gettr, ac.Namespace, log)
	if err != nil {
		return armadav1.ArmadaChartNotReady(ac, "InitFailed", "failed to initialize Helm action runner"), err
	}

	// Determine last release revision.
	rel, observeLastReleaseErr := run.ObserveLastRelease(ac)
	if observeLastReleaseErr != nil {
		err = fmt.Errorf("failed to get last release revision: %w", observeLastReleaseErr)
		return armadav1.ArmadaChartNotReady(ac, "GetLastReleaseFailed", "failed to get last release revision"), err
	}

	if rel == nil {
		log.Info("helm install has started")
		rel, err = run.Install(ctx, ac, chrt, vals)
	} else {
		ac.Status.HelmStatus = string(rel.Info.Status)
		if updateStatusErr := r.patchStatus(ctx, &ac); updateStatusErr != nil {
			log.Error(updateStatusErr, "unable to update status after helm status update")
			return armadav1.ArmadaChartNotReady(ac, "UpdateStatusFailed", updateStatusErr.Error()), updateStatusErr
		}

		if rel.Info.Status == release.StatusDeployed && !isUpdateRequired(ctx, rel, chrt, vals) {
			log.Info("no updates found, skipping upgrade")
			return r.finalizeRelease(ctx, run, restCfg, ac)
		}

		if rel.Info.Status.IsPending() {
			log.Info("warning: release in pending state, unlocking")
			rel.SetStatus(release.StatusFailed, fmt.Sprintf("release unlocked from stale state"))
			if err = run.UpdateReleaseStatus(rel); err != nil {
				return armadav1.ArmadaChartNotReady(ac, "UpdateHelmStatusFailed", err.Error()), err
			}
		} else {
			for _, delRes := range ac.Spec.Upgrade.PreUpgrade.Delete {
				log.Info(fmt.Sprintf("deleting all %ss in %s ns with labels %v", delRes.Type, ac.Spec.Namespace, delRes.Labels))
				obj, objList := new(client.Object), new(client.ObjectList)
				switch delRes.Type {
				case "", "job":
					*obj, *objList = &batchv1.Job{}, &batchv1.JobList{}
				case "pod":
					*obj, *objList = &corev1.Pod{}, &corev1.PodList{}
				case "cronjob":
					*obj, *objList = &batchv1.CronJob{}, &batchv1.CronJobList{}
				}

				err = r.DeleteAllOf(ctx, *obj, client.MatchingLabels(delRes.Labels), client.InNamespace(ac.Spec.Namespace), client.PropagationPolicy(v1.DeletePropagationForeground))
				if err != nil {
					return armadav1.ArmadaChartNotReady(ac, "DeleteFailed", err.Error()), err
				}
				if err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 30*time.Second, true, func(ctx context.Context) (done bool, err error) {
					if err := r.List(ctx, *objList, client.MatchingLabels(delRes.Labels), client.InNamespace(ac.Spec.Namespace)); err != nil {
						return false, err
					}
					l, err := apimeta.ExtractList(*objList)
					if err != nil {
						return false, err
					}
					if len(l) > 0 {
						log.Info(fmt.Sprintf("%d %ss still have found in %s ns with labels %v", len(l), delRes.Type, ac.Spec.Namespace, delRes.Labels))
						return false, nil
					}
					log.Info(fmt.Sprintf("no %ss have found in %s ns with labels %v, pre update completed", delRes.Type, ac.Spec.Namespace, delRes.Labels))
					return true, nil
				}); err != nil {
					return armadav1.ArmadaChartNotReady(ac, "PreUpdateFailed", err.Error()), err
				}
			}
		}

		log.Info("helm upgrade has started")
		rel, err = run.Upgrade(ctx, ac, chrt, vals)
	}

	if err != nil {
		err = fmt.Errorf("failed to install/upgrade helm release: %s", err.Error())
		return armadav1.ArmadaChartNotReady(ac, "InstallUpgradeFailed", err.Error()), err
	}
	ac.Status.HelmStatus = string(rel.Info.Status)
	ac.Status.LastAppliedChartSource = ac.Spec.Source.Location
	if err := r.patchStatus(ctx, &ac); err != nil {
		log.Error(err, "unable to update armadachart status")
	}

	return r.finalizeRelease(ctx, run, restCfg, ac)
}

func (r *ArmadaChartReconciler) finalizeRelease(ctx context.Context, run *runner.Runner, restCfg *rest.Config, hr armadav1.ArmadaChart) (armadav1.ArmadaChart, error) {
	hr, err := r.waitRelease(ctx, restCfg, hr)
	if err != nil {
		return hr, err
	}

	return r.testRelease(ctx, run, hr)
}

func (r *ArmadaChartReconciler) waitRelease(ctx context.Context, restCfg *rest.Config, hr armadav1.ArmadaChart) (armadav1.ArmadaChart, error) {
	log := ctrl.LoggerFrom(ctx)

	if hr.Status.WaitCompleted {
		return hr, nil
	}

	if hr.Spec.Wait.Timeout == 0 {
		log.Info("No Chart timeout specified, using default 900s")
		hr.Spec.Wait.Timeout = 900
	}

	if hr.Spec.Wait.ArmadaChartWaitResources == nil {
		log.Info(fmt.Sprintf("there are no explicitly defined resources to wait: %s, using default ones", hr.Name))
		hr.Spec.Wait.ArmadaChartWaitResources = []armadav1.ArmadaChartWaitResource{{Type: "job"}, {Type: "pod"}}
	}
	if len(hr.Spec.Wait.ArmadaChartWaitResources) == 0 {
		log.Info(fmt.Sprintf("there are none resources to wait: %s", hr.Name))
	}
	if hr.Spec.Wait.Labels == nil {
		hr.Spec.Wait.Labels = make(map[string]string)
	}

	for _, res := range hr.Spec.Wait.ArmadaChartWaitResources {
		log.Info(fmt.Sprintf("processing wait resource %v", res))
		if res.Labels == nil || len(res.Labels) == 0 {
			res.Labels = make(map[string]string)
		}

		for kk, vv := range hr.Spec.Wait.Labels {
			res.Labels[kk] = vv
		}

		if len(res.Labels) == 0 {
			log.Info("no selectors applied, continuing...")
			continue
		}

		log.Info(fmt.Sprintf("Resolved `wait.resources` list: %v", res))

		var labelSelector string
		for k, v := range res.Labels {
			if len(labelSelector) > 0 {
				labelSelector = fmt.Sprintf("%s,%s=%s", labelSelector, k, v)
			} else {
				labelSelector = fmt.Sprintf("%s=%s", k, v)
			}
		}

		opts := waitutil.WaitOptions{
			RestConfig:    restCfg,
			Namespace:     hr.Spec.Namespace,
			LabelSelector: labelSelector,
			ResourceType:  fmt.Sprintf("%ss", res.Type),
			Timeout:       time.Second * time.Duration(hr.Spec.Wait.Timeout),
			MinReady:      res.MinReady,
			Logger:        log,
		}
		err := opts.Wait(ctx)
		if err != nil {
			return hr, err
		}
	}

	log.Info("all resources are ready")
	hr.Status.WaitCompleted = true
	if err := r.patchStatus(ctx, &hr); err != nil {
		return hr, err
	}

	return hr, nil
}

func (r *ArmadaChartReconciler) testRelease(ctx context.Context, run *runner.Runner, ac armadav1.ArmadaChart) (armadav1.ArmadaChart, error) {
	log := ctrl.LoggerFrom(ctx)
	if ac.Spec.Test.Enabled && !ac.Status.Tested {
		log.Info("performing tests")
		if _, err := run.Test(ac); err != nil {
			return armadav1.ArmadaChartNotReady(ac, "TestFailed", err.Error()), err
		}
	}
	return armadav1.ArmadaChartReady(ac), nil
}

// loadHelmChart attempts to download the artifact from the provided source,
// loads it into a chart.Chart, and removes the downloaded artifact.
// It returns the loaded chart.Chart on success, or an error.
func (r *ArmadaChartReconciler) loadHelmChart(ctx context.Context, hr armadav1.ArmadaChart, source string) (*chart.Chart, error) {
	log := ctrl.LoggerFrom(ctx)
	f, err := os.CreateTemp("", fmt.Sprintf("%s-%s-*.tgz", hr.GetNamespace(), hr.GetName()))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	defer os.Remove(f.Name())

	req, err := retryablehttp.NewRequest(http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download artifact, error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artifact '%s' download failed (status code: %s)", source, resp.Status)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		return nil, err
	}

	log.Info(fmt.Sprintf("helm chart downloaded to %s", f.Name()))
	return loader.Load(f.Name())
}

func (r *ArmadaChartReconciler) buildRESTClientGetter(ctx context.Context, hr armadav1.ArmadaChart) (genericclioptions.RESTClientGetter, error) {
	log := ctrl.LoggerFrom(ctx)
	opts := []kube.Option{
		kube.WithNamespace(hr.Spec.Namespace),
		kube.WithPersistent(true),
		kube.WithWarningHandler(func(s string) {
			log.Info(s)
		}),
	}

	return kube.NewInClusterMemoryRESTClientGetter(opts...)
}

func (r *ArmadaChartReconciler) patchStatus(ctx context.Context, ac *armadav1.ArmadaChart) error {
	latest := &armadav1.ArmadaChart{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(ac), latest); err != nil {
		return err
	}
	patch := client.MergeFrom(latest.DeepCopy())
	latest.Status = ac.Status
	return r.Client.Status().Patch(ctx, latest, patch, client.FieldOwner(r.ControllerName))
}

// reconcileDelete deletes the Helm Release of the ArmadaChart,
// and uninstalls the Helm release if the resource has not been suspended.
// It only performs a Helm uninstall if the ServiceAccount to be impersonated
// exists.
func (r *ArmadaChartReconciler) reconcileDelete(ctx context.Context, ac *armadav1.ArmadaChart) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	getter, err := r.buildRESTClientGetter(ctx, *ac)
	if err != nil {
		return ctrl.Result{}, err
	}
	run, err := runner.NewRunner(getter, ac.Spec.Namespace, ctrl.LoggerFrom(ctx))
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := run.Uninstall(*ac); err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
		return ctrl.Result{}, err
	}
	log.Info("uninstalled Helm release for deleted resource")

	// Remove our finalizer from the list and update it.
	controllerutil.RemoveFinalizer(ac, armadav1.ArmadaChartFinalizer)
	ac.Status.HelmStatus = string(release.StatusUninstalled)
	if err := r.Update(ctx, ac); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArmadaChartReconciler) SetupWithManager(mgr ctrl.Manager) error {
	httpClient := retryablehttp.NewClient()
	httpClient.RetryWaitMin = 5 * time.Second
	httpClient.RetryWaitMax = 30 * time.Second
	httpClient.RetryMax = 3
	httpClient.Logger = nil
	r.httpClient = httpClient

	r.ControllerName = "armada-controller"

	return ctrl.NewControllerManagedBy(mgr).
		For(&armadav1.ArmadaChart{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(r)
}

func isUpdateRequired(ctx context.Context, release *release.Release, chrt *chart.Chart, vals chartutil.Values) bool {
	log := ctrl.LoggerFrom(ctx)

	switch {
	case !cmp.Equal(release.Chart.Templates, chrt.Templates, cmpopts.EquateEmpty()):
		log.Info("There are chart template diffs found")
		log.Info(cmp.Diff(release.Chart.Templates, chrt.Templates))
		return true

	case !cmp.Equal(release.Config, vals.AsMap(), cmpopts.EquateEmpty()):
		log.Info("There are chart values diffs found")
		log.Info(cmp.Diff(release.Config, vals.AsMap(), cmpopts.EquateEmpty()))
		return true
	}

	return false
}
