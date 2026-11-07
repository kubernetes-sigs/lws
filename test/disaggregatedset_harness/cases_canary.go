// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"fmt"
	"strings"
)

func canaryTargets(l *Lab, p, d int) Targets {
	if l.virtual {
		return Targets{"model/prefill": p, "model/decode": d}
	}
	return pair(p, d)
}

func startCanary(l *Lab) Targets {
	l.virtual = strings.HasSuffix(l.Case.Name, "subroles")
	desired := canaryTargets(l, 4, 8)
	l.SetSlots(26) // Largest demand is 24, with one surge group per logical role.
	specs := []any{}
	if l.virtual {
		parent := RoleSpec("model", 1, l.Case.Identity, "A", 1, 0, 1, false)
		delete(obj(parent["spec"]), "replicas")
		parent["subRoles"] = childList(pair(4, 8))
		specs = append(specs, parent)
	} else {
		for _, role := range roles {
			specs = append(specs, RoleSpec(role, desired[role], l.Case.Identity, "A", 1, 0, 1, false))
		}
	}
	l.createSet(specs)
	l.Patch(Object{"spec": Object{"scalingPolicy": Object{
		"scaleUpDuringRollout": l.Case.Policy, "scaleDownDuringRollout": l.Case.Policy,
	}}})
	l.Converge("A", desired, 0)
	l.Note("Each logical role has maxSurge=1 and maxUnavailable=0. Canary percentages override both ordinary scaling policies; all templates keep a real readiness gate.")
	return desired
}

// The same sequence exercises both identities, both scaling policies, and
// either ordinary roles or two children sharing one physical LWS.
func percentageCanary(l *Lab) {
	startCanary(l)
	var workloads map[string]bool
	previousPercent := 0
	external := false
	for index, stage := range []struct {
		percent, p, d, newP, newD int
	}{
		{0, 4, 8, 0, 0}, // Install B and its initial hold atomically.
		{25, 4, 8, 1, 2},
		{50, 4, 8, 2, 4},
		{25, 4, 8, 1, 2},
		{0, 4, 8, 0, 0},
		{25, 4, 8, 1, 2},
		{25, 8, 16, 2, 4},
		{25, 4, 8, 1, 2},
		{100, 4, 8, 4, 8},
	} {
		if index == 6 {
			canaryExternalHandoff(l, canaryTargets(l, 4, 8))
			external = true
		}
		before := l.Snapshot()
		desired, updated := canaryTargets(l, stage.p, stage.d), canaryTargets(l, stage.newP, stage.newD)
		old := Targets{}
		for role, n := range desired {
			old[role] = n - updated[role]
		}
		title := fmt.Sprintf("Request %d%% new at %d Prefill / %d Decode: A=%d/%d, B=%d/%d.", stage.percent, stage.p, stage.d, stage.p-stage.newP, stage.d-stage.newD, stage.newP, stage.newD)
		l.Checkpoint(title, "Set canaryPercent and desired replicas; keep template identities unchanged.")
		update := Update{CanaryPercent: &stage.percent, Counts: desired}
		if external {
			update.Counts = nil
		}
		if index == 0 {
			update.Version = "B"
		}
		l.Update(update)
		if external && stage.percent < 100 {
			l.Checkpoint("Resize both External role targets to "+describe(desired)+".", "Write each real RoleScaler /scale subresource; templates and canaryPercent stay unchanged.")
			for _, role := range keys(desired) {
				l.K("-n", Namespace, "scale", "dsrs/"+canaryScalerName(role), fmt.Sprintf("--replicas=%d", desired[role]))
			}
		}
		guard := canarySafety(l, before, desired)
		if stage.percent < previousPercent {
			l.WaitFor("Old-revision replacement groups schedule with readiness still closed", func(s Snapshot) bool {
				guard(s)
				for role := range desired {
					if c := l.Count(s, "A", role); c.Scheduled > c.Ready {
						return true
					}
				}
				return false
			})
			l.Hold("Do not remove Ready B capacity before replacement A becomes Ready", 4, guard)
		}
		l.Checkpoint("Allow the requested split to settle safely.", "Open application readiness for current and newly created A/B groups.")
		s := l.Wait(fmt.Sprintf("%d%% held at A=%s; B=%s", stage.percent, describe(old), describe(updated)), func(s Snapshot) bool {
			return guard(s) && canarySettled(s, old, updated, stage.percent, l.virtual)
		}, 0, 3, func(s Snapshot) {
			guard(s)
			l.Release("A", "", -1, &s)
			l.Release("B", "", -1, &s)
		})
		if index == 1 {
			workloads = uidSet(s.LWS)
			want := 4
			if l.virtual {
				want = 2
			}
			l.Assert(len(workloads) == want, "First 25% hold did not have both physical revisions")
		} else if index > 1 && stage.percent < 100 {
			l.Assert(sameSet(workloads, uidSet(s.LWS)), "Percentage/count edit replaced or deleted a physical revision")
		}
		if stage.percent == 0 {
			l.Assert(len(Pods(s, "B", "")) == 0, "Zero-percent hold retained target Pods")
			if index > 1 {
				l.Note("B's physical LWS objects are retained at replicas=0; returning to 25% reuses their UIDs.")
			}
		}
		l.Hold("Observed split remains Ready without another trigger", 3, func(s Snapshot) bool {
			return guard(s) && canarySettled(s, old, updated, stage.percent, l.virtual)
		})
		previousPercent = stage.percent
	}
}

