/*
Copyright 2026.

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

package controllers

import "testing"

// The original helper is now production code in combined_rollout.go. These
// additional regressions exercise the bounded production reservation window.
func TestCombinedPlannerBoundedReservations(t *testing.T) {
	in := combinedTestInput(100, 101, 100)
	p := combinedTestPlan(t, in)
	s, err := combinedModelDecode(p.state)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Reservations) != combinedReservationLimit || !p.blocked || len(p.state) > combinedStateLimit {
		t.Fatalf("unbounded reservation window: %d, %+v", len(s.Reservations), p)
	}
	in.state, in.partition = p.state, p.partition
	again := combinedTestPlan(t, in)
	if p.state != again.state || p.partition != again.partition {
		t.Fatal("full window spent more credit on restart")
	}
}

func TestCombinedPlannerHPAKeepsBaseline(t *testing.T) {
	in := combinedTestInput(4, 8, 1)
	p := combinedTestPlan(t, in)
	in.state, in.partition = p.state, p.partition
	in.baseline, in.desired, in.generation = 8, 12, 3
	p = combinedTestPlan(t, in)
	s, err := combinedModelDecode(p.state)
	if err != nil {
		t.Fatal(err)
	}
	if s.Baseline != 4 || p.floor != 3 || len(p.deletes) != 1 {
		t.Fatalf("HPA recaptured baseline: %+v", p)
	}
}
