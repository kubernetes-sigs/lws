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

package upgrade

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	testutils "sigs.k8s.io/lws/test/testutils"
	"sigs.k8s.io/lws/test/wrappers"
)

const (
	testNamespace   = "lws-upgrade-test"
	existingLWSName = "existing-lws"
	existingDSName  = "existing-ds"
	newLWSName      = "new-lws"
	newDSName       = "new-ds"
	pauseImage      = "registry.k8s.io/pause:3.10"
)

type objectSnapshot struct {
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type generatedLWSSnapshot struct {
	Name     string    `json:"name"`
	UID      types.UID `json:"uid"`
	Role     string    `json:"role"`
	Revision string    `json:"revision"`
}

type podSnapshot struct {
	Name                  string           `json:"name"`
	UID                   types.UID        `json:"uid"`
	ContainerRestarts     map[string]int32 `json:"containerRestarts"`
	InitContainerRestarts map[string]int32 `json:"initContainerRestarts"`
}

type upgradeSnapshot struct {
	LeaderWorkerSet  objectSnapshot         `json:"leaderWorkerSet"`
	DisaggregatedSet objectSnapshot         `json:"disaggregatedSet"`
	GeneratedLWS     []generatedLWSSnapshot `json:"generatedLWS"`
	WorkloadPods     []podSnapshot          `json:"workloadPods"`
}

var _ = ginkgo.Describe("Controller upgrade", ginkgo.Ordered, func() {
	ginkgo.It("preserves existing workloads and reconciles new workloads", func() {
		switch upgradePhase {
		case "before":
			createExistingWorkloads()
			writeSnapshot(captureSnapshot())
		case "after":
			expected := readSnapshot()
			expectCurrentControllerReady()
			gomega.Consistently(func(g gomega.Gomega) {
				current, err := captureSnapshot()
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(current).To(gomega.Equal(expected))
			}, 30*time.Second, time.Second).Should(gomega.Succeed())
			createNewWorkloads()
		}
	})
})

func waitForWebhooks() {
	ginkgo.By("waiting for the LeaderWorkerSet webhook")
	gomega.Eventually(func() error {
		probe := newLeaderWorkerSet("default", "upgrade-lws-webhook-probe", 1, 1)
		return k8sClient.Create(ctx, probe, client.DryRunAll)
	}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())

	ginkgo.By("waiting for the DisaggregatedSet webhook")
	gomega.Eventually(func() error {
		probe := newDisaggregatedSet("default", "upgrade-ds-webhook-probe")
		return k8sClient.Create(ctx, probe, client.DryRunAll)
	}, 2*time.Minute, 2*time.Second).Should(gomega.Succeed())
}

func createExistingWorkloads() {
	ginkgo.By("creating the upgrade test namespace")
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
	gomega.Expect(k8sClient.Create(ctx, namespace)).To(gomega.Succeed())

	ginkgo.By("creating a LeaderWorkerSet with the old controller")
	lws := newLeaderWorkerSet(testNamespace, existingLWSName, 2, 2)
	testutils.MustCreateLws(ctx, k8sClient, lws)
	testutils.ExpectValidLeaderStatefulSet(ctx, k8sClient, lws, 2)
	testutils.ExpectValidWorkerStatefulSets(ctx, lws, k8sClient, true)
	testutils.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "")
	testutils.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})
	testutils.ExpectValidServices(ctx, k8sClient, lws, 1)

	ginkgo.By("creating a DisaggregatedSet with the old controller")
	ds := newDisaggregatedSet(testNamespace, existingDSName)
	gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())
	waitForDisaggregatedSet(existingDSName)
}

func createNewWorkloads() {
	ginkgo.By("creating a new LeaderWorkerSet with the upgraded controller")
	lws := newLeaderWorkerSet(testNamespace, newLWSName, 1, 2)
	testutils.MustCreateLws(ctx, k8sClient, lws)
	testutils.ExpectLeaderWorkerSetAvailable(ctx, k8sClient, lws, "")
	testutils.ExpectValidPods(ctx, k8sClient, lws, &corev1.PodList{})

	ginkgo.By("creating a new DisaggregatedSet with the upgraded controller")
	ds := newDisaggregatedSet(testNamespace, newDSName)
	gomega.Expect(k8sClient.Create(ctx, ds)).To(gomega.Succeed())
	waitForDisaggregatedSet(newDSName)
}

