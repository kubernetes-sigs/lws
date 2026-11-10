package webhooks

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const exclusiveTestTopologyKey = "topology.kubernetes.io/zone"

func exclusiveTestPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				leaderworkerset.SetNameLabelKey:         "test",
				leaderworkerset.GroupUniqueHashLabelKey: "default-test-0",
			},
		},
	}
}

// A user-declared affinity pair on the same topology key must not be mistaken
// for the exclusive placement of the group.
func TestSetExclusiveAffinitiesPreservesUserTerms(t *testing.T) {
	pod := exclusiveTestPod()
	pod.Spec.Affinity = &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "cache"}},
				TopologyKey:   exclusiveTestTopologyKey,
			}},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "cache"}},
				TopologyKey:   exclusiveTestTopologyKey,
			}},
		},
	}

	SetExclusiveAffinities(pod, "default-test-0", exclusiveTestTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)

	if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
		t.Fatalf("pod affinity terms = %d, want 2 (the user's plus the injected one)", got)
	}
	if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
		t.Fatalf("pod anti-affinity terms = %d, want 2 (the user's plus the injected one)", got)
	}
	injected := pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution[1]
	if len(injected.LabelSelector.MatchExpressions) != 2 {
		t.Fatalf("injected anti-affinity selector = %#v, want the two group-key expressions", injected.LabelSelector)
	}
}

// Re-applying the placement stays idempotent.
func TestSetExclusiveAffinitiesIsIdempotent(t *testing.T) {
	pod := exclusiveTestPod()
	SetExclusiveAffinities(pod, "default-test-0", exclusiveTestTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)
	SetExclusiveAffinities(pod, "default-test-0", exclusiveTestTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)

	if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
		t.Errorf("pod affinity terms = %d, want 1", got)
	}
	if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
		t.Errorf("pod anti-affinity terms = %d, want 1", got)
	}
}

// A pod that already carries a same-key term with a foreign selector only
// half-matches, so the placement is still injected.
func TestSetExclusiveAffinitiesRepairsPartialPlacement(t *testing.T) {
	pod := exclusiveTestPod()
	pod.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "cache"}},
				TopologyKey:   exclusiveTestTopologyKey,
			}},
		},
	}

	SetExclusiveAffinities(pod, "default-test-0", exclusiveTestTopologyKey, leaderworkerset.GroupUniqueHashLabelKey)

	if got := len(pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 1 {
		t.Errorf("pod affinity terms = %d, want 1", got)
	}
	if got := len(pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution); got != 2 {
		t.Errorf("pod anti-affinity terms = %d, want 2", got)
	}
}
