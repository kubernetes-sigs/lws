package inplacerestart

import (
	"context"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

func HandleInPlaceGroupRestart(ctx context.Context, c client.Client, rec events.EventRecorder, lws *leaderworkersetv1.LeaderWorkerSet, leaderPod *corev1.Pod) (ctrl.Result, bool, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("leaderPod", leaderPod.Name)

	val := leaderPod.Annotations[leaderworkersetv1.InPlaceRestartStateAnnotationKey]
	if val == "" {
		return ctrl.Result{}, false, nil
	}

	state, err := UnmarshalState(val)
	if err != nil {
		log.Error(err, "Failed to unmarshal InPlaceRestartState")
		return ctrl.Result{}, false, nil
	}

	if state.Phase == Idle {
		return ctrl.Result{}, false, nil
	}

	config := lws.Spec.LeaderWorkerTemplate.InPlaceGroupRestartConfig
	recoveryTimeout := 5 * time.Minute
	if config != nil && config.RecoveryTimeoutSeconds != 0 {
		recoveryTimeout = time.Duration(config.RecoveryTimeoutSeconds) * time.Second
	}

	if time.Since(state.AttemptStartedAt) > recoveryTimeout {
		log.Info("RecoveryTimeout exceeded. Escalating to recreate group.")
		return escalate(ctx, c, rec, lws, leaderPod, "RecoveryTimeoutExceeded")
	}

	// Fetch all pods in the group
	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(leaderPod.Namespace), client.MatchingLabels{
		leaderworkersetv1.SetNameLabelKey:    lws.Name,
		leaderworkersetv1.GroupIndexLabelKey: leaderPod.Labels[leaderworkersetv1.GroupIndexLabelKey],
	}); err != nil {
		return ctrl.Result{}, false, err
	}

	pods := podList.Items

	// Validate MaxAttempts
	if state.Phase != Idle && config != nil && config.MaxAttempts > 0 {
		if !state.WindowStartedAt.IsZero() && time.Since(state.WindowStartedAt) <= time.Duration(config.WindowSeconds)*time.Second {
			if state.AttemptsWithinWindow >= int(config.MaxAttempts) {
				log.Info("MaxAttempts exceeded within Window. Escalating.")
				return escalate(ctx, c, rec, lws, leaderPod, "MaxAttemptsExceeded")
			}
		}
	}

	// Validate Topology Changes (if topology changed mid-recovery, escalate)
	if state.Phase != Idle {
		expectedSize := 1 // leader
		if lws.Spec.LeaderWorkerTemplate.Size != nil {
			expectedSize += int(*lws.Spec.LeaderWorkerTemplate.Size)
		}
		if state.ExpectedGroupSize > 0 && state.ExpectedGroupSize != expectedSize {
			log.Info("Group membership changed during recovery. Escalating.")
			return escalate(ctx, c, rec, lws, leaderPod, "TopologyMembershipChanged")
		}
	}

	switch state.Phase {
	case Quiescing:
		return handleQuiescing(ctx, c, leaderPod, pods, &state, log)
	case Signaling:
		return handleSignaling(ctx, c, leaderPod, pods, &state, log)
	case WaitingForAcknowledgements:
		return handleWaitingForAcknowledgements(ctx, c, leaderPod, pods, &state, log)
	case OpeningBarrier:
		return handleOpeningBarrier(ctx, c, leaderPod, pods, &state, log)
	case WaitingForReadiness:
		return handleWaitingForReadiness(ctx, c, lws, leaderPod, pods, &state, log)
	}

	return ctrl.Result{}, false, nil
}

func updatePodsAnnotation(ctx context.Context, c client.Client, pods []corev1.Pod, key, val string) error {
	for _, p := range pods {
		if p.Annotations == nil {
			p.Annotations = make(map[string]string)
		}
		if p.Annotations[key] != val {
			p.Annotations[key] = val
			if err := c.Update(ctx, &p); err != nil {
				return err
			}
		}
	}
	return nil
}

func updateState(ctx context.Context, c client.Client, leaderPod *corev1.Pod, state *InPlaceRestartState) (ctrl.Result, bool, error) {
	stateStr, _ := MarshalState(*state)
	if leaderPod.Annotations == nil {
		leaderPod.Annotations = make(map[string]string)
	}
	leaderPod.Annotations[leaderworkersetv1.InPlaceRestartStateAnnotationKey] = stateStr
	err := c.Update(ctx, leaderPod)
	return ctrl.Result{}, true, err
}