func newLeaderWorkerSet(namespace, name string, replicas, size int) *leaderworkersetv1.LeaderWorkerSet {
	return wrappers.BuildLeaderWorkerSet(namespace).
		Name(name).
		Replica(replicas).
		Size(size).
		RestartPolicy(leaderworkersetv1.RecreateGroupOnPodRestart).
		LeaderTemplateSpec(pausePodSpec()).
		WorkerTemplateSpec(pausePodSpec()).
		Obj()
}

func newDisaggregatedSet(namespace, name string) *disaggregatedsetv1.DisaggregatedSet {
	ds := wrappers.BuildDisaggregatedSet(name, namespace).
		UID("").
		WithRole("prefill", 1, pauseImage).
		WithRole("decode", 1, pauseImage).
		Obj()
	for i := range ds.Spec.Roles {
		ds.Spec.Roles[i].Spec.StartupPolicy = leaderworkersetv1.LeaderCreatedStartupPolicy
		ds.Spec.Roles[i].Spec.RolloutStrategy.Type = leaderworkersetv1.RollingUpdateStrategyType
	}
	return ds
}

func pausePodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:            "main",
			Image:           pauseImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "scratch",
				MountPath: "/scratch",
			}},
		}},
		Volumes: []corev1.Volume{{
			Name: "scratch",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}},
	}
}

func waitForDisaggregatedSet(name string) {
	gomega.Eventually(func() error {
		generated := &leaderworkersetv1.LeaderWorkerSetList{}
		if err := k8sClient.List(ctx, generated,
			client.InNamespace(testNamespace),
			client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: name},
		); err != nil {
			return err
		}
		if len(generated.Items) != 2 {
			return fmt.Errorf("expected 2 generated LeaderWorkerSets, got %d", len(generated.Items))
		}

		roles := make(map[string]bool, 2)
		for _, lws := range generated.Items {
			role := lws.Labels[disaggregatedsetv1.RoleLabelKey]
			if role == "" || lws.Labels[disaggregatedsetv1.RevisionLabelKey] == "" {
				return fmt.Errorf("generated LeaderWorkerSet %q is missing role or revision labels", lws.Name)
			}
			if lws.Status.ReadyReplicas != 1 {
				return fmt.Errorf("generated LeaderWorkerSet %q has %d ready replicas", lws.Name, lws.Status.ReadyReplicas)
			}
			roles[role] = true
		}
		if !roles["prefill"] || !roles["decode"] {
			return fmt.Errorf("expected prefill and decode roles, got %v", roles)
		}

		pods := &corev1.PodList{}
		if err := k8sClient.List(ctx, pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: name},
		); err != nil {
			return err
		}
		if len(pods.Items) != 2 {
			return fmt.Errorf("expected 2 DisaggregatedSet pods, got %d", len(pods.Items))
		}
		for _, pod := range pods.Items {
			if !isPodReady(&pod) {
				return fmt.Errorf("pod %q is not ready", pod.Name)
			}
		}

		services := &corev1.ServiceList{}
		if err := k8sClient.List(ctx, services,
			client.InNamespace(testNamespace),
			client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: name},
		); err != nil {
			return err
		}
		if len(services.Items) != 2 {
			return fmt.Errorf("expected 2 DisaggregatedSet services, got %d", len(services.Items))
		}
		return nil
	}, 3*time.Minute, time.Second).Should(gomega.Succeed())
}

func captureSnapshot() (upgradeSnapshot, error) {
	lws := &leaderworkersetv1.LeaderWorkerSet{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: existingLWSName}, lws); err != nil {
		return upgradeSnapshot{}, err
	}

	ds := &disaggregatedsetv1.DisaggregatedSet{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: existingDSName}, ds); err != nil {
		return upgradeSnapshot{}, err
	}

	generated := &leaderworkersetv1.LeaderWorkerSetList{}
	if err := k8sClient.List(ctx, generated,
		client.InNamespace(testNamespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: existingDSName},
	); err != nil {
		return upgradeSnapshot{}, err
	}
	if len(generated.Items) != 2 {
		return upgradeSnapshot{}, fmt.Errorf("expected 2 generated LeaderWorkerSets, got %d", len(generated.Items))
	}

	generatedSnapshots := make([]generatedLWSSnapshot, 0, len(generated.Items))
	for _, generatedLWS := range generated.Items {
		role := generatedLWS.Labels[disaggregatedsetv1.RoleLabelKey]
		revision := generatedLWS.Labels[disaggregatedsetv1.RevisionLabelKey]
		if role == "" || revision == "" {
			return upgradeSnapshot{}, fmt.Errorf("generated LeaderWorkerSet %q is missing role or revision labels", generatedLWS.Name)
		}
		generatedSnapshots = append(generatedSnapshots, generatedLWSSnapshot{
			Name:     generatedLWS.Name,
			UID:      generatedLWS.UID,
			Role:     role,
			Revision: revision,
		})
	}
	sort.Slice(generatedSnapshots, func(i, j int) bool {
		return generatedSnapshots[i].Name < generatedSnapshots[j].Name
	})

	pods, err := workloadPodSnapshots()
	if err != nil {
		return upgradeSnapshot{}, err
	}

	return upgradeSnapshot{
		LeaderWorkerSet:  objectSnapshot{UID: lws.UID, Generation: lws.Generation},
		DisaggregatedSet: objectSnapshot{UID: ds.UID, Generation: ds.Generation},
		GeneratedLWS:     generatedSnapshots,
		WorkloadPods:     pods,
	}, nil
}

