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

package groupdeletion

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestProtectionOwner(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		uid, owner   types.UID
	}{
		{name: "exact identity", marker: "parent/pod", uid: "pod", owner: "parent"},
		{name: "absent protection", uid: "pod"},
		{name: "missing parent", marker: "/pod", uid: "pod"},
		{name: "missing separator", marker: "parent", uid: "pod"},
		{name: "extra separator", marker: "parent/extra/pod", uid: "pod"},
		{name: "copied protection", marker: "parent/old-pod", uid: "pod"},
		{name: "missing Pod UID", marker: "parent/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: tc.uid, Annotations: map[string]string{ProtectionAnnotation: tc.marker}}}
			require.Equal(t, tc.owner, ProtectionOwner(pod))
		})
	}
}
