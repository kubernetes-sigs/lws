// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import "time"

var roles = []string{"prefill", "decode"}
var renamed = []string{"prefill-v2", "decode-v2"}

func both(p func(string) bool) bool {
	for _, r := range roles {
		if !p(r) {
			return false
		}
	}
	return true
}
func ordinary(l *Lab) {
	l.Create(2, 4, CreateOptions{})
	initial := l.Snapshot()
	revision, workloads := revisions(initial, ""), allUIDs(initial.LWS)
	for _, desired := range []Targets{pair(4, 6), pair(2, 4)} {
		l.Checkpoint("Steady-state resize to "+describe(desired), "Change replica counts only; keep template A.")
		l.Update(Update{Counts: desired})
		s := l.Converge("A", desired, 0)
		l.Assert(sameSet(revisions(s, ""), revision), "A count-only edit created a new revision")
		l.Assert(sameSet(allUIDs(s.LWS), workloads), "Steady scaling replaced a physical LeaderWorkerSet")
	}
	l.Checkpoint("Roll 2 Prefill / 4 Decode to B; maxSurge=1 / maxUnavailable=0 per role.", "Change templates to B; hold their application readiness.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	l.WaitFor("Both target roles have a scheduled loading group", func(s Snapshot) bool { return both(func(r string) bool { return l.Count(s, "B", r).Scheduled >= 1 }) })
	l.Checkpoint("Make Prefill Ready first; Decode is deliberately slow.", "Open only B's first Prefill readiness file.")
	l.Release("B", "prefill", 1, nil)
	l.WaitFor("B Prefill is Ready, but B Decode is not", func(s Snapshot) bool {
		return l.Count(s, "B", "prefill").Ready == 1 && l.Count(s, "B", "decode").Ready == 0
	})
	l.Hold("Healthy Prefill cannot make an incomplete B revision serve Decode traffic", 8, func(s Snapshot) bool {
		for r, floor := range pair(2, 4) {
			usable := 0
			for _, v := range []string{"A", "B"} {
				if both(func(peer string) bool { return l.Count(s, v, peer).Ready > 0 }) {
					usable += l.Count(s, v, r).Ready
				}
			}
			l.Assert(l.Total(s, r, "ready") >= floor, "Independent "+r+" Ready floor was crossed")
			l.Assert(usable >= floor, "Same-revision serving capacity for "+r+" was lost")
		}
		return true
	})
	l.Note("Ordinary surge ceilings are Prefill=3, Decode=5. Bootstrap is separately marked in observer/events.")
	l.Checkpoint("Release Decode and all subsequent B groups.", "Open B readiness files until rollout completes.")
	l.Converge("B", pair(2, 4), 0)
}
func renamedRollout(l *Lab, old, desired Targets, newUnavailable int) {
	l.Create(old["prefill"], old["decode"], CreateOptions{Surge: 0, Unavailable: 1, Slots: totalTargets(old)})
	target := Targets{renamed[0]: desired["prefill"], renamed[1]: desired["decode"]}
	l.Checkpoint("Rename both roles; preserve the saved old availability allowance.", "Replace role names, request "+describe(target)+", hold B readiness.")
	l.Update(Update{Version: "B", Names: renamed, Counts: target, Surge: 0, Unavailable: newUnavailable, Ready: boolPtr(false)})
	l.WaitFor("Old capacity releases before the first renamed pair schedules", func(s Snapshot) bool {
		for i, r := range roles {
			if l.Count(s, "A", r).Spec != old[r]-1 {
				return false
			}
			c := l.Count(s, "B", renamed[i])
			if c.Spec != 1 || c.Scheduled != 1 {
				return false
			}
		}
		return true
	})
	l.Checkpoint("Only new Prefill becomes Ready; new Decode must get its turn.", "Open first new Prefill; keep Decode closed.")
	l.Release("B", renamed[0], 1, nil)
	l.WaitFor("New Prefill Ready; new Decode loading", func(s Snapshot) bool {
		return l.Count(s, "B", renamed[0]).Ready == 1 && l.Count(s, "B", renamed[1]).Ready == 0
	})
	l.Hold("No repeated fast-role growth, no extra old-budget spending", 8, func(s Snapshot) bool {
		for i, r := range roles {
			l.Assert(l.Count(s, "A", r).Ready >= old[r]-1, "Renaming spent more than saved old "+r+" allowance")
			l.Assert(l.Count(s, "B", renamed[i]).Spec <= 1, "Fast renamed role grew before all target roles became Ready")
		}
		l.Assert(!eventReason(s, "BootstrapSurge"), "Bootstrap bypassed renamed-role released capacity")
		l.Assert(!eventReason(s, "AvailabilityFallback"), "Renamed roles used same-role scheduler recovery")
		return true
	})
	l.Checkpoint("Release Decode and replace using the same fixed slots.", "Open all present and future B readiness files.")
	l.Converge("B", target, 360*time.Second)
}
func renameCapacity(l *Lab, s Snapshot, t Targets) bool {
	for i, r := range roles {
		l.Assert(l.Count(s, "A", r).Spec+l.Count(s, "B", renamed[i]).Spec <= t[r], "Renaming "+r+" exceeded zero-surge ceiling")
	}
	l.Assert(!eventReason(s, "BootstrapSurge") && !eventReason(s, "AvailabilityFallback"), "Full-downtime rename should not need emergency capacity")
	return true
}
func renameOutage(l *Lab) {
	t := pair(2, 3)
	l.Create(2, 3, CreateOptions{Surge: 0, Unavailable: "100%", Slots: 5})
	l.Checkpoint("Rename 2P/3D with maxSurge=0 and maxUnavailable=100%. Full downtime is permitted.", "Rename both roles, change A to B, hold all B readiness.")
	l.Update(Update{Version: "B", Names: renamed, Ready: boolPtr(false)})
	loading := func(s Snapshot) bool {
		renameCapacity(l, s, t)
		for i, r := range roles {
			c := l.Count(s, "B", renamed[i])
			l.Assert(c.Ready == 0, "Replacement readiness opened before the manual event")
			if l.Count(s, "A", r).Spec != 0 || c.Spec != t[r] || c.Scheduled != t[r] {
				return false
			}
		}
		return len(s.Pods) == 5 && len(Pods(s, "A", "")) == 0
	}
	l.WaitFor("All old groups drained; all five renamed groups scheduled, none Ready", loading)
	l.Hold("Zero Ready is allowed; no sixth slot or emergency allowance needed", 8, loading)
	l.Checkpoint("Make both renamed Prefill groups Ready; Decode still loads.", "Open only B Prefill readiness files.")
	l.Release("B", renamed[0], -1, nil)
	l.WaitFor("Renamed Prefill Ready, Decode unready", func(s Snapshot) bool {
		return renameCapacity(l, s, t) && l.Count(s, "B", renamed[0]).Ready == 2 && l.Count(s, "B", renamed[1]).Ready == 0
	})
	l.Checkpoint("Release Decode; restore complete 2P/3D service.", "Open B Decode readiness files.")
	s := l.Converge("B", Targets{renamed[0]: 2, renamed[1]: 3}, 0)
	renameCapacity(l, s, t)
}
func outage(l *Lab) {
	var pUnavailable any = 1
	if l.Case.Variant == "percentage" {
		pUnavailable = "100%"
	}
	l.Create(1, 2, CreateOptions{Surge: 0, Unavailable: Object{"prefill": pUnavailable, "decode": 1}, Slots: 3})
	l.Checkpoint("Roll 1P/2D without a spare slot; Prefill explicitly permits outage.", "Change A to B; hold all B readiness.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	capacity := func(s Snapshot) {
		for r, ceiling := range pair(1, 2) {
			l.Assert(l.Total(s, r, "spec") <= ceiling, "Outage case exceeded "+r+" zero-surge ceiling")
		}
	}
	l.WaitFor("A=0P/1D and B=1P/1D fit three scheduler slots", func(s Snapshot) bool {
		capacity(s)
		l.Assert(!eventReason(s, "BootstrapSurge"), "Permitted-outage rollout requested bootstrap capacity")
		return l.Count(s, "A", "prefill").Spec == 0 && l.Count(s, "A", "decode").Spec == 1 && both(func(r string) bool { c := l.Count(s, "B", r); return c.Spec == 1 && c.Scheduled == 1 })
	})
	l.Hold("Prefill outage is allowed; one Ready Decode stays protected", 8, func(s Snapshot) bool {
		capacity(s)
		l.Assert(l.Total(s, "decode", "ready") >= 1, "Prefill outage erased Decode independent Ready floor")
		l.Assert(!eventReason(s, "BootstrapSurge"), "A fourth slot was required")
		return true
	})
	l.Checkpoint("Make the replacement pair Ready; finish in three slots.", "Open B readiness; second Decode follows old Decode retirement.")
	s := l.Converge("B", pair(1, 2), 0)
	l.Assert(!eventReason(s, "BootstrapSurge"), "Rollout used extra capacity despite outage budget")
}
