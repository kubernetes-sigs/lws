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

// The runner generates LeaderWorkerSets in a cluster that runs the LWS
// controller, times how long creating and scaling them takes, and writes the
// result as summary.yaml. See test/performance/README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

const (
	phaseCreate = "create"
	// pollInterval is the resolution of the latencies. The runner polls an
	// informer cache, which adds no load to the API server.
	pollInterval = 100 * time.Millisecond
	// readyTimeout bounds the wait for the LWS webhooks.
	readyTimeout = 2 * time.Minute
)

var (
	configPath    = flag.String("config", "", "path of the generator config")
	rangeSpecPath = flag.String("rangespec", "", "path of the range spec to check the summary against; empty skips the check")
	outputDir     = flag.String("output-dir", ".", "directory summary.yaml is written to")
	qps           = flag.Float64("qps", 100, "client QPS of the runner, high enough that the runner is not the bottleneck")
)

func main() {
	flag.Parse()
	if err := run(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	var rangeSpec *RangeSpec
	if *rangeSpecPath != "" {
		if rangeSpec, err = loadRangeSpec(*rangeSpecPath); err != nil {
			return err
		}
	}
	r, err := newRunner(ctx, cfg)
	if err != nil {
		return err
	}
	summary, runErr := r.run(ctx)
	// Write what was measured even when a later phase failed.
	if len(summary.Phases) > 0 {
		path := filepath.Join(*outputDir, "summary.yaml")
		if err := writeSummary(path, summary); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	if runErr != nil || rangeSpec == nil {
		return runErr
	}
	if failures := rangeSpec.check(summary); len(failures) > 0 {
		return fmt.Errorf("limits of %s exceeded:\n%s", *rangeSpecPath, strings.Join(failures, "\n"))
	}
	fmt.Println("all limits of", *rangeSpecPath, "hold")
	return nil
}

type runner struct {
	cfg *Config
	// client reads from and writes to the API server, and cache reads from
	// informers on the workload namespace.
	client client.Client
	cache  client.Reader
	// replicas is the current target of each LeaderWorkerSet, by name.
	replicas map[string]int32
}

func newRunner(ctx context.Context, cfg *Config) (*runner, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	restConfig.QPS, restConfig.Burst = float32(*qps), int(*qps)*2
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(leaderworkerset.AddToScheme(scheme))
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	informers, err := cache.New(restConfig, cache.Options{Scheme: scheme, DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}})
	if err != nil {
		return nil, err
	}
	// Sync the informers now, so that their first list does not count as latency.
	for _, obj := range []client.Object{&leaderworkerset.LeaderWorkerSet{}, &corev1.Pod{}} {
		if _, err := informers.GetInformer(ctx, obj); err != nil {
			return nil, err
		}
	}
	go func() {
		if err := informers.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "informers:", err)
		}
	}()
	if !informers.WaitForCacheSync(ctx) {
		return nil, errors.New("the informers did not sync")
	}
	return &runner{cfg: cfg, client: c, cache: informers, replicas: map[string]int32{}}, nil
}

func (r *runner) run(ctx context.Context) (*Summary, error) {
	summary := &Summary{}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.cfg.Namespace}}
	if err := r.client.Create(ctx, namespace); err != nil {
		return summary, fmt.Errorf("creating namespace: %w", err)
	}
	defer func() {
		// The run context may be cancelled already.
		if err := r.client.Delete(context.Background(), namespace); err != nil {
			fmt.Fprintln(os.Stderr, "deleting namespace:", err)
		}
	}()
	if err := r.waitForController(ctx); err != nil {
		return summary, err
	}
	// Creation is a phase that scales every class from nothing.
	create := ScalePhase{Name: phaseCreate, Replicas: map[string]int32{}}
	for _, class := range r.cfg.Classes {
		create.Replicas[class.Name] = class.Replicas
	}
	for _, phase := range append([]ScalePhase{create}, r.cfg.Scale...) {
		result, err := r.phase(ctx, phase)
		if err != nil {
			return summary, err
		}
		summary.Phases = append(summary.Phases, result)
		fmt.Printf("phase %s: %d groups in %dms\n", phase.Name, result.Groups, result.WallTimeMs)
		if err := r.checkPods(ctx, phase.Name); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

// waitForController waits until the LWS webhooks admit a LeaderWorkerSet. They
// can lag behind the rollout of the controller, and that wait must not count
// as latency.
func (r *runner) waitForController(ctx context.Context) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, time.Second, readyTimeout, true, func(ctx context.Context) (bool, error) {
		lastErr = r.client.Create(ctx, r.cfg.buildLeaderWorkerSet(r.cfg.Classes[0], 0), client.DryRunAll)
		return lastErr == nil, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the LWS controller: %w: %v", err, lastErr)
	}
	return nil
}

