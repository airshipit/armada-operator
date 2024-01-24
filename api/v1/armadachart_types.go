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

package v1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
)

const ArmadaChartKind = "ArmadaChart"
const ArmadaChartAPIVersion = "armada.airshipit.org/v1"
const ArmadaChartPlural = "armadacharts"
const ArmadaChartGroup = "armada.airshipit.org"
const ArmadaChartVersion = "v1"
const ArmadaChartLabel = "armada.airshipit.org/release-name"
const ArmadaChartFinalizer = "finalizers.armada.airshipit.org"

const ReadyCondition string = "Ready"
const ReconcilingCondition string = "Reconciling"
const ProgressingReason string = "Progressing"

// ArmadaChartSpec defines the specification of ArmadaChart
type ArmadaChartSpec struct {
	// ChartName is name of ArmadaChart
	ChartName string `json:"chart_name,omitempty"`
	// Namespace is a namespace for ArmadaChart
	Namespace string `json:"namespace,omitempty"`
	// Release is a name of corresponding Helm Release of ArmadaChart
	Release string `json:"release,omitempty"`

	// Source is a source location of Helm Chart *.tgz
	Source ArmadaChartSource `json:"source,omitempty"`

	// Values holds the values for this Helm release.
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// Wait holds the wait options  for this Helm release.
	// +optional
	Wait ArmadaChartWait `json:"wait,omitempty"`

	// Test holds the test parameters for this Helm release.
	// +optional
	Test ArmadaChartTestOptions `json:"test,omitempty"`

	// Upgrade holds the upgrade options for this Helm release.
	// +optional
	Upgrade ArmadaChartUpgrade `json:"upgrade,omitempty"`
}

// ArmadaChartSource defines the location options of Helm Release for ArmadaChart
type ArmadaChartSource struct {
	Location string `json:"location,omitempty"`
	Subpath  string `json:"subpath,omitempty"`
	Type     string `json:"type,omitempty"`
}

// ArmadaChartWait defines the wait options of ArmadaChart
type ArmadaChartWait struct {
	// Timeout is the time to wait for full reconciliation of Helm release.
	// +optional
	Timeout int `json:"timeout,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`

	// +optional
	ArmadaChartWaitResources []ArmadaChartWaitResource `json:"resources"`

	// +optional
	Native *ArmadaChartWaitNative `json:"native,omitempty"`
}

// ArmadaChartWaitResource defines the wait options of ArmadaChart
type ArmadaChartWaitResource struct {
	Type string `json:"type,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`

	// +optional
	MinReady string `json:"min_ready,omitempty"`
}

type ArmadaChartUpgrade struct {
	PreUpgrade ArmadaChartPreUpgrade `json:"pre,omitempty"`
}

type ArmadaChartPreUpgrade struct {
	Delete []ArmadaChartDeleteResource `json:"delete,omitempty"`
}

