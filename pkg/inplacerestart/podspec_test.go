package inplacerestart

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestInjectPodSpec(t *testing.T) {
	tests := []struct {
		name       string
		lws        *leaderworkersetv1.LeaderWorkerSet
		role       leaderworkersetv1.PodRole
		inputSpec  corev1.PodSpec
		verifyFunc func(t *testing.T, spec corev1.PodSpec)
	}{
		{
			name: "no-op when restart policy is not InPlaceGroupRestart",
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						RestartPolicy: leaderworkersetv1.RecreateGroupOnPodRestart,
					},
				},
			},
			role: leaderworkersetv1.WorkerRole,
			inputSpec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "workload"}},
			},
			verifyFunc: func(t *testing.T, spec corev1.PodSpec) {
				if len(spec.InitContainers) != 0 {
					t.Errorf("Expected no init containers, got %d", len(spec.InitContainers))
				}
				if len(spec.Volumes) != 0 {
					t.Errorf("Expected no volumes, got %d", len(spec.Volumes))
				}
			},
		},
		{
			name: "injection successful when policy is InPlaceGroupRestart",
			lws: &leaderworkersetv1.LeaderWorkerSet{
				Spec: leaderworkersetv1.LeaderWorkerSetSpec{
					LeaderWorkerTemplate: leaderworkersetv1.LeaderWorkerTemplate{
						RestartPolicy: leaderworkersetv1.InPlaceGroupRestart,
						InPlaceGroupRestartConfig: &leaderworkersetv1.InPlaceGroupRestartConfig{
							Triggers: []leaderworkersetv1.InPlaceGroupRestartTrigger{
								{
									Role:                 leaderworkersetv1.BothRole,
									ContainerName:        "workload",
									RecoverableExitCodes: []int32{1, 2},
								},
							},
						},
					},
				},
			},
			role: leaderworkersetv1.WorkerRole,
			inputSpec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "workload"}},
			},
			verifyFunc: func(t *testing.T, spec corev1.PodSpec) {
				// 1. Verify Volumes
				if len(spec.Volumes) != 3 {
					t.Errorf("Expected 3 volumes injected, got %d", len(spec.Volumes))
				}
				volumeNames := map[string]bool{}
				for _, v := range spec.Volumes {
					volumeNames[v.Name] = true
				}
				if !volumeNames[StateVolumeName] || !volumeNames[ApiVolumeName] || !volumeNames[EmptyDirNoTokenVolume] {
					t.Errorf("Missing expected volumes: %v", volumeNames)
				}

				// 2. Verify InitContainers
				if len(spec.InitContainers) != 3 {
					t.Errorf("Expected 3 init containers injected, got %d", len(spec.InitContainers))
				}
				if spec.InitContainers[0].Name != MarkerContainerName {
					t.Errorf("Expected first init to be %s", MarkerContainerName)
				}
				if spec.InitContainers[1].Name != AgentContainerName {
					t.Errorf("Expected second init to be %s", AgentContainerName)
				}
				if spec.InitContainers[2].Name != BarrierContainerName {
					t.Errorf("Expected third init to be %s", BarrierContainerName)
				}

				// 3. Verify Agent Restart Policy
				agent := spec.InitContainers[1]
				if agent.RestartPolicy == nil || *agent.RestartPolicy != corev1.ContainerRestartPolicyAlways {
					t.Errorf("Agent must be a restartable init container")
				}
				if len(agent.RestartPolicyRules) != 1 || agent.RestartPolicyRules[0].Action != corev1.ContainerRestartRuleActionRestartAllContainers {
					t.Errorf("Agent missing exit 88 mapping to RestartAllContainers")
				}

				// 4. Verify Workload Injection
				workload := spec.Containers[0]
				if len(workload.RestartPolicyRules) == 0 {
					t.Fatalf("Workload missing injected trigger rules")
				}
				if diff := cmp.Diff(workload.RestartPolicyRules[0].ExitCodes.Values, []int32{1, 2}); diff != "" {
					t.Errorf("Injected trigger exit codes mismatch (-want +got):\n%s", diff)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			InjectPodSpec(&tt.inputSpec, tt.lws, tt.role)
			tt.verifyFunc(t, tt.inputSpec)
		})
	}
}
