package inplacerestart

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	AgentImage            = "lws-restart-helper:latest"
	StateVolumeName       = "in-place-restart-state"
	ApiVolumeName         = "lws-api"
	StateVolumeMountPath  = "/var/run/lws-state"
	ApiVolumeMountPath    = "/var/run/lws-api"
	MarkerContainerName   = "lws-restart-marker"
	AgentContainerName    = "lws-restart-agent"
	BarrierContainerName  = "lws-restart-barrier"
	NoServiceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount"
	EmptyDirNoTokenVolume = "no-sa-token"
)

// InjectPodSpec modifies the PodSpec to support InPlaceGroupRestart.
func InjectPodSpec(spec *corev1.PodSpec, lws *leaderworkersetv1.LeaderWorkerSet, role leaderworkersetv1.PodRole) {
	if lws.Spec.LeaderWorkerTemplate.RestartPolicy != leaderworkersetv1.InPlaceGroupRestart {
		return
	}

	// 1. Add state volume (emptyDir)
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: StateVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	// 2. Add Downward API volume
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: ApiVolumeName,
		VolumeSource: corev1.VolumeSource{
			DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: []corev1.DownwardAPIVolumeFile{
					{
						Path: "desired-restart-generation",
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.annotations['" + leaderworkersetv1.DesiredRestartGenerationAnnotationKey + "']",
						},
					},
					{
						Path: "barrier-open",
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.annotations['" + leaderworkersetv1.BarrierOpenAnnotationKey + "']",
						},
					},
				},
			},
		},
	})

	// 3. Add volume to mask service account token
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: EmptyDirNoTokenVolume,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	commonMounts := []corev1.VolumeMount{
		{Name: StateVolumeName, MountPath: StateVolumeMountPath},
		{Name: ApiVolumeName, MountPath: ApiVolumeMountPath},
		{Name: EmptyDirNoTokenVolume, MountPath: NoServiceAccountToken},
	}

	// 4. Create Init Containers (Marker, Agent, Barrier)
	marker := corev1.Container{
		Name:            MarkerContainerName,
		Image:           AgentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"marker"},
		VolumeMounts:    commonMounts,
	}

	agent := corev1.Container{
		Name:            AgentContainerName,
		Image:           AgentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"agent"},
		VolumeMounts:    commonMounts,
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways), // restartable init container
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/ready",
					Port: intstr.FromInt32(8080),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
		},
		RestartPolicyRules: []corev1.ContainerRestartRule{
			{
				Action: corev1.ContainerRestartRuleActionRestartAllContainers,
				ExitCodes: &corev1.ContainerRestartRuleOnExitCodes{
					Values: []int32{88},
				},
			},
		},
	}

	barrier := corev1.Container{
		Name:            BarrierContainerName,
		Image:           AgentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"barrier"},
		VolumeMounts:    commonMounts,
	}

	spec.InitContainers = append(spec.InitContainers, marker, agent, barrier)

	// 5. Inject triggers into workload containers
	if lws.Spec.LeaderWorkerTemplate.InPlaceGroupRestartConfig != nil {
		for _, trigger := range lws.Spec.LeaderWorkerTemplate.InPlaceGroupRestartConfig.Triggers {
			if trigger.Role == role || trigger.Role == leaderworkersetv1.BothRole {
				for i := range spec.Containers {
					if spec.Containers[i].Name == trigger.ContainerName {
						spec.Containers[i].RestartPolicyRules = append(spec.Containers[i].RestartPolicyRules, corev1.ContainerRestartRule{
							Action: corev1.ContainerRestartRuleActionRestartAllContainers,
							ExitCodes: &corev1.ContainerRestartRuleOnExitCodes{
								Values: trigger.RecoverableExitCodes,
							},
						})
					}
				}
				for i := range spec.InitContainers {
					if spec.InitContainers[i].Name == trigger.ContainerName {
						spec.InitContainers[i].RestartPolicyRules = append(spec.InitContainers[i].RestartPolicyRules, corev1.ContainerRestartRule{
							Action: corev1.ContainerRestartRuleActionRestartAllContainers,
							ExitCodes: &corev1.ContainerRestartRuleOnExitCodes{
								Values: trigger.RecoverableExitCodes,
							},
						})
					}
				}
			}
		}
	}
}
