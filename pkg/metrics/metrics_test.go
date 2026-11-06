/*
Copyright 2026 The Kubernetes Authors.

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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

func resetAll(t *testing.T) {
	t.Helper()
	GroupRecreationsTotal.Reset()
	GroupRestartBudgetExhaustedTotal.Reset()
	GroupRestartBackoffsTotal.Reset()
	t.Cleanup(func() {
		GroupRecreationsTotal.Reset()
		GroupRestartBudgetExhaustedTotal.Reset()
		GroupRestartBackoffsTotal.Reset()
	})
}

func TestCounters(t *testing.T) {
	resetAll(t)

	GroupRecreated("ns-a", "lws-a", GroupRecreateReasonPodRestart)
	GroupRecreated("ns-a", "lws-a", GroupRecreateReasonPodRestart)
	GroupRecreated("ns-a", "lws-a", GroupRecreateReasonUpgradeSplit)
	GroupRecreated("ns-b", "lws-a", GroupRecreateReasonPodRestart)
	GroupRestartBudgetExhausted("ns-a", "lws-a")
	GroupRestartBackedOff("ns-a", "lws-a")
	GroupRestartBackedOff("ns-a", "lws-a")
	GroupRestartBackedOff("ns-a", "lws-b")

	for _, tc := range []struct {
		name string
		got  float64
		want float64
	}{
		{"recreations ns-a/lws-a PodRestart", testutil.ToFloat64(GroupRecreationsTotal.WithLabelValues("ns-a", "lws-a", GroupRecreateReasonPodRestart)), 2},
		{"recreations ns-a/lws-a UpgradeSplit", testutil.ToFloat64(GroupRecreationsTotal.WithLabelValues("ns-a", "lws-a", GroupRecreateReasonUpgradeSplit)), 1},
		{"recreations ns-b/lws-a PodRestart", testutil.ToFloat64(GroupRecreationsTotal.WithLabelValues("ns-b", "lws-a", GroupRecreateReasonPodRestart)), 1},
		{"budget exhausted ns-a/lws-a", testutil.ToFloat64(GroupRestartBudgetExhaustedTotal.WithLabelValues("ns-a", "lws-a")), 1},
		{"backoffs ns-a/lws-a", testutil.ToFloat64(GroupRestartBackoffsTotal.WithLabelValues("ns-a", "lws-a")), 2},
		{"backoffs ns-a/lws-b", testutil.ToFloat64(GroupRestartBackoffsTotal.WithLabelValues("ns-a", "lws-b")), 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestClearLeaderWorkerSet(t *testing.T) {
	resetAll(t)

	GroupRecreated("ns", "gone", GroupRecreateReasonPodRestart)
	GroupRecreated("ns", "gone", GroupRecreateReasonUpgradeSplit)
	GroupRestartBudgetExhausted("ns", "gone")
	GroupRestartBackedOff("ns", "gone")
	GroupRecreated("ns", "kept", GroupRecreateReasonPodRestart)
	GroupRecreated("other-ns", "gone", GroupRecreateReasonPodRestart)

	ClearLeaderWorkerSet("ns", "gone")

	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"group_recreations_total", testutil.CollectAndCount(GroupRecreationsTotal), 2},
		{"group_restart_budget_exhausted_total", testutil.CollectAndCount(GroupRestartBudgetExhaustedTotal), 0},
		{"group_restart_backoffs_total", testutil.CollectAndCount(GroupRestartBackoffsTotal), 0},
	} {
		if tc.got != tc.want {
			t.Errorf("%s has %d series, want %d", tc.name, tc.got, tc.want)
		}
	}
	if got := testutil.ToFloat64(GroupRecreationsTotal.WithLabelValues("ns", "kept", GroupRecreateReasonPodRestart)); got != 1 {
		t.Errorf("recreations ns/kept = %v, want 1", got)
	}
	if got := testutil.ToFloat64(GroupRecreationsTotal.WithLabelValues("other-ns", "gone", GroupRecreateReasonPodRestart)); got != 1 {
		t.Errorf("recreations other-ns/gone = %v, want 1", got)
	}
}

func TestRegister(t *testing.T) {
	resetAll(t)
	Register()

	GroupRecreated("ns", "lws", GroupRecreateReasonPodRestart)
	GroupRestartBudgetExhausted("ns", "lws")
	GroupRestartBackedOff("ns", "lws")

	names := []string{
		"lws_group_recreations_total",
		"lws_group_restart_budget_exhausted_total",
		"lws_group_restart_backoffs_total",
	}
	count, err := testutil.GatherAndCount(metrics.Registry, names...)
	if err != nil {
		t.Fatalf("gathering from the controller-runtime registry: %v", err)
	}
	if count != len(names) {
		t.Errorf("the controller-runtime registry serves %d series of %v, want %d", count, names, len(names))
	}
}
