package inplacerestart

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// InitiateRestartFromWorker tries to transition the leader's state from Idle to Quiescing atomically.
func InitiateRestartFromWorker(ctx context.Context, c client.Client, lws *leaderworkersetv1.LeaderWorkerSet, leaderPod *corev1.Pod, triggeringWorkerUID string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latestLeader corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: leaderPod.Name, Namespace: leaderPod.Namespace}, &latestLeader); err != nil {
			return err
		}

		val := latestLeader.Annotations[leaderworkersetv1.InPlaceRestartStateAnnotationKey]
		var state InPlaceRestartState
		if val != "" {
			var err error
			state, err = UnmarshalState(val)
			if err != nil {
				return nil // Malformed state handled by leader
			}
			if state.Phase != Idle {
				return nil // Already recovering
			}
		} else {
			state = InPlaceRestartState{
				Phase:             Idle,
				CurrentGeneration: 0,
				DesiredGeneration: 0,
			}
		}

		state.Phase = Quiescing
		state.AttemptStartedAt = time.Now()
		state.TriggerPodUID = triggeringWorkerUID

		if latestLeader.Annotations == nil {
			latestLeader.Annotations = make(map[string]string)
		}

		stateStr, _ := MarshalState(state)
		latestLeader.Annotations[leaderworkersetv1.InPlaceRestartStateAnnotationKey] = stateStr

		return c.Update(ctx, &latestLeader)
	})
}
