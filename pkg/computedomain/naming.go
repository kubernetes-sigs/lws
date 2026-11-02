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

package computedomain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	uidHashLength      = 5
	overflowHashLength = 10
	incarnationLength  = 5
)

// UIDPrefix returns "<name>-<uid5>" so ComputeDomain names do not collide across
// delete-and-recreate cycles of a same-named workload while older pods drain.
func UIDPrefix(name string, uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	uidHash := hex.EncodeToString(sum[:])[:uidHashLength]
	return name + "-" + uidHash
}

// BoundedName bounds raw to the 63-character DNS-1035 label limit, truncating
// and appending a deterministic 10-character SHA-256 hex digest on overflow.
func BoundedName(raw string) string {
	if len(raw) <= validation.DNS1035LabelMaxLength {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	digest := hex.EncodeToString(sum[:])[:overflowHashLength]
	maxPrefixLen := validation.DNS1035LabelMaxLength - overflowHashLength - 1
	prefix := strings.TrimRight(raw[:maxPrefixLen], "-")
	return prefix + "-" + digest
}

// ReplicaComputeDomainName returns the ComputeDomain name for one leader
// incarnation of a LeaderWorkerSet replica. incarnation is a random token drawn
// at leader admission, so every leader Pod gets its own ComputeDomain.
func ReplicaComputeDomainName(prefix, groupIndex, revisionKey, incarnation string) string {
	return BoundedName(fmt.Sprintf("%s-%s-%s-%s", prefix, revisionKey, groupIndex, incarnation))
}

// SliceComputeDomainName returns the deterministic ComputeDomain name for a
// DisaggregatedSet (slice, revision).
func SliceComputeDomainName(ds *disaggregatedsetv1.DisaggregatedSet, slice int, revision string) string {
	return BoundedName(fmt.Sprintf("%s-s%d-%s", UIDPrefix(ds.Name, ds.UID), slice, revision))
}

// LeaderComputeDomainName returns the ComputeDomain name PodWebhook stamped on
// leaderPod at admission, or "" when the leader was admitted without one.
func LeaderComputeDomainName(leaderPod *corev1.Pod) string {
	return leaderPod.Annotations[leaderworkersetv1.ComputeDomainNameAnnotationKey]
}
