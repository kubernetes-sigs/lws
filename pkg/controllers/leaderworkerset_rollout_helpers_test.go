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

package controllers

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	appsapplyv1 "k8s.io/client-go/applyconfigurations/apps/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/utils"
)

// The helpers in this file keep the signatures that the rollout functions had
// before the availability floor. The table tests that were written against
// those signatures call them, so that the tests stay unchanged. The controller
// calls planRollingUpdate, applyLeaderStatefulSet, applyLeaderDeployment,
// getReplicaStatesAndLeaders, rollingUpdatePartitionWithFloor, and
// constructLeaderStatefulSetApplyConfigurationWithBaseline.

// rollingUpdateParameters returns the partition and the replica count of the
// rolling update plan.
func (r *LeaderWorkerSetReconciler) rollingUpdateParameters(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, sts *appsv1.StatefulSet, revisionKey string, leaderWorkerSetUpdated bool) (stsPartition int32, replicas int32, err error) {
	plan, err := r.planRollingUpdate(ctx, lws, sts, revisionKey, leaderWorkerSetUpdated)
	if err != nil {
		return 0, 0, err
	}
	return plan.partition, plan.replicas, nil
}

// SSAWithStatefulset applies the leader StatefulSet with no rollout baseline.
func (r *LeaderWorkerSetReconciler) SSAWithStatefulset(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32, revisionKey string) error {
	return r.applyLeaderStatefulSet(ctx, lws, partition, replicas, nil, revisionKey)
}

// SSAWithDeployment applies the leader Deployment with no rollout baseline.
func (r *LeaderWorkerSetReconciler) SSAWithDeployment(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, revisionKey string) error {
	return r.applyLeaderDeployment(ctx, lws, nil, revisionKey)
}

// getReplicaStates returns the replica states. It passes no leader StatefulSet,
// so the read does not check the owner or the StatefulSet revision of the
// leader pods.
func (r *LeaderWorkerSetReconciler) getReplicaStates(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, stsReplicas int32, revisionKey string) ([]replicaState, error) {
	states, _, _, err := r.getReplicaStatesAndLeaders(ctx, lws, nil, stsReplicas, revisionKey)
	return states, err
}

// rollingUpdatePartition converts a rolling step to the floor
// stsReplicas - rollingStep, with no user partition.
func rollingUpdatePartition(states []replicaState, stsReplicas int32, rollingStep int32, currentPartition int32) int32 {
	floor := utils.NonZeroValue(stsReplicas - rollingStep)
	activeStates := states[:min(len(states), int(stsReplicas))]
	return rollingUpdatePartitionWithFloor(activeStates, floor, min(currentPartition, stsReplicas), 0)
}

// constructLeaderStatefulSetApplyConfiguration constructs the applied
// configuration for the leader StatefulSet with no rollout baseline.
func constructLeaderStatefulSetApplyConfiguration(lws *leaderworkerset.LeaderWorkerSet, partition, replicas int32, revisionKey string) (*appsapplyv1.StatefulSetApplyConfiguration, error) {
	return constructLeaderStatefulSetApplyConfigurationWithBaseline(lws, partition, replicas, nil, revisionKey)
}
