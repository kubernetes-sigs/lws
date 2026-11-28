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

// Reasons for a DisaggregatedSet rollout step past maxSurge or maxUnavailable,
// the values of the reason label of DisaggregatedSetRecoveryStepsTotal.
const (
	// RecoveryStepReasonBootstrapSurge is a step that created a target replica
	// without a free maxSurge slot, so that the target revision has every role.
	RecoveryStepReasonBootstrapSurge = "BootstrapSurge"
	// RecoveryStepReasonAvailabilityFallback is a step that retired old replicas
	// past maxUnavailable to free capacity for unschedulable target Pods.
	RecoveryStepReasonAvailabilityFallback = "AvailabilityFallback"
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

	// DisaggregatedSetRolloutsStartedTotal counts the slices of a
	// DisaggregatedSet that started rolling to a new revision.
	DisaggregatedSetRolloutsStartedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "disaggregatedset_rollouts_started_total",
			Help:      "The number of DisaggregatedSet slices that started rolling to a new revision.",
		}, []string{"namespace", "name"},
	)

	// DisaggregatedSetRolloutsCompletedTotal counts the slice rollouts whose old
	// revisions are drained and whose target revision is Ready.
	DisaggregatedSetRolloutsCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "disaggregatedset_rollouts_completed_total",
			Help:      "The number of DisaggregatedSet slice rollouts that completed: the old revisions are drained and the target revision is Ready.",
		}, []string{"namespace", "name"},
	)

	// DisaggregatedSetRecoveryStepsTotal counts the DisaggregatedSet rollout
	// steps that went past maxSurge or maxUnavailable to unblock a rollout.
	DisaggregatedSetRecoveryStepsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      "disaggregatedset_recovery_steps_total",
			Help: "The number of DisaggregatedSet rollout steps that went past maxSurge or maxUnavailable to unblock the rollout, by reason: " +
				"BootstrapSurge (a target replica created without a free maxSurge slot) " +
				"or AvailabilityFallback (old replicas retired past maxUnavailable to release capacity for unschedulable target Pods).",
		}, []string{"namespace", "name", "reason"},
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

// InitLeaderWorkerSet creates the series of the LeaderWorkerSet namespace/name
// at 0, so that rate() and increase() see its first recreation, exhaustion or
// backoff.
func InitLeaderWorkerSet(namespace, name string) {
	GroupRecreationsTotal.WithLabelValues(namespace, name, GroupRecreateReasonPodRestart)
	GroupRecreationsTotal.WithLabelValues(namespace, name, GroupRecreateReasonUpgradeSplit)
	GroupRestartBudgetExhaustedTotal.WithLabelValues(namespace, name)
	GroupRestartBackoffsTotal.WithLabelValues(namespace, name)
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

// DisaggregatedSetRolloutStarted records a slice of the DisaggregatedSet
// namespace/name that started rolling to a new revision.
func DisaggregatedSetRolloutStarted(namespace, name string) {
	DisaggregatedSetRolloutsStartedTotal.WithLabelValues(namespace, name).Inc()
}

// DisaggregatedSetRolloutCompleted records a completed slice rollout of the
// DisaggregatedSet namespace/name.
func DisaggregatedSetRolloutCompleted(namespace, name string) {
	DisaggregatedSetRolloutsCompletedTotal.WithLabelValues(namespace, name).Inc()
}

// DisaggregatedSetRecoveryStep records a rollout step of the DisaggregatedSet
// namespace/name that went past maxSurge or maxUnavailable for the given reason.
func DisaggregatedSetRecoveryStep(namespace, name, reason string) {
	DisaggregatedSetRecoveryStepsTotal.WithLabelValues(namespace, name, reason).Inc()
}

// InitDisaggregatedSet creates the series of the DisaggregatedSet namespace/name
// at 0, so that rate() and increase() see its first rollout or recovery step.
func InitDisaggregatedSet(namespace, name string) {
	DisaggregatedSetRolloutsStartedTotal.WithLabelValues(namespace, name)
	DisaggregatedSetRolloutsCompletedTotal.WithLabelValues(namespace, name)
	DisaggregatedSetRecoveryStepsTotal.WithLabelValues(namespace, name, RecoveryStepReasonBootstrapSurge)
	DisaggregatedSetRecoveryStepsTotal.WithLabelValues(namespace, name, RecoveryStepReasonAvailabilityFallback)
}

// ClearDisaggregatedSet removes the series of the DisaggregatedSet namespace/name,
// so a deleted DisaggregatedSet does not keep them until the manager restarts.
func ClearDisaggregatedSet(namespace, name string) {
	labels := prometheus.Labels{"namespace": namespace, "name": name}
	DisaggregatedSetRolloutsStartedTotal.DeletePartialMatch(labels)
	DisaggregatedSetRolloutsCompletedTotal.DeletePartialMatch(labels)
	DisaggregatedSetRecoveryStepsTotal.DeletePartialMatch(labels)
}

// Register registers the LWS metrics with the controller-runtime registry,
// which the manager serves on its metrics endpoint.
func Register() {
	metrics.Registry.MustRegister(
		GroupRecreationsTotal,
		GroupRestartBudgetExhaustedTotal,
		GroupRestartBackoffsTotal,
		DisaggregatedSetRolloutsStartedTotal,
		DisaggregatedSetRolloutsCompletedTotal,
		DisaggregatedSetRecoveryStepsTotal,
	)
}