func workloadPodSnapshots() ([]podSnapshot, error) {
	standalonePods := &corev1.PodList{}
	if err := k8sClient.List(ctx, standalonePods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{leaderworkersetv1.SetNameLabelKey: existingLWSName},
	); err != nil {
		return nil, err
	}
	if len(standalonePods.Items) != 4 {
		return nil, fmt.Errorf("expected 4 standalone LeaderWorkerSet pods, got %d", len(standalonePods.Items))
	}

	dsPods := &corev1.PodList{}
	if err := k8sClient.List(ctx, dsPods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{disaggregatedsetv1.SetNameLabelKey: existingDSName},
	); err != nil {
		return nil, err
	}
	if len(dsPods.Items) != 2 {
		return nil, fmt.Errorf("expected 2 DisaggregatedSet pods, got %d", len(dsPods.Items))
	}

	allPods := append(standalonePods.Items, dsPods.Items...)
	snapshots := make([]podSnapshot, 0, len(allPods))
	for i := range allPods {
		pod := &allPods[i]
		snapshots = append(snapshots, podSnapshot{
			Name:                  pod.Name,
			UID:                   pod.UID,
			ContainerRestarts:     restartCounts(pod.Status.ContainerStatuses),
			InitContainerRestarts: restartCounts(pod.Status.InitContainerStatuses),
		})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Name < snapshots[j].Name
	})
	return snapshots, nil
}

func restartCounts(statuses []corev1.ContainerStatus) map[string]int32 {
	counts := make(map[string]int32, len(statuses))
	for _, status := range statuses {
		counts[status.Name] = status.RestartCount
	}
	return counts
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func writeSnapshot(snapshot upgradeSnapshot, err error) {
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	data, err := json.MarshalIndent(snapshot, "", "  ")
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(os.WriteFile(snapshotPath, data, 0o600)).To(gomega.Succeed())
}

func readSnapshot() upgradeSnapshot {
	data, err := os.ReadFile(snapshotPath)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	var snapshot upgradeSnapshot
	gomega.Expect(json.Unmarshal(data, &snapshot)).To(gomega.Succeed())
	return snapshot
}

func lwsNamespace() string {
	if ns := os.Getenv("LWS_NAMESPACE"); ns != "" {
		return ns
	}
	return "lws-system"
}

func expectCurrentControllerReady() {
	gomega.Eventually(func() error {
		deployment := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Namespace: lwsNamespace(),
			Name:      "lws-controller-manager",
		}, deployment); err != nil {
			return err
		}
		if deployment.Status.ObservedGeneration != deployment.Generation {
			return fmt.Errorf("controller deployment has not observed generation %d", deployment.Generation)
		}
		if deployment.Spec.Replicas == nil ||
			deployment.Status.UpdatedReplicas != *deployment.Spec.Replicas ||
			deployment.Status.ReadyReplicas != *deployment.Spec.Replicas ||
			deployment.Status.AvailableReplicas != *deployment.Spec.Replicas {
			return fmt.Errorf("controller deployment is not fully available")
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name == "manager" {
				if container.Image != currentImageTag {
					return fmt.Errorf("expected controller image %q, got %q", currentImageTag, container.Image)
				}
				return nil
			}
		}
		return fmt.Errorf("manager container not found")
	}, 5*time.Minute, 2*time.Second).Should(gomega.Succeed())
}
