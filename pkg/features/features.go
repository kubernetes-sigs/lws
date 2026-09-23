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

package features

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

const (
	// owner: @pacoxu
	// kep: https://github.com/kubernetes-sigs/lws/blob/main/keps/666-gang-scheduling-in-lws/README.md
	//
	// Enables spec.scheduling. Existing scheduled objects continue reconciling
	// when it is disabled.
	WorkloadAwareScheduling featuregate.Feature = "WorkloadAwareScheduling"
)

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.Add(defaultFeatureGates))
}

// defaultFeatureGates consists of all known LWS-specific feature keys.
// To add a new feature, define a key for it above and add it here.
//
// Entries are separated from each other with blank lines to avoid sweeping gofmt
// changes when adding or removing one entry.
var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	WorkloadAwareScheduling: {Default: false, PreRelease: featuregate.Alpha},
}

func SetFeatureGateDuringTest(tb testing.TB, f featuregate.Feature, value bool) {
	featuregatetesting.SetFeatureGateDuringTest(tb, utilfeature.DefaultFeatureGate, f, value)
}

// Enabled is helper for `utilfeature.DefaultFeatureGate.Enabled()`
func Enabled(f featuregate.Feature) bool {
	return utilfeature.DefaultFeatureGate.Enabled(f)
}

// Known returns the set of supported LWS feature-gate names.
func Known() []string {
	names := make([]string, 0, len(defaultFeatureGates))
	for name := range defaultFeatureGates {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}
