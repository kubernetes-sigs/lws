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
	"testing"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
)

const testFeature featuregate.Feature = "TestFeature"

// TestSetEnable verifies that the SetEnable helper toggles a registered
// feature gate end-to-end via the default mutable feature gate.
func TestSetEnable(t *testing.T) {
	if err := utilfeature.DefaultMutableFeatureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		testFeature: {Default: false, PreRelease: featuregate.Alpha},
	}); err != nil {
		t.Fatalf("failed to register test feature: %v", err)
	}

	// Ensure gate is restored after the test regardless of outcome.
	SetFeatureGateDuringTest(t, testFeature, false)

	if Enabled(testFeature) {
		t.Fatalf("expected %s to be disabled by default", testFeature)
	}

	if err := SetEnable(testFeature, true); err != nil {
		t.Fatalf("SetEnable returned error: %v", err)
	}

	if !Enabled(testFeature) {
		t.Fatalf("expected %s to be enabled after SetEnable(true)", testFeature)
	}
}

// TestSetFeatureGateDuringTest verifies the test helper restores the original
// feature gate value after the test finishes.
func TestSetFeatureGateDuringTest(t *testing.T) {
	const f featuregate.Feature = "TempTestFeature"

	if err := utilfeature.DefaultMutableFeatureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		f: {Default: false, PreRelease: featuregate.Alpha},
	}); err != nil {
		t.Fatalf("failed to register %s: %v", f, err)
	}

	t.Run("inner", func(t *testing.T) {
		SetFeatureGateDuringTest(t, f, true)
		if !Enabled(f) {
			t.Fatalf("expected %s to be enabled inside helper scope", f)
		}
	})

	if Enabled(f) {
		t.Fatalf("expected %s to be restored to disabled after helper scope", f)
	}
}