// At a held 50% canary, introducing C replaces B within the experimental half.
// A's requested groups, Pods and child assignments must not change.
func percentageCanaryChain(l *Lab) {
	desired := startCanary(l)
	half, zero := canaryTargets(l, 2, 4), canaryTargets(l, 0, 0)
	percent := 50
	guard := canarySafety(l, l.Snapshot(), desired)
	l.Checkpoint("Hold B at 50%: A=2/4, B=2/4.", "Install B with canaryPercent=50; open only B's application readiness.")
	l.Update(Update{Version: "B", CanaryPercent: &percent})
	before := l.Wait("A and B each serve half the requested groups", func(s Snapshot) bool {
		return guard(s) && canarySettled(s, half, half, percent, l.virtual)
	}, 0, 3, func(s Snapshot) {
		guard(s)
		l.Release("B", "", -1, &s)
	})
	frozen := canaryStableIdentity(before)
	stable := func(s Snapshot) bool {
		guard(s)
		l.Assert(jsonText(canaryStableIdentity(s)) == jsonText(frozen), "Replacing B with C changed A's LWS spec, Pod identities or subrole labels")
		for role, count := range half {
			c := l.Count(s, "A", role)
			l.Assert(c.Spec == count && c.Ready == count, "A's protected half changed for "+role)
		}
		return true
	}
	l.Checkpoint("Keep A fixed at 50%; replace B with C in the other half.", "Change only the template to C; canaryPercent stays 50 and C readiness remains closed.")
	l.Update(Update{Version: "C"})
	l.WaitFor("C schedules while A stays unchanged", func(s Snapshot) bool {
		stable(s)
		for role := range desired {
			if l.Count(s, "C", role).Scheduled > 0 {
				return true
			}
		}
		return false
	})
	l.Hold("B keeps serving while C is loading; A remains unchanged", 4, stable)
	l.Checkpoint("Allow C to replace B safely.", "Open C's application readiness; keep A's half untouched.")
	settled := func(s Snapshot) bool {
		return stable(s) && canaryVersionsSettled(s, map[string]Targets{"A": half, "B": zero, "C": half}, percent, l.virtual)
	}
	l.Wait("A=2/4 and C=2/4 are Ready; B has no groups", settled, 0, 3, func(s Snapshot) {
		stable(s)
		l.Release("C", "", -1, &s)
	})
	l.Hold("The 50% hold preserves the original A Pods after B→C completes", 3, settled)
}

func canaryStableIdentity(s Snapshot) map[string]string {
	identity := map[string]string{}
	for _, lws := range s.LWS {
		if VersionOf(lws) == "A" {
			identity["lws/"+str(at(lws, "metadata", "uid"))] = jsonText(lws["spec"]) + "/" + str(at(lws, "metadata", "annotations", ChildCounts))
		}
	}
	for _, pod := range s.Pods {
		if label(pod, VersionLabel) == "A" {
			identity["pod/"+str(at(pod, "metadata", "uid"))] = label(pod, SubroleLabel)
		}
	}
	return identity
}

func canaryScalerName(role string) string { return SetName + "-" + strings.ReplaceAll(role, "/", "-") }

