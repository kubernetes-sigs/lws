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
	"bufio"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	podutils "sigs.k8s.io/lws/pkg/utils/pod"
)

// sample sums the metrics of the LWS controller pods at one time.
type sample struct {
	cpuSeconds, rssBytes float64
	// requests counts the requests sent to the API server, by HTTP method.
	requests map[string]float64
	leader   bool
}

// controllerMetrics reads the metrics of the LWS controller pods through
// kubectl port-forward, with a token of the controller service account, which
// config/rbac allows to read /metrics, as the e2e metrics test does. The pods
// are summed: the leader reconciles, and every pod serves webhooks.
type controllerMetrics struct {
	http  *http.Client
	token string
	urls  []string
	// cancel ends the port-forwards and the memory sampling, and forwards
	// tracks the kubectl processes until they have exited.
	cancel   context.CancelFunc
	forwards sync.WaitGroup

	mu      sync.Mutex
	peakRSS float64
}

// startControllerMetrics port-forwards to each ready controller pod in
// namespace, and samples their memory every second until stop is called.
func startControllerMetrics(ctx context.Context, c client.Client, kubectl, namespace string) (*controllerMetrics, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"control-plane": "controller-manager"}); err != nil {
		return nil, fmt.Errorf("listing the LWS controller pods: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &controllerMetrics{
		// The metrics server has a self-signed certificate.
		http:   &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
		cancel: cancel,
	}
	for _, pod := range pods.Items {
		if podutils.PodDeleted(pod) || !podutils.PodRunningAndReady(pod) {
			continue
		}
		if m.token == "" {
			sa := &corev1.ServiceAccount{}
			sa.Name, sa.Namespace = pod.Spec.ServiceAccountName, namespace
			request := &authenticationv1.TokenRequest{}
			if err := c.SubResource("token").Create(ctx, sa, request); err != nil {
				m.stop()
				return nil, fmt.Errorf("requesting a token for %s: %w", sa.Name, err)
			}
			m.token = request.Status.Token
		}
		url, err := portForward(ctx, &m.forwards, kubectl, namespace, pod.Name)
		if err != nil {
			m.stop()
			return nil, err
		}
		m.urls = append(m.urls, url)
	}
	if len(m.urls) == 0 {
		m.stop()
		return nil, fmt.Errorf("no ready LWS controller pod in namespace %s", namespace)
	}
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		if s, err := m.scrape(ctx); err == nil {
			m.observe(s.rssBytes)
		}
	}, time.Second)
	return m, nil
}

// stop ends the port-forwards and the memory sampling, and waits until the
// kubectl processes have exited, so that none outlives the runner.
func (m *controllerMetrics) stop() {
	m.cancel()
	m.forwards.Wait()
}

// portForward starts kubectl port-forward to the metrics port of a pod and
// returns the URL of its metrics endpoint. The process runs until ctx is done,
// and forwards is done when it has exited.
func portForward(ctx context.Context, forwards *sync.WaitGroup, kubectl, namespace, pod string) (string, error) {
	args := []string{"port-forward", "-n", namespace, "pod/" + pod, ":8443"}
	// The runner takes the kubeconfig flag from controller-runtime.
	if kubeconfig := flag.Lookup("kubeconfig"); kubeconfig != nil && kubeconfig.Value.String() != "" {
		args = append(args, "--kubeconfig", kubeconfig.Value.String())
	}
	cmd := exec.CommandContext(ctx, kubectl, args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("port-forwarding to %s: %w", pod, err)
	}
	// kubectl prints "Forwarding from 127.0.0.1:<port> -> 8443" once it
	// listens, and then a line for each connection.
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		var port int
		if _, err := fmt.Sscanf(lines.Text(), "Forwarding from 127.0.0.1:%d ->", &port); err == nil {
			forwards.Go(func() {
				for lines.Scan() {
				}
				_ = cmd.Wait()
			})
			return fmt.Sprintf("https://127.0.0.1:%d/metrics", port), nil
		}
	}
	_ = cmd.Wait()
	return "", fmt.Errorf("port-forwarding to %s: kubectl exited before listening", pod)
}

func (m *controllerMetrics) scrape(ctx context.Context) (sample, error) {
	s := sample{requests: map[string]float64{}}
	for _, url := range m.urls {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return s, err
		}
		request.Header.Set("Authorization", "Bearer "+m.token)
		response, err := m.http.Do(request)
		if err != nil {
			return s, err
		}
		parser := expfmt.NewTextParser(model.UTF8Validation)
		families, err := parser.TextToMetricFamilies(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return s, fmt.Errorf("reading %s: %s", url, response.Status)
		}
		if err != nil {
			return s, fmt.Errorf("reading %s: %w", url, err)
		}
		s.add(families)
	}
	return s, nil
}

// add adds the metrics of one controller pod to the sample.
func (s *sample) add(families map[string]*dto.MetricFamily) {
	for _, metric := range families["process_cpu_seconds_total"].GetMetric() {
		s.cpuSeconds += metric.GetCounter().GetValue()
	}
	for _, metric := range families["process_resident_memory_bytes"].GetMetric() {
		s.rssBytes += metric.GetGauge().GetValue()
	}
	for _, metric := range families["rest_client_requests_total"].GetMetric() {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "method" {
				s.requests[label.GetValue()] += metric.GetCounter().GetValue()
			}
		}
	}
	for _, metric := range families["leader_election_master_status"].GetMetric() {
		s.leader = s.leader || metric.GetGauge().GetValue() == 1
	}
}

func (m *controllerMetrics) observe(rssBytes float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peakRSS = max(m.peakRSS, rssBytes)
}

// begin starts measuring a phase, and returns the sample to pass to end.
func (m *controllerMetrics) begin(ctx context.Context) (sample, error) {
	s, err := m.scrape(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peakRSS = s.rssBytes
	return s, err
}

// end returns what the controller used since begin.
func (m *controllerMetrics) end(ctx context.Context, before sample) (*ControllerUsage, error) {
	after, err := m.scrape(ctx)
	if err != nil {
		return nil, err
	}
	m.observe(after.rssBytes)
	m.mu.Lock()
	defer m.mu.Unlock()
	return controllerUsage(before, after, m.peakRSS), nil
}

func controllerUsage(before, after sample, peakRSS float64) *ControllerUsage {
	usage := &ControllerUsage{
		CPUSeconds:          round2(after.cpuSeconds - before.cpuSeconds),
		PeakMemoryMiB:       round2(peakRSS / (1 << 20)),
		APIRequestsByMethod: map[string]int64{},
	}
	for method, count := range after.requests {
		if delta := int64(count - before.requests[method]); delta > 0 {
			usage.APIRequestsByMethod[method] = delta
			usage.APIRequests += delta
		}
	}
	return usage
}
