/*
Copyright 2025.

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

package podgroup

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// Provider defines the interface for managing pod group resources
type Provider interface {
	// CreatePodGroupIfNotExists creates a PodGroup if it doesn't exist, called by pod controller
	CreatePodGroupIfNotExists(ctx context.Context, lws *leaderworkerset.LeaderWorkerSet, leaderPod *corev1.Pod) error

	// SetPodMeta sets pod meta for PodGroup association, called by webhook
	SetPodMeta(pod *corev1.Pod) error
}
