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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

func TestConfigValidate(t *testing.T) {
	tests := map[string]func(*Config){
		"no namespace":                   func(c *Config) { c.Namespace = "" },
		"no timeout":                     func(c *Config) { c.Timeout = metav1.Duration{} },
		"duplicate class":                func(c *Config) { c.Classes = append(c.Classes, c.Classes[0]) },
		"no LeaderWorkerSets in a class": func(c *Config) { c.Classes[0].Count = 0 },
		"scale phase of unknown class":   func(c *Config) { c.Scale[0].Replicas = map[string]int32{"other": 1} },
		"scale phase named like create":  func(c *Config) { c.Scale[0].Name = phaseCreate },
	}
	valid := func() *Config {
		return &Config{
			Namespace: "ns",
			Timeout:   metav1.Duration{Duration: time.Minute},
			Classes:   []Class{{Name: "small", Count: 1, Replicas: 2, Size: 2}},
			Scale:     []ScalePhase{{Name: "scale-up", Replicas: map[string]int32{"small": 4}}},
		}
	}
	if err := valid().validate(); err != nil {
		t.Errorf("validate() of a valid config = %v", err)
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(cfg)
			if cfg.validate() == nil {
				t.Error("validate() = nil, want an error")
			}
		})
	}
}

// The shipped configs must load, and the range spec must only bound phases
// the baseline config produces.
func TestShippedConfigs(t *testing.T) {
	if _, err := loadConfig("../configs/smoke/generator.yaml"); err != nil {
		t.Error(err)
	}
	cfg, err := loadConfig("../configs/baseline/generator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := loadRangeSpec("../configs/baseline/rangespec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	phases := map[string]bool{phaseCreate: true}
	for _, phase := range cfg.Scale {
		phases[phase.Name] = true
	}
	for name := range spec.Phases {
		if !phases[name] {
			t.Errorf("range spec phase %s is not produced by the baseline config", name)
		}
	}
}

func TestBuildLeaderWorkerSet(t *testing.T) {
	cfg := &Config{
		Namespace:    "ns",
		NodeSelector: map[string]string{"type": "kwok"},
		Tolerations:  []corev1.Toleration{{Key: "kwok.x-k8s.io/node", Operator: corev1.TolerationOpExists}},
	}
	lws := cfg.buildLeaderWorkerSet(Class{Name: "large", Replicas: 3, Size: 8, GroupIdentity: leaderworkerset.GroupIdentityHash}, 2)
	got := fmt.Sprintf("%s/%s %v replicas=%d size=%d %s", lws.Namespace, lws.Name, lws.Labels, *lws.Spec.Replicas,
		*lws.Spec.LeaderWorkerTemplate.Size, lws.Spec.GroupIdentity)
	if want := "ns/large-2 map[performance.lws.x-k8s.io/class:large] replicas=3 size=8 Hash"; got != want {
		t.Errorf("buildLeaderWorkerSet() = %s, want %s", got, want)
	}
	for _, spec := range []corev1.PodSpec{lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec, lws.Spec.LeaderWorkerTemplate.WorkerTemplate.Spec} {
		if spec.NodeSelector["type"] != "kwok" || len(spec.Tolerations) != 1 {
			t.Errorf("pods land on %v with tolerations %v, want the simulated nodes", spec.NodeSelector, spec.Tolerations)
		}
	}
	if got := cfg.buildLeaderWorkerSet(Class{Name: "small", Replicas: 1, Size: 1}, 0).Spec.GroupIdentity; got != leaderworkerset.GroupIdentityOrdinal {
		t.Errorf("group identity of a class without one = %s, want Ordinal", got)
	}
}

func TestReachedTarget(t *testing.T) {
	type state struct{ spec, status, ready, observed, available int32 }
	tests := map[string]struct {
		state  state
		target int32
		want   bool
	}{
		"all replicas ready and available":     {state: state{2, 2, 2, 3, 3}, target: 2, want: true},
		"spec not updated yet":                 {state: state{2, 2, 2, 3, 3}, target: 4},
		"spec not observed yet":                {state: state{2, 2, 2, 2, 3}, target: 2},
		"replicas not ready yet":               {state: state{4, 4, 2, 3, 3}, target: 4},
		"surplus replicas not removed yet":     {state: state{1, 2, 1, 3, 3}, target: 1},
		"condition from an earlier generation": {state: state{2, 2, 2, 3, 2}, target: 2},
		"not available":                        {state: state{2, 2, 2, 3, 0}, target: 2},
		"scaled to zero needs no condition":    {state: state{0, 0, 0, 3, 0}, target: 0, want: true},
		"scaled to zero with replicas left":    {state: state{0, 1, 0, 3, 0}, target: 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lws := &leaderworkerset.LeaderWorkerSet{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       leaderworkerset.LeaderWorkerSetSpec{Replicas: ptr.To(tc.state.spec)},
				Status: leaderworkerset.LeaderWorkerSetStatus{
					Replicas: tc.state.status, ReadyReplicas: tc.state.ready, ObservedGeneration: int64(tc.state.observed),
				},
			}
			if tc.state.available > 0 {
				lws.Status.Conditions = []metav1.Condition{{Type: string(leaderworkerset.LeaderWorkerSetAvailable),
					Status: metav1.ConditionTrue, ObservedGeneration: int64(tc.state.available)}}
			}
			if got := reachedTarget(lws, tc.target); got != tc.want {
				t.Errorf("reachedTarget() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestPodMismatch(t *testing.T) {
	pod := func(name, set string, ready, terminating bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{leaderworkerset.SetNameLabelKey: set}}}
		if ready {
			p.Status.Phase = corev1.PodRunning
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		if terminating {
			p.DeletionTimestamp = &metav1.Time{}
		}
		return p
	}
	leader, worker := pod("a-0", "a", true, false), pod("a-0-1", "a", true, false)
	tests := map[string]struct {
		pods []corev1.Pod
		want string
	}{
		"match":           {pods: []corev1.Pod{leader, worker}},
		"missing pod":     {pods: []corev1.Pod{leader}, want: "LeaderWorkerSet a has 1 pods, want 2"},
		"pod not ready":   {pods: []corev1.Pod{leader, pod("a-0-1", "a", false, false)}, want: "pod a-0-1 is not running and ready"},
		"terminating pod": {pods: []corev1.Pod{leader, pod("a-0-1", "a", true, true)}, want: "pod a-0-1 is not running and ready"},
		"surplus pod":     {pods: []corev1.Pod{leader, worker, pod("b-0", "b", true, false)}, want: "LeaderWorkerSet b has 1 pods, want 0"},
		"unrelated pod":   {pods: []corev1.Pod{pod("other", "", true, false)}, want: "pod other is not expected"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := podMismatch(tc.pods, map[string]int{"a": 2, "b": 0}); got != tc.want {
				t.Errorf("podMismatch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSummarizePhase(t *testing.T) {
	var latencies []time.Duration
	// Unsorted on purpose: 10s, 9s, ..., 1s.
	for i := 10; i >= 1; i-- {
		latencies = append(latencies, time.Duration(i)*time.Second)
	}
	want := PhaseSummary{
		Name: "create", LeaderWorkerSets: 10, Groups: 40, Pods: 160, WallTimeMs: 30000, GroupsPerSecond: 1.33,
		P50Ms: 5000, P90Ms: 9000, P99Ms: 10000, MaxMs: 10000,
	}
	if diff := cmp.Diff(want, summarizePhase("create", latencies, 30*time.Second, 40, 160)); diff != "" {
		t.Errorf("summarizePhase() (-want +got):\n%s", diff)
	}
	if got := summarizePhase("empty", nil, 0, 0, 0); got.MaxMs != 0 || got.GroupsPerSecond != 0 {
		t.Errorf("summarizePhase() of no latencies = %+v, want zero values", got)
	}
}

func TestWriteSummary(t *testing.T) {
	summary := &Summary{Phases: []PhaseSummary{{Name: "create", LeaderWorkerSets: 2, WallTimeMs: 1000, GroupsPerSecond: 1.5, P90Ms: 800,
		Controller: &ControllerUsage{CPUSeconds: 1.5, APIRequests: 3, APIRequestsByMethod: map[string]int64{"POST": 3}}}}}
	path := filepath.Join(t.TempDir(), "results", "summary.yaml")
	if err := writeSummary(path, summary); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := &Summary{}
	if err := yaml.UnmarshalStrict(raw, got); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(summary, got); diff != "" {
		t.Errorf("summary read back (-want +got):\n%s", diff)
	}
}

func TestRangeSpecCheck(t *testing.T) {
	summary := &Summary{Phases: []PhaseSummary{
		{Name: "create", WallTimeMs: 1000, P50Ms: 500, P90Ms: 800, Controller: &ControllerUsage{CPUSeconds: 2.5, APIRequests: 500}},
		{Name: "scale-up", WallTimeMs: 500, MaxMs: 450},
	}}
	tests := map[string]struct {
		spec map[string]map[string]float64
		want []string
	}{
		"within the limits": {
			spec: map[string]map[string]float64{"create": {"wallTimeMs": 1000, "p90Ms": 800, "controller.cpuSeconds": 2.5}},
		},
		"each exceeded bound is reported": {
			spec: map[string]map[string]float64{
				"create":   {"wallTimeMs": 999, "p50Ms": 499.5, "p90Ms": 799, "controller.cpuSeconds": 2.4, "controller.apiRequests": 499},
				"scale-up": {"maxMs": 449},
			},
			want: []string{
				"phase create: controller.apiRequests is 500, above the limit of 499",
				"phase create: controller.cpuSeconds is 2.5, above the limit of 2.4",
				"phase create: p50Ms is 500, above the limit of 499.5",
				"phase create: p90Ms is 800, above the limit of 799",
				"phase create: wallTimeMs is 1000, above the limit of 999",
				"phase scale-up: maxMs is 450, above the limit of 449",
			},
		},
		"bounds without a measurement": {
			spec: map[string]map[string]float64{"scale-up": {"controller.cpuSeconds": 1, "wallTime": 1}, "scale-down": {"maxMs": 1}},
			want: []string{
				"phase scale-down: not in the summary",
				"phase scale-up: controller.cpuSeconds is not measured",
				"phase scale-up: wallTime is not measured",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec := &RangeSpec{Phases: tc.spec}
			if diff := cmp.Diff(tc.want, spec.check(summary)); diff != "" {
				t.Errorf("check() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestControllerUsage(t *testing.T) {
	leader := func(cpuSeconds float64, posts, patches int) string {
		return fmt.Sprintf(`# TYPE process_cpu_seconds_total counter
process_cpu_seconds_total %g
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 1.048576e+08
# TYPE rest_client_requests_total counter
rest_client_requests_total{code="200",method="GET"} 100
rest_client_requests_total{code="201",method="POST"} %d
rest_client_requests_total{code="200",method="PATCH"} %d
# TYPE leader_election_master_status gauge
leader_election_master_status{name="lws"} 1
`, cpuSeconds, posts, patches)
	}
	const follower = `# TYPE process_cpu_seconds_total counter
process_cpu_seconds_total 1
# TYPE process_resident_memory_bytes gauge
process_resident_memory_bytes 5.24288e+07
# TYPE leader_election_master_status gauge
leader_election_master_status{name="lws"} 0
`
	scrape := func(pods ...string) sample {
		s := sample{requests: map[string]float64{}}
		for _, text := range pods {
			parser := expfmt.NewTextParser(model.UTF8Validation)
			families, err := parser.TextToMetricFamilies(strings.NewReader(text))
			if err != nil {
				t.Fatal(err)
			}
			s.add(families)
		}
		return s
	}
	if scrape(follower).leader {
		t.Error("a follower alone is the leader")
	}
	before, after := scrape(leader(10, 20, 0), follower), scrape(leader(12.5, 70, 30), follower)
	if !before.leader {
		t.Error("no leader among the leader and a follower")
	}
	want := &ControllerUsage{CPUSeconds: 2.5, PeakMemoryMiB: 150, APIRequests: 80, APIRequestsByMethod: map[string]int64{"POST": 50, "PATCH": 30}}
	if diff := cmp.Diff(want, controllerUsage(before, after, after.rssBytes)); diff != "" {
		t.Errorf("controllerUsage() (-want +got):\n%s", diff)
	}
}
