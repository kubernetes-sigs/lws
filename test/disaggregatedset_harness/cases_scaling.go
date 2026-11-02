// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"fmt"
	"strings"
)

func mixed(l *Lab) Snapshot {
	l.Create(4, 4, CreateOptions{})
	l.Checkpoint("Start B; Pods schedule but application readiness stays closed.", "Change both role templates A → B.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	l.WaitFor("B has a scheduled loading group in both roles", func(s Snapshot) bool { return both(func(r string) bool { return l.Count(s, "B", r).Scheduled >= 1 }) })
	l.Checkpoint("Release one B group in each role.", "Open their /tmp/ready files.")
	for _, r := range roles {
		l.Release("B", r, 1, nil)
	}
	l.WaitFor("Next Prefill group is running", func(s Snapshot) bool { return l.Count(s, "B", "prefill").Scheduled >= 2 })
	l.Checkpoint("Release a second Prefill; Decode stays one group behind.", "Open one additional Prefill readiness file.")
	l.Release("B", "prefill", 2, nil)
	return l.WaitFor("B Ready at 2 Prefill / 1 Decode, next groups scheduled", func(s Snapshot) bool {
		p, d := l.Count(s, "B", "prefill"), l.Count(s, "B", "decode")
		return p.Ready == 2 && d.Ready == 1 && p.Scheduled >= 3 && d.Scheduled >= 2
	})
}

func scaleUp(l *Lab) {
	before := mixed(l)
	old, target := l.Counts(before, "A"), l.Counts(before, "B")
	started := uidSet(Pods(before, "B", ""))
	revision := podRevisions(before, "B")
	l.Checkpoint("Raise desired 4 → 6 per role. Missing capacity is not Ready capacity.", "Patch only spec.replicas to six.")
	l.Update(Update{Counts: pair(6, 6)})
	l.Checkpoint("Let currently loading groups finish; hold next batch.", "Release B up to three Ready Prefill / two Ready Decode.")
	l.Release("B", "prefill", 3, nil)
	l.Release("B", "decode", 2, nil)
	protected := func(s Snapshot) bool {
		l.Assert(subset(started, uidSet(Pods(s, "B", ""))), "Scale-up replaced scheduled target startup work")
		for _, r := range roles {
			a, b := l.Count(s, "A", r), l.Count(s, "B", r)
			l.Assert(a.Spec <= old[r].Spec, "Demand increase recreated old replicas")
			l.Assert(b.Spec >= target[r].Spec, "Scale-up cancelled scheduled target replicas")
			old[r], target[r] = a, b
			l.Assert(l.Total(s, r, "ready") >= 4, "Scale-up destroyed already-Ready capacity")
		}
		return true
	}
	growing := func(s Snapshot) bool {
		return protected(s) && both(func(r string) bool { return l.Total(s, r, "ready") >= 5 })
	}
	l.WaitFor("Preserve five Ready groups while building toward floor six", growing)
	l.Hold("Slow startup does not permit another old drain", 8, growing)
	l.Assert(sameSet(podRevisions(l.Snapshot(), "B"), revision), "Replica-only edit created another B revision")
	l.Checkpoint("Release remaining B; finish at exactly 6+6.", "Open current and subsequently created B readiness files.")
	l.Converge("B", pair(6, 6), 0)
}

func scaleDown(l *Lab) {
	before := mixed(l)
	old, target := l.Counts(before, "A"), l.Counts(before, "B")
	neededDecode := uidSet(Pods(before, "B", "decode"))
	revision := podRevisions(before, "B")
	l.Checkpoint("Lower desired 4 → 2 per role; prefer safe old drains.", "Patch only spec.replicas to two; preserve needed loading work.")
	l.Update(Update{Counts: pair(2, 2)})
	safe := func(s Snapshot) bool {
		l.Assert(sameSet(neededDecode, uidSet(Pods(s, "B", "decode"))), "Scale-down replaced needed scheduled Decode group")
		for _, r := range roles {
			a, b := l.Count(s, "A", r), l.Count(s, "B", r)
			l.Assert(a.Spec <= old[r].Spec, "Scale-down recreated old replicas")
			old[r] = a
			l.Assert(b.Spec <= target[r].Spec, "Scale-down grew sufficient target replicas")
			l.Assert(b.Spec >= min(target[r].Spec, 2), "Scale-down cancelled startup work below final desired")
			l.Assert(l.Total(s, r, "ready") >= 2, "Scale-down crossed new availability floor")
		}
		return true
	}
	expected := target["prefill"].Spec
	if l.Case.Policy == "Immediate" {
		expected = 2
	}
	l.WaitFor("Old replicas shrink while B's second Decode still loads", func(s Snapshot) bool {
		return safe(s) && both(func(r string) bool { return l.Count(s, "A", r).Spec <= 1 }) && l.Count(s, "B", "prefill").Spec == expected
	})
	l.Hold("Needed scheduled groups stay; real excess may go", 8, safe)
	s := l.Snapshot()
	l.Assert(l.Count(s, "B", "prefill").Spec == expected, "Target scale-down did not follow selected policy")
	l.Assert(sameSet(podRevisions(s, "B"), revision), "Replica-only edit created another B revision")
	l.Checkpoint("Let remaining Decode become Ready; finish at exactly 2+2.", "Open all B readiness files.")
	l.Converge("B", pair(2, 2), 0)
}

func handoffPreserved(l *Lab, s, before Snapshot) {
	l.Assert(sameSet(uidSet(s.Pods), uidSet(before.Pods)), "Static→External deleted/replaced an existing Pod")
	l.Assert(sameSet(uidSet(s.LWS), uidSet(before.LWS)), "Replica source change created/deleted workload revision")
	for r, old := range l.Counts(before, "A") {
		now := l.Count(s, "A", r)
		l.Assert(now.Spec == old.Spec && now.Ready == old.Ready, "Static→External changed "+r+" requested or Ready count")
	}
}

func autoscalerHandoff(l *Lab) {
	l.Create(3, 2, CreateOptions{Size: 2})
	before := l.Snapshot()
	l.Assert(len(before.Scalers) == 0, "Static role unexpectedly has a scaler")
	l.Checkpoint("Hand Prefill's three Ready groups to an external scaler; keep every Pod.", "Set scaling.mode=External and remove inline replicas in one patch; templates unchanged.")
	l.Update(Update{ExternalRoles: []string{"prefill"}})
	scalerName := SetName + "-prefill"
	seeded := func(s Snapshot) bool {
		handoffPreserved(l, s, before)
		if len(s.Scalers) == 0 {
			return false
		}
		l.Assert(len(s.Scalers) == 1 && name(s.Scalers[0]) == scalerName, "Only External Prefill should get a scaler")
		scaler := s.Scalers[0]
		l.Assert(integer(at(scaler, "spec", "replicas")) == 3, "New scaler must start at three groups, not zero/one/six Pods")
		owned := false
		for _, owner := range objects(at(scaler, "metadata", "ownerReferences")) {
			owned = owned || (owner["controller"] == true && owner["uid"] == uid(before.DS))
		}
		l.Assert(owned, "Scaler not owned by this DisaggregatedSet")
		return integer(at(scaler, "status", "replicas")) == 3 && at(s.DS, "status", "observedGeneration") == at(s.DS, "metadata", "generation")
	}
	l.WaitFor("Scaler inherits three replicas without replacing any Pod", seeded)
	scale := l.JSON("get", "--raw", "/apis/disaggregatedset.x-k8s.io/v1/namespaces/"+Namespace+"/disaggregatedsetrolescalers/"+scalerName+"/scale")
	l.Assert(integer(at(scale, "spec", "replicas")) == 3 && integer(at(scale, "status", "replicas")) == 3, "First /scale read did not see three existing groups")
	l.Hold("No autoscaler write for 15 seconds; ten original Pods remain Ready", 15, seeded)
	for _, change := range [][2]int{{3, 4}, {4, 2}} {
		previous, desired := change[0], change[1]
		prior := l.Snapshot()
		l.Checkpoint(fmt.Sprintf("External scaler requests Prefill %d → %d; Decode stays two.", previous, desired), "Write the real /scale subresource.")
		l.K("-n", Namespace, "scale", "dsrs/"+scalerName, fmt.Sprintf("--replicas=%d", desired))
		l.Wait(fmt.Sprintf("/scale reaches %dP/2D without changing templates", desired), func(s Snapshot) bool {
			l.Assert(sameSet(uidSet(s.LWS), uidSet(before.LWS)), "/scale created another revision")
			l.Assert(sameSet(uidSet(Pods(s, "", "decode")), uidSet(Pods(before, "", "decode"))), "Scaling Prefill replaced Decode Pod")
			d, p := l.Count(s, "A", "decode"), l.Count(s, "A", "prefill")
			l.Assert(d.Spec == 2 && d.Ready == 2, "Scaling Prefill disturbed Decode")
			l.Assert(p.Spec >= min(previous, desired) && p.Spec <= max(previous, desired) && p.Ready >= min(previous, desired), "External scaling drained needed Prefill")
			if desired > previous {
				l.Assert(subset(uidSet(prior.Pods), uidSet(s.Pods)), "Scale-up replaced existing Pods")
			}
			return p.Spec == desired && p.Ready == desired && len(s.Pods) == (desired+2)*2 && allPods(s.Pods, Ready)
		}, 0, 3, nil)
	}
	l.Note("This tests the autoscaler handoff and /scale API, not a metrics-driven HPA.")
}

func restartRollback(l *Lab) {
	l.Create(4, 4, CreateOptions{Unavailable: 1})
	l.Checkpoint("Start B with readiness held.", "Change A templates to B.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	l.WaitFor("B has scheduled groups", func(s Snapshot) bool { return both(func(r string) bool { return l.Count(s, "B", r).Scheduled >= 1 }) })
	l.Checkpoint("Make one B group Ready per role; rollout unfinished.", "Open one group per role.")
	for _, r := range roles {
		l.Release("B", r, 1, nil)
	}
	s := l.WaitFor("B has one Ready group per role", func(s Snapshot) bool { return both(func(r string) bool { return l.Count(s, "B", r).Ready == 1 }) })
	original := podRevisions(s, "B")
	l.Checkpoint("Interrupt B with C.", "Patch templates to C; readiness held.")
	l.Update(Update{Version: "C", Ready: boolPtr(false)})
	l.WaitFor("Interrupted C revision exists", func(s Snapshot) bool { return len(Pods(s, "C", "")) > 0 })
	l.Checkpoint("Restart real controller during interrupted rollout.", "Delete owned manager Pod; wait for replacement and admission.")
	l.RestartController()
	l.Checkpoint("Roll back to B with new observer active.", "Restore exact B template, including readiness gate.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	l.Hold("Restarted controller does not spend allowance twice", 8, func(s Snapshot) bool { return both(func(r string) bool { return l.Total(s, r, "ready") >= 3 }) })
	l.Assert(sameSet(podRevisions(l.Snapshot(), "B"), original), "Rollback did not reuse B revision")
	l.Checkpoint("Release B and complete rollback.", "Open B readiness files.")
	l.Converge("B", pair(4, 4), 0)
}

func childList(counts Targets) []any {
	out := []any{}
	for _, n := range keys(counts) {
		out = append(out, Object{"name": n, "replicas": counts[n]})
	}
	return out
}

func (l *Lab) SubrolesCreate(counts Targets) {
	defer l.action("subroles_create", []any{counts}, nil, nil)()
	parent := RoleSpec("decode", 1, l.Case.Identity, "A", 1, 0, 2, true)
	delete(obj(parent["spec"]), "replicas")
	parent["subRoles"] = childList(counts)
	l.createSet([]any{parent})
	target := Targets{}
	for n, v := range counts {
		target["decode/"+n] = v
	}
	l.Converge("A", target, 0)
}

func (l *Lab) SubrolesUpdate(counts Targets) {
	defer l.action("subroles_update", nil, Object{"counts": counts}, nil)()
	ds := l.JSON("-n", Namespace, "get", DSResource, SetName, "-o", "json")
	parent := objects(at(ds, "spec", "roles"))[0]
	parent["subRoles"] = childList(counts)
	l.Patch(Object{
		"spec": Object{
			"roles": []any{parent},
		},
	})
}

func assigned(s Snapshot, desired Targets, version string) bool {
	issued := SubroleCounts(s, version, false)
	observed := Targets{}
	published := Targets{}
	for k, c := range issued {
		published[k] = c.Spec
	}
	if !sameTargets(published, desired) {
		return false
	}
	for _, lws := range s.LWS {
		if VersionOf(lws) == version && str(at(lws, "metadata", "annotations", Prefix+"subrole-plan")) != "" {
			return false
		}
	}
	for _, p := range s.Pods {
		if deleting(p) && label(p, VersionLabel) == version {
			return false
		}
	}
	for _, g := range Groups(s, version, "decode") {
		if len(g) != 2 || !allPods(g, Ready) {
			return false
		}
		child := label(g[0], SubroleLabel)
		indices := map[string]bool{}
		for _, p := range g {
			if child == "" || label(p, SubroleLabel) != child {
				return false
			}
			indices[label(p, WorkerIndex)] = true
		}
		if len(indices) != 2 || !indices["0"] || !indices["1"] {
			return false
		}
		observed[child]++
	}
	return sameTargets(observed, desired) && GroupCounts(s, version)["decode"].Spec == totalTargets(desired)
}

func subroles(l *Lab) {
	l.SubrolesCreate(Targets{"a": 2, "b": 1})
	initial := l.WaitFor("Shared LWS has a=2/b=1 whole Ready groups", func(s Snapshot) bool { return assigned(s, Targets{"a": 2, "b": 1}, "A") })
	original, revision := uidSet(Pods(initial, "", "")), podRevisions(initial, "A")
	l.Checkpoint("Move assignment a=2/b=1 → a=1/b=2 without restart.", "Patch children; physical total stays three.")
	l.SubrolesUpdate(Targets{"a": 1, "b": 2})
	s := l.WaitFor("Assignments changed without replacing Pods", func(s Snapshot) bool { return assigned(s, Targets{"a": 1, "b": 2}, "A") })
	l.Assert(sameSet(uidSet(Pods(s, "", "")), original), "Relabelling replaced Pod")
	l.Checkpoint("Shrink b=2 → 1; restart manager during assignment recovery.", "Patch b and restart owned controller.")
	l.SubrolesUpdate(Targets{"a": 1, "b": 1})
	l.RestartController()
	s = l.WaitFor("Two physical groups with coherent child labels", func(s Snapshot) bool { return assigned(s, Targets{"a": 1, "b": 1}, "A") })
	survivors := uidSet(Pods(s, "", ""))
	l.Assert(subset(survivors, original), "Shrinking replaced unrelated surviving group")
	l.Assert(sameSet(podRevisions(s, "A"), revision), "Child-count edit created revision")
	l.Checkpoint("Grow b=1 → 2; labels do not substitute for capacity.", "Parent grows from two to three.")
	l.SubrolesUpdate(Targets{"a": 1, "b": 2})
	s = l.WaitFor("Physical growth restores a=1/b=2", func(s Snapshot) bool { return assigned(s, Targets{"a": 1, "b": 2}, "A") })
	l.Assert(subset(survivors, uidSet(Pods(s, "", ""))), "Growth replaced existing group")
	live := 0
	for _, o := range s.LWS {
		if !deleting(o) {
			live++
		}
	}
	l.Assert(live == 1, "Subroles created multiple physical LWSs")
	l.Checkpoint("Roll shared template to B; each child maxSurge=1 / maxUnavailable=0.", "Hold B readiness.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	l.WaitFor("B groups started without spending old child availability", func(s Snapshot) bool { return len(Pods(s, "B", "")) > 0 })
	l.Hold("Unready B cannot remove A needed child groups", 8, func(s Snapshot) bool { return assigned(s, Targets{"a": 1, "b": 2}, "A") })
	l.Checkpoint("Make B Ready and finish both child rollouts.", "Open all B readiness.")
	l.Converge("B", Targets{"decode/a": 1, "decode/b": 2}, 0)
	l.WaitFor("Target template has coherent a=1/b=2 Ready groups", func(s Snapshot) bool { return assigned(s, Targets{"a": 1, "b": 2}, "B") })
	if l.Case.Identity == "Hash" {
		l.Note("Hash deletion costs are preferences; this checks eventual coherent repair, not guaranteed child survivors.")
	}
}

func nativeRestart(l *Lab) {
	l.Create(2, 1, CreateOptions{Size: 2})
	for _, member := range []string{"worker", "leader"} {
		index := "1"
		if member == "leader" {
			index = "0"
		}
		s := l.Snapshot()
		var victim Object
		for _, p := range Pods(s, "A", "prefill") {
			if label(p, WorkerIndex) == index {
				victim = p
				break
			}
		}
		l.Assert(victim != nil, "Crash target exists")
		group := label(victim, GroupLabel)
		affected := map[string]bool{}
		for _, p := range Pods(s, "A", "prefill") {
			if label(p, GroupLabel) == group {
				affected[uid(p)] = true
			}
		}
		unaffected := uidSet(Pods(s, "", ""))
		for id := range affected {
			delete(unaffected, id)
		}
		l.Checkpoint("Crash one "+member+"; native policy recreates only its group.", "touch /tmp/crash in "+name(victim))
		l.K("-n", Namespace, "exec", name(victim), "--", "touch", "/tmp/crash")
		l.WaitFor(member+" group replaced; all other UIDs unchanged", func(s Snapshot) bool {
			current := uidSet(Pods(s, "", ""))
			l.Assert(subset(unaffected, current), member+" crash restarted unrelated group")
			for id := range affected {
				if current[id] {
					return false
				}
			}
			return len(current) == 6 && len(s.Pods) == 6 && allPods(Pods(s, "", ""), Ready)
		})
		recovered := uidSet(Pods(l.Snapshot(), "", ""))
		l.Hold("No replacement loop after "+member+" recovery", 12, func(s Snapshot) bool {
			return sameSet(uidSet(Pods(s, "", "")), recovered) && allPods(Pods(s, "", ""), Ready)
		})
	}
	l.Note("RecreateGroupOnPodRestart changes UIDs only in the failed group; not the in-place prototype.")
}

func childTargets(parent string, t Targets) Targets {
	out := Targets{}
	for k, n := range t {
		if strings.HasPrefix(k, parent+"/") {
			out[strings.TrimPrefix(k, parent+"/")] = n
		}
	}
	return out
}
