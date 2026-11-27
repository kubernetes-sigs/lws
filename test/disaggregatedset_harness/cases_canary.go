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
	l.Note("Each logical role has maxSurge=1 and maxUnavailable=0. Only Immediate admits demand changes during a partial canary; Deferred queues them. All templates keep a real readiness gate.")
	return desired
}

// The same sequence exercises both identities, both scaling policies, and
// either ordinary roles or two children sharing one physical LWS.
func percentageCanary(l *Lab) {
	startCanary(l)
	var workloads map[string]bool
	var prefillPods map[string]bool
	external := false
	for index, stage := range []struct {
		percent, p, d, newP, newD int
	}{
		{0, 4, 8, 0, 0}, // Install B and its initial hold atomically.
		{25, 4, 8, 1, 2},
		{25, 8, 16, 2, 4},
		{50, 8, 16, 4, 8}, // Promotion proceeds even when Deferred queues demand.
		{50, 2, 8, 1, 4},  // Resize after promotion settles; these are separate API writes.
		{50, 1, 8, 1, 4},  // Keep the issued Prefill pair.
		{50, 1, 0, 1, 0},
		{50, 1, 1, 1, 0}, // Decode resumes on A; do not restart Prefill.
		{50, 2, 2, 1, 1},
		{50, 4, 8, 2, 4}, // Settle demand before finalization; separate /scale writes are not atomic.
		{100, 4, 8, 4, 8},
		{100, 8, 16, 8, 16},
		{100, 4, 8, 4, 8},
	} {
		if index == 2 {
			canaryExternalHandoff(l, canaryTargets(l, 4, 8))
			external = true
		}
		before := l.Snapshot()
		desired := canaryTargets(l, stage.p, stage.d)
		admittedP, admittedD := stage.p, stage.d
		if l.Case.Policy == "Deferred" && stage.percent < 100 {
			admittedP, admittedD = 4, 8
			stage.newP, stage.newD = 4*stage.percent/100, 8*stage.percent/100
		}
		admitted := canaryTargets(l, admittedP, admittedD)
		updated := canaryTargets(l, stage.newP, stage.newD)
		old := Targets{}
		settled := true
		for role, n := range admitted {
			old[role] = n - updated[role]
			if stage.percent > 0 && stage.percent < 100 && n == 1 && updated[role] == 1 {
				old[role] = 1 // One extra group retains the last baseline/canary pair.
			}
			settled = settled && old[role]+updated[role] == desired[role]
		}
		title := fmt.Sprintf("Request %d%% new at %d Prefill / %d Decode: A=%s, B=%s.", stage.percent, stage.p, stage.d, describe(old), describe(updated))
		l.Checkpoint(title, "Set canary.percent and desired replicas; keep template identities unchanged.")
		update := Update{CanaryPercent: &stage.percent, Counts: desired}
		if external {
			update.Counts = nil
		}
		if index == 0 {
			update.Version = "B"
		}
		l.Update(update)
		if external {
			l.Checkpoint("Resize both External role targets to "+describe(desired)+".", "Write each real RoleScaler /scale subresource; templates and canary.percent stay unchanged.")
			for _, role := range keys(desired) {
				l.K("-n", Namespace, "scale", "dsrs/"+canaryScalerName(role), fmt.Sprintf("--replicas=%d", desired[role]))
			}
		}
		guard := canarySafety(l, before, admitted)
		check := func(s Snapshot) bool {
			guard(s)
			if index >= 5 && index <= 8 {
				l.Assert(sameSet(prefillPods, canaryPrefillPods(s)), "Singleton or peer-resume scaling restarted an existing Prefill Pod")
			}
			return canaryObserved(s, map[string]Targets{"A": old, "B": updated}, desired, stage.percent, l.virtual, settled)
		}
		l.Checkpoint("Allow the requested split to settle safely.", "Open application readiness for current and newly created A/B groups.")
		s := l.Wait(fmt.Sprintf("%d%% observed at A=%s; B=%s; settled=%t", stage.percent, describe(old), describe(updated), settled), check, 0, 3, func(s Snapshot) {
			guard(s)
			l.Release("A", "", -1, &s)
			l.Release("B", "", -1, &s)
		})
		if index == 4 {
			prefillPods = canaryPrefillPods(s)
			l.Checkpoint("Reject 50% → 25%; keep the current groups.", "Ask admission to decrease the percentage; expect rejection, without changing templates.")
			_, stderr, err := l.Options.Executor.Run(l.ctx, l.Options.Kubectl, l.Options.kube("-n", Namespace, "patch", DSResource, SetName, "--type=merge", "-p", `{"spec":{"rolloutStrategy":{"canary":{"percent":25}}}}`), nil)
			l.Assert(err != nil && strings.Contains(string(stderr), "cannot decrease percent"), "Percentage decrease was not rejected by admission")
		}
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
		}
		if stage.percent == 100 {
			l.Assert(len(Pods(s, "A", "")) == 0, "Completed rollout retained baseline Pods")
			l.WaitFor("100% finalizes without another edit: A's empty LWS objects are removed", func(s Snapshot) bool {
				for _, lws := range s.LWS {
					if VersionOf(lws) == "A" {
						return false
					}
				}
				return canarySettled(s, old, updated, 100, l.virtual)
			})
			l.Note("B is accepted as the next baseline. Rollback after completion requires restoring templates in Git.")
		}
		if !settled {
			l.Note("The request is visible, but the step is not settled: Deferred queued demand, or a retained pair uses one extra group.")
		}
		l.Hold("Observed split remains Ready without another trigger", 3, check)
	}
	l.Checkpoint("Omission is equivalent to completed 100%.", "Remove canary.percent after verifying that B is already available and A has been cleaned up.")
	l.Patch(Object{"spec": Object{"rolloutStrategy": Object{"canary": Object{"percent": nil}}}})
	l.WaitFor("B is available and A's empty LWS objects have been removed", func(s Snapshot) bool {
		for _, lws := range s.LWS {
			if VersionOf(lws) == "A" {
				return false
			}
		}
		return canarySettled(s, canaryTargets(l, 0, 0), canaryTargets(l, 4, 8), -1, l.virtual)
	})
}