// phase sets the replicas of every LeaderWorkerSet of the classes it lists,
// creating the LeaderWorkerSets that do not exist yet, and waits until each
// has reached its target.
func (r *runner) phase(ctx context.Context, phase ScalePhase) (PhaseSummary, error) {
	started := map[string]time.Time{}
	var groups, pods int64
	begin := time.Now()
	for _, class := range r.cfg.Classes {
		target, found := phase.Replicas[class.Name]
		if !found {
			continue
		}
		for i := range class.Count {
			lws := r.cfg.buildLeaderWorkerSet(class, i)
			current, exists := r.replicas[lws.Name]
			if exists && current == target {
				continue
			}
			started[lws.Name] = time.Now()
			var err error
			if exists {
				patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, target)
				err = r.client.Patch(ctx, lws, client.RawPatch(types.MergePatchType, patch))
			} else {
				err = r.client.Create(ctx, lws)
			}
			if err != nil {
				return PhaseSummary{}, fmt.Errorf("phase %s: %w", phase.Name, err)
			}
			r.replicas[lws.Name] = target
			delta := int64(max(target-current, current-target))
			groups += delta
			pods += delta * int64(class.Size)
		}
	}

	var latencies []time.Duration
	err := wait.PollUntilContextTimeout(ctx, pollInterval, r.cfg.Timeout.Duration, true, func(ctx context.Context) (bool, error) {
		list := &leaderworkerset.LeaderWorkerSetList{}
		if err := r.cache.List(ctx, list, client.InNamespace(r.cfg.Namespace)); err != nil {
			return false, err
		}
		for i := range list.Items {
			lws := &list.Items[i]
			if start, waiting := started[lws.Name]; waiting && reachedTarget(lws, r.replicas[lws.Name]) {
				latencies = append(latencies, time.Since(start))
				delete(started, lws.Name)
			}
		}
		return len(started) == 0, nil
	})
	if err != nil {
		return PhaseSummary{}, fmt.Errorf("phase %s: %w (%d LeaderWorkerSets did not reach their target)", phase.Name, err, len(started))
	}
	return summarizePhase(phase.Name, latencies, time.Since(begin), groups, pods), nil
}

// reachedTarget reports whether the controller has observed the current spec
// and the LeaderWorkerSet has exactly target replicas, all of them ready.
func reachedTarget(lws *leaderworkerset.LeaderWorkerSet, target int32) bool {
	if lws.Spec.Replicas == nil || *lws.Spec.Replicas != target || lws.Status.ObservedGeneration != lws.Generation ||
		lws.Status.Replicas != target || lws.Status.ReadyReplicas != target {
		return false
	}
	available := apimeta.FindStatusCondition(lws.Status.Conditions, string(leaderworkerset.LeaderWorkerSetAvailable))
	return target == 0 || (available != nil && available.Status == metav1.ConditionTrue && available.ObservedGeneration == lws.Generation)
}

// checkPods waits until each LeaderWorkerSet has exactly as many pods as its
// target asks for, all running and ready, and there are no other pods.
func (r *runner) checkPods(ctx context.Context, phase string) error {
	want := map[string]int{}
	for _, class := range r.cfg.Classes {
		for i := range class.Count {
			name := leaderWorkerSetName(class, i)
			want[name] = int(r.replicas[name] * class.Size)
		}
	}
	var mismatch string
	err := wait.PollUntilContextTimeout(ctx, time.Second, r.cfg.Timeout.Duration, true, func(ctx context.Context) (bool, error) {
		pods := &corev1.PodList{}
		if err := r.cache.List(ctx, pods, client.InNamespace(r.cfg.Namespace)); err != nil {
			return false, err
		}
		mismatch = podMismatch(pods.Items, want)
		return mismatch == "", nil
	})
	if err != nil {
		return fmt.Errorf("checking the pods after phase %s: %w: %s", phase, err, mismatch)
	}
	return nil
}

// podMismatch returns how the pods differ from want, the number of pods of
// each LeaderWorkerSet by name, or "" when they match.
func podMismatch(pods []corev1.Pod, want map[string]int) string {
	got := map[string]int{}
	for _, pod := range pods {
		name := pod.Labels[leaderworkerset.SetNameLabelKey]
		if _, found := want[name]; !found {
			return fmt.Sprintf("pod %s is not expected", pod.Name)
		}
		if podutils.PodDeleted(pod) || !podutils.PodRunningAndReady(pod) {
			return fmt.Sprintf("pod %s is not running and ready", pod.Name)
		}
		got[name]++
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got[name] != want[name] {
			return fmt.Sprintf("LeaderWorkerSet %s has %d pods, want %d", name, got[name], want[name])
		}
	}
	return ""
}
