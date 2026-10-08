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
	"net"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsRetryableUpgradeError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			// Verbatim from the outage that motivated this change: the
			// cert-manager webhook had a single replica, which was lost with
			// its node. The resulting uninstall deleted the MariaDB releases.
			name: "webhook unavailable during server-side apply",
			err: errors.New(`server-side apply failed for object openstack/mariadb-tls-direct ` +
				`cert-manager.io/v1, Kind=Certificate: Internal error occurred: ` +
				`failed calling webhook "webhook.cert-manager.io": failed to call webhook: ` +
				`Post "https://cert-manager-webhook.cert-manager.svc:443/validate?timeout=30s": ` +
				`dial tcp 10.96.14.120:443: connect: connection refused`),
			want: true,
		},
		{
			name: "wrapped connection refused",
			err:  fmt.Errorf("failed to upgrade release: %w", errors.New("dial tcp 10.96.14.120:443: connect: connection refused")),
			want: true,
		},
		{
			name: "net.OpError",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
			want: true,
		},
		{
			name: "context deadline exceeded",
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "apierrors service unavailable",
			err:  apierrors.NewServiceUnavailable("backend unavailable"),
			want: true,
		},
		{
			name: "apierrors timeout",
			err:  apierrors.NewTimeoutError("request timed out", 30),
			want: true,
		},
		{
			name: "apierrors too many requests",
			err:  apierrors.NewTooManyRequestsError("slow down"),
			want: true,
		},
		{
			name: "etcd request timed out",
			err:  errors.New("etcdserver: request timed out"),
			want: true,
		},
		{
			// Terminal: the release genuinely needs a reinstall, so the
			// uninstall path must still be reachable.
			name: "immutable field conflict",
			err: errors.New(`cannot patch "mariadb-server" with kind StatefulSet: ` +
				`StatefulSet.apps "mariadb-server" is invalid: spec: Forbidden: ` +
				`updates to statefulset spec for fields other than 'replicas', ` +
				`'ordinals', 'template' are forbidden`),
			want: false,
		},
		{
			name: "release stuck in pending-upgrade",
			err:  errors.New(`another operation (install/upgrade/rollback) is in progress`),
			want: false,
		},
		{
			name: "chart rendering failure",
			err:  errors.New(`template: mariadb/templates/statefulset.yaml:12:3: executing "" at <.Values.missing>: nil pointer evaluating interface {}`),
			want: false,
		},
		{
			name: "admission denied by policy",
			err: apierrors.NewForbidden(
				schema.GroupResource{Group: "apps", Resource: "statefulsets"},
				"mariadb-server",
				errors.New("denied by gatekeeper policy"),
			),
			want: false,
		},
		{
			name: "not found",
			err: apierrors.NewNotFound(
				schema.GroupResource{Group: "apps", Resource: "statefulsets"},
				"mariadb-server",
			),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableUpgradeError(tt.err); got != tt.want {
				t.Errorf("isRetryableUpgradeError() = %v, want %v\nerror: %v", got, tt.want, tt.err)
			}
		})
	}
}

// TestIsRetryableUpgradeErrorStatusError guards the typed path for the exact
// shape the API server returns for a failed admission webhook call, which
// arrives as a StatusError with reason InternalError.
func TestIsRetryableUpgradeErrorStatusError(t *testing.T) {
	err := &apierrors.StatusError{ErrStatus: v1.Status{
		Status:  v1.StatusFailure,
		Code:    500,
		Reason:  v1.StatusReasonInternalError,
		Message: `Internal error occurred: failed calling webhook "webhook.cert-manager.io"`,
	}}

	if !isRetryableUpgradeError(err) {
		t.Errorf("expected webhook StatusError to be retryable, got false")
	}
}