func canaryPrefillPods(s Snapshot) map[string]bool {
	pods := []Object{}
	for _, pod := range s.Pods {
		if label(pod, RoleLabel) == "prefill" || label(pod, SubroleLabel) == "prefill" {
			pods = append(pods, pod)
		}
	}
	return uidSet(pods)
}

// At a held 50% canary, introducing C replaces B within the experimental half.
// A's requested groups, Pods and child assignments must not change.
func percentageCanaryChain(l *Lab) {
	desired := startCanary(l)
	half, zero := canaryTargets(l, 2, 4), canaryTargets(l, 0, 0)
	percent := 50
	guard := canarySafety(l, l.Snapshot(), desired)
	l.Checkpoint("Hold B at 50%: A=2/4, B=2/4.", "Install B with canary.percent=50; open only B's application readiness.")
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
	l.Checkpoint("Keep A fixed at 50%; replace B with C in the other half.", "Change only the template to C; canary.percent stays 50 and C readiness remains closed.")
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
	servingReady := func(s Snapshot, role string) int {
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
		return serving
	}
	floors, ceilings := Targets{}, Targets{}
	for role, count := range desired {
		floors[role] = min(count, servingReady(before, role))
		parent, _, _ := strings.Cut(role, "/")
		ceilings[parent] += max(count+1, l.Total(before, role, "spec"))
	}
	return func(s Snapshot) bool {
		for role, count := range desired {
			ready := l.Total(s, role, "ready")
			l.Assert(ready >= floors[role], "Canary crossed observed Ready floor for "+role)
			serving := servingReady(s, role)
			l.Assert(serving >= floors[role], "Canary crossed same-revision serving floor for "+role)
			floors[role] = max(floors[role], min(count, serving))
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
	return canaryObserved(s, versions, nil, percent, virtual, true)
}

func canaryObserved(s Snapshot, versions map[string]Targets, requested Targets, percent int, virtual, settled bool) bool {
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
	if requested != nil {
		desired = requested
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
	if s.DS == nil || at(s.DS, "status", "observedGeneration") != generation || integer(at(s.DS, "spec", "rolloutStrategy", "canary", "percent"), -1) != percent {
		return false
	}
	want, kind := "CanaryHeld", "Progressing"
	if !settled {
		want = "RolloutInProgress"
		if at(s.DS, "status", "canary", "settled") != false {
			return false
		}
	}
	if percent < 0 || percent == 100 { // Omission and 100 both finalize.
		want, kind = "AllRolesReady", "Available"
	}
	for _, condition := range objects(at(s.DS, "status", "conditions")) {
		if condition["type"] == kind && condition["reason"] == want && condition["status"] == "True" && condition["observedGeneration"] == generation {
			return true
		}
	}
	return false
}