func handleQuiescing(ctx context.Context, c client.Client, leaderPod *corev1.Pod, pods []corev1.Pod, state *InPlaceRestartState, log logr.Logger) (ctrl.Result, bool, error) {
	log.Info("State: Quiescing")
	if err := updatePodsAnnotation(ctx, c, pods, leaderworkersetv1.BarrierOpenAnnotationKey, "false"); err != nil {
		return ctrl.Result{}, true, err
	}

	allQuiesced := true
	for _, p := range pods {
		if !podutils.PodDeleted(p) && podutils.IsPodReady(&p) {
			allQuiesced = false
			break
		}
	}

	if allQuiesced {
		state.Phase = Signaling
		state.DesiredGeneration = state.CurrentGeneration + 1
		return updateState(ctx, c, leaderPod, state)
	}

	return ctrl.Result{RequeueAfter: 2 * time.Second}, true, nil
}

func handleSignaling(ctx context.Context, c client.Client, leaderPod *corev1.Pod, pods []corev1.Pod, state *InPlaceRestartState, log logr.Logger) (ctrl.Result, bool, error) {
	log.Info("State: Signaling")
	genStr := strconv.Itoa(state.DesiredGeneration)
	if err := updatePodsAnnotation(ctx, c, pods, leaderworkersetv1.DesiredRestartGenerationAnnotationKey, genStr); err != nil {
		return ctrl.Result{}, true, err
	}

	state.Phase = WaitingForAcknowledgements
	return updateState(ctx, c, leaderPod, state)
}

func handleWaitingForAcknowledgements(ctx context.Context, c client.Client, leaderPod *corev1.Pod, pods []corev1.Pod, state *InPlaceRestartState, log logr.Logger) (ctrl.Result, bool, error) {
	log.Info("State: WaitingForAcknowledgements")
	allAcknowledged := true
	for _, p := range pods {
		if podutils.PodDeleted(p) {
			continue
		}
		atBarrier := false
		for _, ic := range p.Status.InitContainerStatuses {
			if ic.Name == BarrierContainerName {
				if ic.State.Running != nil {
					atBarrier = true
					break
				}
			}
		}
		if !atBarrier {
			allAcknowledged = false
			break
		}
	}

	if allAcknowledged {
		state.Phase = OpeningBarrier
		return updateState(ctx, c, leaderPod, state)
	}

	return ctrl.Result{RequeueAfter: 2 * time.Second}, true, nil
}

func handleOpeningBarrier(ctx context.Context, c client.Client, leaderPod *corev1.Pod, pods []corev1.Pod, state *InPlaceRestartState, log logr.Logger) (ctrl.Result, bool, error) {
	log.Info("State: OpeningBarrier")
	if err := updatePodsAnnotation(ctx, c, pods, leaderworkersetv1.BarrierOpenAnnotationKey, "true"); err != nil {
		return ctrl.Result{}, true, err
	}

	state.Phase = WaitingForReadiness
	return updateState(ctx, c, leaderPod, state)
}

func handleWaitingForReadiness(ctx context.Context, c client.Client, lws *leaderworkersetv1.LeaderWorkerSet, leaderPod *corev1.Pod, pods []corev1.Pod, state *InPlaceRestartState, log logr.Logger) (ctrl.Result, bool, error) {
	log.Info("State: WaitingForReadiness")
	allReady := true
	for _, p := range pods {
		if !podutils.PodDeleted(p) && !podutils.IsPodReady(&p) {
			allReady = false
			break
		}
	}

	if allReady {
		log.Info("Group successfully restarted in-place.")
		state.Phase = Idle
		state.CurrentGeneration = state.DesiredGeneration

		// Update attempts history
		config := lws.Spec.LeaderWorkerTemplate.InPlaceGroupRestartConfig
		if config != nil {
			if state.WindowStartedAt.IsZero() || time.Since(state.WindowStartedAt) > time.Duration(config.WindowSeconds)*time.Second {
				state.WindowStartedAt = time.Now()
				state.AttemptsWithinWindow = 1
			} else {
				state.AttemptsWithinWindow++
			}
		}

		ClearAttemptState(state)
		return updateState(ctx, c, leaderPod, state)
	}

	return ctrl.Result{RequeueAfter: 2 * time.Second}, true, nil
}

func escalate(ctx context.Context, c client.Client, rec events.EventRecorder, lws *leaderworkersetv1.LeaderWorkerSet, leaderPod *corev1.Pod, reason string) (ctrl.Result, bool, error) {
	if err := c.Delete(ctx, leaderPod); err != nil {
		return ctrl.Result{}, true, client.IgnoreNotFound(err)
	}
	rec.Eventf(lws, leaderPod, corev1.EventTypeWarning, "GroupRecreated", "RecreateGroup", "Group escalated to recreation due to: %s", reason)
	return ctrl.Result{}, true, nil
}