func canaryExternalHandoff(l *Lab, desired Targets) {
	before := l.Snapshot()
	l.Checkpoint("Give the held canary to External scalers without losing capacity.", "Switch both logical roles from Static to External; delay the first /scale write.")
	l.Update(Update{ExternalRoles: keys(desired)})
	preserved := func(s Snapshot) bool {
		l.Assert(sameSet(uidSet(s.Pods), uidSet(before.Pods)), "Canary Static→External handoff replaced an existing Pod")
		l.Assert(sameSet(uidSet(s.LWS), uidSet(before.LWS)), "Canary Static→External handoff changed a workload revision")
		if len(s.Scalers) != len(desired) {
			return false
		}
		for _, scaler := range s.Scalers {
			role := label(scaler, RoleLabel)
			if child := label(scaler, SubroleLabel); child != "" {
				role += "/" + child
			}
			l.Assert(integer(at(scaler, "spec", "replicas"), -1) == desired[role], "Canary scaler did not inherit the existing desired count for "+role)
		}
		return at(s.DS, "status", "observedGeneration") == at(s.DS, "metadata", "generation")
	}
	l.WaitFor("Both scalers inherit 4 Prefill / 8 Decode; every existing Pod survives", preserved)
	l.Hold("No first autoscaler write yet: the held canary stays unchanged", 3, preserved)
}

// Ratchet only observed Ready capacity, never count requested growth as Ready.
// After a desired reduction, the new desired is the availability floor. Native
// terminating Pods can outlive a spec reduction, so surge checks use LWS specs.
func canarySafety(l *Lab, before Snapshot, desired Targets) Predicate {
	floors, ceilings := Targets{}, Targets{}
	for role, count := range desired {
		floors[role] = min(count, l.Total(before, role, "ready"))
		parent, _, _ := strings.Cut(role, "/")
		ceilings[parent] += max(count+1, l.Total(before, role, "spec"))
	}
	return func(s Snapshot) bool {
		for role, count := range desired {
			ready := l.Total(s, role, "ready")
			l.Assert(ready >= floors[role], "Canary crossed observed Ready floor for "+role)
			serving := 0
			for _, version := range []string{"A", "B", "C"} {
				complete := true
				for peer, n := range desired {
					complete = complete && (n == 0 || l.Count(s, version, peer).Ready > 0)
				}
				if complete {
					serving += l.Count(s, version, role).Ready
				}
			}
			l.Assert(serving >= floors[role], "Canary crossed same-revision serving floor for "+role)
			floors[role] = max(floors[role], min(count, ready))
		}
		physical := Targets{}
		for _, workload := range s.LWS {
			physical[label(workload, RoleLabel)] += integer(at(workload, "spec", "replicas"), 1)
		}
		for parent, issued := range physical {
			l.Assert(issued <= ceilings[parent], "Canary exceeded physical requested surge ceiling for "+parent)
		}
		return true
	}
}

func canarySettled(s Snapshot, old, updated Targets, percent int, virtual bool) bool {
	return canaryVersionsSettled(s, map[string]Targets{"A": old, "B": updated}, percent, virtual)
}

func canaryVersionsSettled(s Snapshot, versions map[string]Targets, percent int, virtual bool) bool {
	counts := GroupCounts
	if virtual {
		counts = func(s Snapshot, v string) map[string]Counts { return SubroleCounts(s, v, true) }
	}
	desired := Targets{}
	for version, expected := range versions {
		actual := counts(s, version)
		for role, count := range expected {
			desired[role] += count
			if actual[role].Spec != count || actual[role].Ready != count {
				return false
			}
		}
	}
	if len(s.Pods) != totalTargets(desired) || !allPods(s.Pods, Ready) {
		return false
	}
	for _, workload := range s.LWS {
		if deleting(workload) || str(at(workload, "metadata", "annotations", Prefix+"subrole-plan")) != "" {
			return false
		}
	}
	for _, scaler := range s.Scalers {
		role := label(scaler, RoleLabel)
		if child := label(scaler, SubroleLabel); child != "" {
			role += "/" + child
		}
		if integer(at(scaler, "spec", "replicas"), -1) != desired[role] || at(scaler, "status", "observedGeneration") != at(scaler, "metadata", "generation") {
			return false
		}
	}
	generation := at(s.DS, "metadata", "generation")
	if s.DS == nil || at(s.DS, "status", "observedGeneration") != generation || integer(at(s.DS, "spec", "rolloutStrategy", "canaryPercent"), -1) != percent {
		return false
	}
	want, kind := "CanaryHeld", "Progressing"
	if percent == 100 {
		want, kind = "AllRolesReady", "Available"
	}
	for _, condition := range objects(at(s.DS, "status", "conditions")) {
		if condition["type"] == kind && condition["reason"] == want && condition["status"] == "True" && condition["observedGeneration"] == generation {
			return true
		}
	}
	return false
}
