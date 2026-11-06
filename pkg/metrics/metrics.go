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

// Package metrics defines the Prometheus metrics of the LWS controller manager.
// They are served next to the controller-runtime metrics on the manager's
// metrics endpoint.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const subsystem = "lws"

// Reasons for a group recreation, the values of the reason label of
// GroupRecreationsTotal.
const (
	// GroupRecreateReasonPodRestart is a recreation by the restart policy
	// (RecreateGroupOnPodRestart or RecreateGroupAfterStart) after a container
	// of the group restarted or a pod of the group was deleted.
	GroupRecreateReasonPodRestart = "PodRestart"
	// GroupRecreateReasonUpgradeSplit is a recreation of a group that an LWS
	// upgrade split across PodGroups.
	GroupRecreateReasonUpgradeSplit = "UpgradeSplit"
)

var (
	// GroupRecreationsTotal counts the groups the pod controller recreated by
	// deleting their leader pod.
	GroupRecreationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "group_recreations_total",
			Help: "The number of groups the pod controller recreated by deleting their leader pod, by reason: " +
				"PodRestart (the restart policy, after a container of the group restarted or a pod of the group was deleted) " +
				"or UpgradeSplit (an LWS upgrade split the group across PodGroups).",
		}, []string{"namespace", "name", "reason"},
	)

	// GroupRestartBudgetExhaustedTotal counts the groups the pod controller
	// terminated because they used up spec.leaderWorkerTemplate.maxGroupRestarts.
	GroupRestartBudgetExhaustedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "group_restart_budget_exhausted_total",
			Help: "The number of groups the pod controller terminated because they used up their restart budget " +
				"(maxGroupRestarts). Such a group stays down until it is recovered explicitly.",
		}, []string{"namespace", "name"},
	)

	// GroupRestartBackoffsTotal counts the group recreations the pod controller
	// delayed because of spec.leaderWorkerTemplate.restartBackoff.
	GroupRestartBackoffsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "group_restart_backoffs_total",
			Help:      "The number of group recreations the pod controller delayed because of restartBackoff.",
		}, []string{"namespace", "name"},
	)
)

// GroupRecreated records a group of the LeaderWorkerSet namespace/name
// recreated for the given reason.
func GroupRecreated(namespace, name, reason string) {
	GroupRecreationsTotal.WithLabelValues(namespace, name, reason).Inc()
}

// GroupRestartBudgetExhausted records a group of the LeaderWorkerSet
// namespace/name terminated because its restart budget is used up.
func GroupRestartBudgetExhausted(namespace, name string) {
	GroupRestartBudgetExhaustedTotal.WithLabelValues(namespace, name).Inc()
}

// GroupRestartBackedOff records a recreation of a group of the
// LeaderWorkerSet namespace/name delayed by restartBackoff.
func GroupRestartBackedOff(namespace, name string) {
	GroupRestartBackoffsTotal.WithLabelValues(namespace, name).Inc()
}

// ClearLeaderWorkerSet removes the series of the LeaderWorkerSet
// namespace/name, so a deleted LeaderWorkerSet does not keep them until the
// manager restarts.
func ClearLeaderWorkerSet(namespace, name string) {
	labels := prometheus.Labels{"namespace": namespace, "name": name}
	GroupRecreationsTotal.DeletePartialMatch(labels)
	GroupRestartBudgetExhaustedTotal.DeletePartialMatch(labels)
	GroupRestartBackoffsTotal.DeletePartialMatch(labels)
}

// Register registers the LWS metrics with the controller-runtime registry,
// which the manager serves on its metrics endpoint.
func Register() {
	metrics.Registry.MustRegister(
		GroupRecreationsTotal,
		GroupRestartBudgetExhaustedTotal,
		GroupRestartBackoffsTotal,
	)
}
