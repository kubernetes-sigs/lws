/*
Copyright 2024.

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

package utils

import (
	"crypto/sha1"
	"encoding/hex"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

// Sha1Hash accepts an input string and returns the 40 character SHA1 hash digest of the input string.
func Sha1Hash(s string) string {
	h := sha1.New()
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

func NonZeroValue(value int32) int32 {
	if value < 0 {
		return 0
	}
	return value
}

func LeaderWorkerTemplateHash(lws *leaderworkerset.LeaderWorkerSet) string {
	if lws.Spec.NetworkConfig == nil {
		return Sha1Hash(lws.Spec.LeaderWorkerTemplate.LeaderTemplate.String() +
			lws.Spec.LeaderWorkerTemplate.WorkerTemplate.String())
	}

	return Sha1Hash(lws.Spec.LeaderWorkerTemplate.LeaderTemplate.String() +
		lws.Spec.LeaderWorkerTemplate.WorkerTemplate.String() + string(*lws.Spec.NetworkConfig.SubdomainPolicy))
}

// SortByIndex returns an ascending list, the length of the list is always specified by the parameter.
func SortByIndex[T appsv1.StatefulSet | corev1.Pod | int](indexFunc func(T) (int, error), items []T, length int) []T {
	result := make([]T, length)

	for _, item := range items {
		index, err := indexFunc(item)
		if err != nil {
			// When no index found, continue, this can happen when
			// statefulset doesn't have the index.
			continue
		}

		if index >= length {
			continue
		}
		result[index] = item
	}

	return result
}