// ArmadaChartDeleteResource defines the delete options of ArmadaChart
type ArmadaChartDeleteResource struct {
	Type string `json:"type,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
}

// ArmadaChartWaitNative defines the wait options of ArmadaChart
type ArmadaChartWaitNative struct {
	Enabled bool `json:"enabled,omitempty"`
}

// ArmadaChartTestOptions defines the test options of ArmadaChart
type ArmadaChartTestOptions struct {
	// Enabled is an example field of ArmadaChart. Edit armadachart_types.go to remove/update
	Enabled bool `json:"enabled,omitempty"`
}

// ArmadaChartStatus defines the observed state of ArmadaChart
type ArmadaChartStatus struct {
	// ObservedGeneration is the last observed generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the ArmadaChart.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastAppliedRevision is the revision of the last successfully applied source.
	// +optional
	LastAppliedRevision string `json:"lastAppliedRevision,omitempty"`

	// LastAttemptedRevision is the revision of the last reconciliation attempt.
	// +optional
	LastAttemptedRevision string `json:"lastAttemptedRevision,omitempty"`

	// LastAttemptedValuesChecksum is the SHA1 checksum of the values of the last
	// reconciliation attempt.
	// +optional
	LastAttemptedValuesChecksum string `json:"lastAttemptedValuesChecksum,omitempty"`

	// LastReleaseRevision is the revision of the last successful Helm release.
	// +optional
	LastReleaseRevision int `json:"lastReleaseRevision,omitempty"`

	// HelmChart is the namespaced name of the HelmChart resource created by
	// the controller for the ArmadaChart.
	// +optional
	HelmChart string `json:"helmChart,omitempty"`

	// Failures is the reconciliation failure count against the latest desired
	// state. It is reset after a successful reconciliation.
	// +optional
	Failures int64 `json:"failures,omitempty"`

	// InstallFailures is the install failure count against the latest desired
	// state. It is reset after a successful reconciliation.
	// +optional
	InstallFailures int64 `json:"installFailures,omitempty"`

	// UpgradeFailures is the upgrade failure count against the latest desired
	// state. It is reset after a successful reconciliation.
	// +optional
	UpgradeFailures int64 `json:"upgradeFailures,omitempty"`

	// Tested is the bool value whether the Helm Release was successfully
	// tested or not.
	// +optional
	Tested bool `json:"tested,omitempty"`
}

// ArmadaChartProgressing resets any failures and registers progress toward
// reconciling the given ArmadaChart by setting the ReadyCondition to
// 'Unknown' for ProgressingReason.
func ArmadaChartProgressing(ac ArmadaChart) ArmadaChart {
	ac.Status.Conditions = []metav1.Condition{}
	newCondition := metav1.Condition{
		Type:    ReadyCondition,
		Status:  metav1.ConditionUnknown,
		Reason:  ProgressingReason,
		Message: "Reconciliation in progress",
	}
	apimeta.SetStatusCondition(ac.GetStatusConditions(), newCondition)
	resetFailureCounts(&ac)
	resetTested(&ac)
	return ac
}

// ArmadaChartNotReady registers a failed reconciliation of the given ArmadaChart.
func ArmadaChartNotReady(ac ArmadaChart, reason, message string) ArmadaChart {
	newCondition := metav1.Condition{
		Type:    ReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	}
	apimeta.SetStatusCondition(ac.GetStatusConditions(), newCondition)
	ac.Status.Failures++
	resetTested(&ac)
	return ac
}

// ArmadaChartReady registers a successful reconciliation of the given ArmadaChart.
func ArmadaChartReady(ac ArmadaChart) ArmadaChart {
	newCondition := metav1.Condition{
		Type:    ReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  "ReconciliationSucceeded",
		Message: "Release reconciliation succeeded",
	}
	apimeta.SetStatusCondition(ac.GetStatusConditions(), newCondition)
	ac.Status.LastAppliedRevision = ac.Status.LastAttemptedRevision
	resetFailureCounts(&ac)
	setTested(&ac)
	return ac
}

func resetFailureCounts(hr *ArmadaChart) {
	hr.Status.Failures = 0
	hr.Status.InstallFailures = 0
	hr.Status.UpgradeFailures = 0
}

func resetTested(hr *ArmadaChart) {
	hr.Status.Tested = false
}

func setTested(hr *ArmadaChart) {
	hr.Status.Tested = true
}

func NewForConfigOrDie(c *rest.Config) *rest.RESTClient {
	cs, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return cs
}

func NewForConfig(c *rest.Config) (*rest.RESTClient, error) {
	config := *c
	if err := setConfigDefaults(&config); err != nil {
		return nil, err
	}
	client, err := rest.UnversionedRESTClientFor(&config)

	if err != nil {
		return nil, err
	}
	return client, nil
}

func setConfigDefaults(config *rest.Config) error {
	gv := schema.GroupVersion{Group: ArmadaChartGroup, Version: ArmadaChartVersion}
	config.GroupVersion = &gv
	config.APIPath = "/apis"

	sch := runtime.NewScheme()
	if err := AddToScheme(sch); err != nil {
		return err
	}
	sch.AddUnversionedTypes(gv, &ArmadaChart{}, &ArmadaChartList{})
	config.NegotiatedSerializer = serializer.NewCodecFactory(sch)

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// ArmadaChart is the Schema for the armadacharts API
type ArmadaChart struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ArmadaChartSpec   `json:"data,omitempty"`
	Status ArmadaChartStatus `json:"status,omitempty"`
}

// GetStatusConditions returns a pointer to the Status.Conditions slice.
func (in *ArmadaChart) GetStatusConditions() *[]metav1.Condition {
	return &in.Status.Conditions
}

//+kubebuilder:object:root=true

// ArmadaChartList contains a list of ArmadaChart
type ArmadaChartList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArmadaChart `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArmadaChart{}, &ArmadaChartList{})
}
