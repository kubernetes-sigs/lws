/*
Copyright 2023.

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

package metrics

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	podutils "sigs.k8s.io/lws/pkg/utils/pod"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	rollingUpdateDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: "lws",
			Name:      "rolling_update_duration",
			Help:      "Duration of rolling updates",
		}, []string{"hash"},
	)

	recreateGroupTimes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: "lws",
			Name:      "recreate_group_times",
			Help:      "number of times a group has been recreated",
		}, []string{"leadername"},
	)

	replicaReadyStatusDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: "lws",
			Name:      "replica_ready_status_duration",
			Help:      "latency for each replica to be scheduled and become ready",
		}, []string{"leadername"},
	)
)

func RollingUpdate(hash string, duration time.Duration) {
	rollingUpdateDuration.WithLabelValues(hash).Observe(duration.Seconds())
}

func RecreatingGroup(leaderName string) {
	recreateGroupTimes.WithLabelValues(leaderName).Inc()
}

func ReplicaReadyStatus(readyPods []corev1.Pod, startTime metav1.Time) {
	for _, pod := range readyPods {
		readyTime := podutils.PodReadyConditionLastTransitionTime(pod).Time
		latency := readyTime.Sub(startTime.Time)
		replicaReadyStatusDuration.WithLabelValues(pod.Name).Observe(latency.Seconds())
	}
}

func Register() {
	metrics.Registry.MustRegister(
		rollingUpdateDuration,
		recreateGroupTimes,
		replicaReadyStatusDuration,
	)
}
