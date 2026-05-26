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

package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	release "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"k8s.io/cli-runtime/pkg/genericclioptions"

	armadav1 "opendev.org/airship/armada-operator/api/v1"
)

// Runner represents a Helm action runner capable of performing Helm
// operations for a ArmadaChart.
type Runner struct {
	mu        sync.Mutex
	config    *action.Configuration
	logBuffer *LogBuffer
}

type ActionError struct {
	Err          error
	CapturedLogs string
}

func (e ActionError) Error() string {
	return e.Err.Error()
}

func (e ActionError) Unwrap() error {
	return e.Err
}

// NewRunner constructs a new Runner configured to run Helm actions with the
// given genericclioptions.RESTClientGetter, and the release and storage
// namespace configured to the provided values.
func NewRunner(getter genericclioptions.RESTClientGetter, storageNamespace string, logger logr.Logger) (*Runner, error) {
	runner := &Runner{
		logBuffer: NewLogBuffer(NewDebugLog(logger.V(2)), defaultBufferSize),
	}

	handler := runner.logBuffer.SlogHandler()
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(handler))
	if err := cfg.Init(getter, storageNamespace, "secret"); err != nil {
		return nil, err
	}

	// Override the kube client logger with our log buffer so captured logs
	// appear in error messages.
	if kc, ok := cfg.KubeClient.(*kube.Client); ok {
		kc.SetLogger(handler)
	}
	runner.config = cfg

	return runner, nil
}

// Install runs a Helm install action for the given ArmadaChart.
func (r *Runner) Install(ctx context.Context, ac armadav1.ArmadaChart, chrt *chart.Chart, values common.Values) (*release.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.logBuffer.Reset()

	install := action.NewInstall(r.config)
	install.ReleaseName = ac.Name
	install.Namespace = ac.Namespace
	install.Timeout = time.Duration(int64(time.Second) * int64(ac.Spec.Wait.Timeout))

	// HookOnly: wait for hooks only; Legacy: wait for all resources and hooks.
	install.WaitStrategy = kube.HookOnlyStrategy
	if ac.Spec.Wait.Native != nil && ac.Spec.Wait.Native.Enabled {
		install.WaitStrategy = kube.LegacyStrategy
	}
	install.DisableOpenAPIValidation = true
	install.CreateNamespace = true

	reli, err := install.RunWithContext(ctx, chrt, values.AsMap())
	if err != nil {
		return nil, wrapActionErr(r.logBuffer, err)
	}
	rel, ok := reli.(*release.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected release type: %T", reli)
	}
	return rel, nil
}

// Upgrade runs a Helm upgrade action for the given ArmadaChart.
func (r *Runner) Upgrade(ctx context.Context, ac armadav1.ArmadaChart, chrt *chart.Chart, values common.Values) (*release.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.logBuffer.Reset()

	upgrade := action.NewUpgrade(r.config)
	upgrade.Namespace = ac.Spec.Namespace
	upgrade.Timeout = time.Duration(int64(time.Second) * int64(ac.Spec.Wait.Timeout))

	// HookOnly: wait for hooks only; Legacy: wait for all resources and hooks.
	upgrade.WaitStrategy = kube.HookOnlyStrategy
	if ac.Spec.Wait.Native != nil && ac.Spec.Wait.Native.Enabled {
		upgrade.WaitStrategy = kube.LegacyStrategy
	}
	upgrade.DisableOpenAPIValidation = true

	reli, err := upgrade.RunWithContext(ctx, ac.Name, chrt, values.AsMap())
	if err != nil {
		return nil, wrapActionErr(r.logBuffer, err)
	}
	rel, ok := reli.(*release.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected release type: %T", reli)
	}
	return rel, nil
}

// Test runs a Helm test action for the given ArmadaChart.
func (r *Runner) Test(ac armadav1.ArmadaChart) (*release.Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.logBuffer.Reset()

	r.logBuffer.Log("performing test")
	test := action.NewReleaseTesting(r.config)
	test.Namespace = ac.Spec.Namespace
	test.Timeout = time.Duration(int64(time.Second) * int64(ac.Spec.Wait.Timeout))

	reli, _, err := test.Run(ac.Name)
	if err != nil {
		return nil, wrapActionErr(r.logBuffer, err)
	}
	rel, ok := reli.(*release.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected release type: %T", reli)
	}
	return rel, nil
}

// Uninstall runs a Helm uninstall action
func (r *Runner) Uninstall(ac armadav1.ArmadaChart) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.logBuffer.Reset()

	uninstall := action.NewUninstall(r.config)
	uninstall.WaitStrategy = kube.LegacyStrategy
	uninstall.Timeout = time.Duration(int64(time.Second) * 300)

	_, err := uninstall.Run(ac.Name)
	return wrapActionErr(r.logBuffer, err)
}

// ObserveLastRelease observes the last revision, if there is one,
// for the actual Helm release associated with the given ArmadaChart.
func (r *Runner) ObserveLastRelease(ac armadav1.ArmadaChart) (*release.Release, error) {
	reli, err := r.config.Releases.Last(ac.Name)
	if err != nil && errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rel, ok := reli.(*release.Release)
	if !ok {
		return nil, fmt.Errorf("unexpected release type: %T", reli)
	}
	return rel, nil
}

// UpdateReleaseStatus sets the new status for release
func (r *Runner) UpdateReleaseStatus(rel *release.Release) error {
	return r.config.Releases.Update(rel)
}

func wrapActionErr(log *LogBuffer, err error) error {
	if err == nil {
		return err
	}
	err = &ActionError{
		Err:          err,
		CapturedLogs: log.String(),
	}
	return err
}
