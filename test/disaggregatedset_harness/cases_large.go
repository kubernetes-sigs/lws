// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"
)

var initial = pair(37, 19)
var grown = pair(55, 29)
var shrunk = pair(28, 14)
var largeSurge = pair(12, 6)
var children = Targets{"prefill/fast": 25, "prefill/bulk": 12, "decode/fast": 13, "decode/bulk": 6}
var reassigned = Targets{"prefill/fast": 21, "prefill/bulk": 16, "decode/fast": 10, "decode/bulk": 9}
var childSurge = Targets{"prefill/fast": 6, "prefill/bulk": 6, "decode/fast": 3, "decode/bulk": 3}
var versions = []string{"A", "B", "C"}

type firstGroupBootstrap struct {
	eligible, used map[string]bool
	previous       map[string]string
}

func bootstrapEvents(s Snapshot) map[string]string {
	events := map[string]string{}
	owner := uid(s.DS)
	for _, e := range s.Events {
		if owner != "" && e["reason"] == "BootstrapSurge" && at(e, "involvedObject", "uid") == owner {
			message := str(e["message"])
			events[fmt.Sprintf("%s/%d/%s", uid(e), integer(e["count"], 1), message)] = message
		}
	}
	return events
}

func (b *firstGroupBootstrap) start(l *Lab, s Snapshot, ceilings Targets) {
	b.eligible, b.used, b.previous = map[string]bool{}, map[string]bool{}, bootstrapEvents(s)
	for r, n := range ceilings {
		if l.Total(s, r, "spec") == n && l.Count(s, "C", r).Spec == 0 {
			b.eligible[r] = true
		}
	}
}

func (b *firstGroupBootstrap) allows(l *Lab, s Snapshot, r string, ceiling int) bool {
	total := l.Total(s, r, "spec")
	if total <= ceiling {
		if b.used[r] {
			delete(b.eligible, r)
		}
		return false
	}
	if !b.eligible[r] || total != ceiling+1 || l.Count(s, "C", r).Spec != 1 {
		return false
	}
	b.used[r] = true
	return true
}

func (b *firstGroupBootstrap) confirmed(s Snapshot) bool {
	named := map[string]bool{}
	for key, message := range bootstrapEvents(s) {
		if _, exists := b.previous[key]; exists {
			continue
		}
		_, after, found := strings.Cut(message, "roles [")
		if found {
			names, _, _ := strings.Cut(after, "]")
			for _, r := range strings.Fields(names) {
				named[r] = true
			}
		}
	}
	return subset(b.used, named)
}

func bounds(l *Lab, s Snapshot, floors, ceilings Targets, b *firstGroupBootstrap) bool {
	for r, floor := range floors {
		l.Assert(l.Total(s, r, "ready") >= floor, fmt.Sprintf("%s: Ready capacity fell below protected floor %d", r, floor))
		borrowed := false
		if b != nil {
			borrowed = b.allows(l, s, r, ceilings[r])
		}
		l.Assert(l.Total(s, r, "spec") <= ceilings[r] || borrowed, fmt.Sprintf("%s: requested groups exceeded ceiling %d", r, ceilings[r]))
	}
	return true
}

func largeRelease(l *Lab, s Snapshot, version string, targets Targets) {
	for _, role := range keys(targets) {
		parent, child, _ := strings.Cut(role, "/")
		pods := []Object{}
		for _, p := range Pods(s, version, parent) {
			if child == "" || label(p, SubroleLabel) == child {
				pods = append(pods, p)
			}
		}
		sort.SliceStable(pods, func(i, j int) bool {
			a, b := Ready(pods[i]), Ready(pods[j])
			if a != b {
				return a
			}
			return name(pods[i]) < name(pods[j])
		})
		n := min(len(pods), targets[role])
		for _, p := range pods[:n] {
			l.releasePod(p)
		}
	}
}

func largeProgress(l *Lab, version string, targets, floors, ceilings Targets) Snapshot {
	return l.Wait(version+" reaches Ready "+describe(targets)+"; next batch loading", func(s Snapshot) bool {
		bounds(l, s, floors, ceilings, nil)
		for r, n := range targets {
			c := l.Count(s, version, r)
			if c.Ready != n || c.Scheduled <= n || c.Scheduled != c.Spec {
				return false
			}
		}
		return true
	}, 480*time.Second, 3, func(s Snapshot) { largeRelease(l, s, version, targets) })
}

func sharedLayout(s Snapshot, version string, desired Targets) bool {
	parents := []Object{}
	for _, o := range s.LWS {
		if VersionOf(o) == version && !deleting(o) {
			parents = append(parents, o)
		}
	}
	if len(parents) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, lws := range parents {
		parent := label(lws, RoleLabel)
		if _, ok := initial[parent]; !ok || seen[parent] {
			return false
		}
		seen[parent] = true
		wanted := childTargets(parent, desired)
		if str(at(lws, "metadata", "annotations", Prefix+"subrole-plan")) != "" || !sameTargets(annotatedCounts(lws), wanted) || integer(at(lws, "spec", "replicas")) != totalTargets(wanted) {
			return false
		}
		groups := Groups(s, version, parent)
		if len(groups) != totalTargets(wanted) {
			return false
		}
		for child, n := range wanted {
			found := 0
			for _, g := range groups {
				if allPods(g, func(p Object) bool { return label(p, SubroleLabel) == child }) {
					if len(g) != 1 || !Ready(g[0]) {
						return false
					}
					found++
				}
			}
			if found != n {
				return false
			}
		}
	}
	return true
}

func largeFinish(l *Lab, version string, desired, floors, ceilings Targets, protected Predicate, b *firstGroupBootstrap) {
	l.Wait("Only "+version+" remains: "+describe(desired)+", all Ready and old Pods gone", func(s Snapshot) bool {
		bounds(l, s, floors, ceilings, b)
		if protected != nil {
			protected(s)
		}
		return largeConverged(s, version, desired, l.virtual)
	}, 600*time.Second, 3, func(s Snapshot) {
		targets := Targets{}
		for r, n := range desired {
			c := l.Count(s, version, r)
			targets[r] = min(max(n, c.Spec), c.Ready+max(1, n/8))
		}
		largeRelease(l, s, version, targets)
	})
}

func largeConverged(s Snapshot, version string, desired Targets, virtual bool) bool {
	// Every large case has exactly one Pod per requested group. Extra target
	// roles/physical workloads must not make a nominally converged fleet pass.
	return len(s.Pods) == totalTargets(desired) && converged(s, version, desired) && (!virtual || sharedLayout(s, version, desired))
}

func largeStart(l *Lab, virtual, percentage bool) (Targets, Targets) {
	l.virtual = virtual
	detail := "maxUnavailable=0; retain every existing Ready group."
	if percentage {
		detail = "maxSurge=10%, maxUnavailable=10%; initial floors 34/18, ceilings 41/21."
	}
	l.Checkpoint("Create A: 37 Prefill / 19 Decode Pods; one Pod per group.", detail)
	if !virtual {
		c := CreateOptions{Surge: largeSurge, Slots: 128}
		surge := largeSurge
		if percentage {
			c.Surge, c.Unavailable = "10%", "10%"
			_, ceil := percentBounds(initial)
			surge = Targets{}
			for r, n := range initial {
				surge[r] = ceil[r] - n
			}
		}
		l.Create(37, 19, c)
		return initial, surge
	}
	l.SetSlots(128)
	parents := []any{}
	for _, parent := range roles {
		surge := 6
		if parent == "decode" {
			surge = 3
		}
		role := RoleSpec(parent, 1, l.Case.Identity, "A", surge, 0, 1, true)
		delete(obj(role["spec"]), "replicas")
		role["subRoles"] = childList(childTargets(parent, children))
		parents = append(parents, role)
	}
	l.createSet(parents)
	zero := Targets{}
	for r := range children {
		zero[r] = 0
	}
	largeFinish(l, "A", children, zero, children, nil, nil)
	before := uidSet(Pods(l.Snapshot(), "", ""))
	l.Checkpoint("Reassign children without changing 37 Prefill / 19 Decode physical Pods.", "Prefill fast/bulk 25/12 → 21/16; Decode fast/bulk 13/6 → 10/9.")
	ds := l.Snapshot().DS
	rs := objects(at(ds, "spec", "roles"))
	for _, r := range rs {
		r["subRoles"] = childList(childTargets(str(r["name"]), reassigned))
	}
	l.Patch(Object{
		"spec": Object{"roles": rs},
	})
	s := l.WaitFor("Independent child counts changed; one shared LWS per physical role", func(s Snapshot) bool { return sharedLayout(s, "A", reassigned) })
	l.Assert(sameSet(uidSet(Pods(s, "", "")), before), "Relabelling children replaced physical Pod")
	return reassigned, childSurge
}

func midpointVisible(l *Lab, s Snapshot, desired Targets) bool {
	for r, n := range desired {
		a, b := l.Count(s, "A", r).Ready, l.Count(s, "B", r).Ready
		if a < max(1, n/3) || b < n/3 || b > 2*n/3 {
			return false
		}
	}
	return true
}

func largeMixed(l *Lab, virtual, percentage bool, bootstrap *firstGroupBootstrap) (string, Snapshot, Targets, Targets) {
	desired, surge := largeStart(l, virtual, percentage)
	floors := desired
	if percentage {
		floors, _ = percentBounds(desired)
	}
	ceilings, progress := Targets{}, Targets{}
	for r, n := range desired {
		ceilings[r] = n + surge[r]
		progress[r] = max(1, n/2)
	}
	l.Checkpoint("Start B; keep readiness closed until each batch is released.", "Change physical templates A → B; preserve rollout budgets.")
	l.Update(Update{Version: "B", Ready: boolPtr(false)})
	l.Checkpoint("Bring B halfway through startup; A serves and next B batch loads.", "Release exactly "+describe(progress)+" through readiness files.")
	largeProgress(l, "B", progress, floors, ceilings)
	midpoint := func(s Snapshot) bool { return bounds(l, s, floors, ceilings, nil) && midpointVisible(l, s, desired) }
	state := l.Hold("Mid-rollout: substantial A and B both still Ready", 8, midpoint)
	target := "B"
	if l.Case.Variant == "abc" {
		oldReady := map[string]Targets{}
		for _, v := range []string{"A", "B"} {
			oldReady[v] = Targets{}
			for r := range desired {
				oldReady[v][r] = l.Count(state, v, r).Ready
			}
		}
		l.Checkpoint("Interrupt unfinished A → B with C.", "Ready at trigger: "+jsonText(oldReady)+". Change B → C; hold C readiness.")
		if bootstrap != nil {
			bootstrap.start(l, state, ceilings)
		}
		l.Update(Update{Version: "C", Ready: boolPtr(false)})
		coexist := func(s Snapshot) bool {
			bounds(l, s, floors, ceilings, bootstrap)
			if !midpointVisible(l, s, desired) {
				return false
			}
			for r := range desired {
				c := l.Count(s, "C", r)
				if c.Ready != 0 || c.Scheduled <= 0 || c.Scheduled != c.Spec {
					return false
				}
			}
			return bootstrap == nil || bootstrap.confirmed(s)
		}
		l.Wait("A+B still serve; every C Pod scheduled and none Ready", coexist, 480*time.Second, 3, nil)
		state = l.Hold("Observe interruption before C advances: A+B Ready, C loading", 8, coexist)
		target = "C"
	}
	return target, state, desired, ceilings
}

func largeOrdinary(l *Lab) {
	target, _, desired, ceilings := largeMixed(l, false, false, nil)
	l.Checkpoint("Finish "+target+" in successive readiness batches.", "Keep 37 Ready Prefill / 19 Ready Decode throughout.")
	largeFinish(l, target, desired, desired, ceilings, nil, nil)
}

func largeSubroles(l *Lab) {
	var bootstrap *firstGroupBootstrap
	if l.Case.Variant == "abc" {
		bootstrap = &firstGroupBootstrap{}
	}
	target, _, desired, ceilings := largeMixed(l, true, false, bootstrap)
	l.Checkpoint("Finish "+target+"; each child keeps its availability budget.", "Verify final children and two shared physical LWSs.")
	largeFinish(l, target, desired, desired, ceilings, nil, bootstrap)
}

func oldSpecs(l *Lab, s Snapshot, target string) map[string]Targets {
	out := map[string]Targets{}
	for _, v := range versions {
		if v == target {
			continue
		}
		out[v] = Targets{}
		for r := range initial {
			out[v][r] = l.Count(s, v, r).Spec
		}
	}
	return out
}

func noOldRegrowth(l *Lab, s Snapshot, old map[string]Targets) {
	for v, counts := range old {
		for r, n := range counts {
			now := l.Count(s, v, r).Spec
			l.Assert(now <= n, "Demand change regrew an old revision")
			counts[r] = now
		}
	}
}

func largeScaleUp(l *Lab) {
	target, before, _, _ := largeMixed(l, false, false, nil)
	old := oldSpecs(l, before, target)
	started := uidSet(Pods(before, target, ""))
	floors, ceilings := maps.Clone(initial), Targets{}
	for r, n := range grown {
		ceilings[r] = n + largeSurge[r]
	}
	protected := func(s Snapshot) bool {
		l.Assert(subset(started, uidSet(Pods(s, target, ""))), "Scale-up discarded already scheduled target Pod")
		noOldRegrowth(l, s, old)
		bounds(l, s, floors, ceilings, nil)
		for r := range floors {
			floors[r] = max(floors[r], min(grown[r], l.Total(s, r, "ready")))
		}
		return true
	}
	l.Checkpoint("Increase demand: Prefill 37 → 55; Decode 19 → 29. Grow only target.", "Patch counts; retain old Ready capacity and loading target Pods.")
	l.Update(Update{Counts: grown})
	l.Hold("Higher desired does not permit destroying Ready or loading work", 10, protected)
	l.Checkpoint("Release "+target+"; newly Ready capacity raises protected floor.", "Never open or regrow an old revision.")
	largeFinish(l, target, grown, initial, ceilings, protected, nil)
}

func scaleDownRetainsOld(l *Lab, s Snapshot, target string, before Targets) bool {
	for r := range initial {
		oldReady, oldSpec := 0, 0
		for _, v := range versions {
			if v != target {
				c := l.Count(s, v, r)
				oldReady += c.Ready
				oldSpec += c.Spec
			}
		}
		if oldReady <= 0 || oldSpec >= before[r] {
			return false
		}
	}
	return true
}

func largeScaleDown(l *Lab) {
	target, before, _, ceilings := largeMixed(l, false, true, nil)
	floors, newCeilings := percentBounds(shrunk)
	issued := Targets{}
	for r := range initial {
		c := l.Count(before, target, r)
		issued[r] = c.Spec
		l.Assert(c.Spec < shrunk[r] && c.Ready < floors[r], "Scale-down must start mid-rollout: old Ready work still needed")
	}
	started := uidSet(Pods(before, target, ""))
	old := oldSpecs(l, before, target)
	oldBefore := Targets{}
	for _, counts := range old {
		for r, n := range counts {
			oldBefore[r] += n
		}
	}
	protected := func(s Snapshot) bool {
		bounds(l, s, floors, ceilings, nil)
		l.Assert(subset(started, uidSet(Pods(s, target, ""))), "Scale-down discarded needed scheduled target work")
		noOldRegrowth(l, s, old)
		for r := range initial {
			n := l.Count(s, target, r).Spec
			l.Assert(issued[r] <= n && n <= shrunk[r], "Scale-down cancelled needed target work or grew excess")
		}
		return true
	}
	l.Checkpoint("Reduce demand gently: Prefill 37 → 28; Decode 19 → 14 mid-rollout.", "Keep 10% budgets. Ready floors "+describe(floors)+"; Immediate ceilings "+describe(newCeilings)+"; issued "+describe(issued)+".")
	l.Update(Update{Counts: shrunk})
	visible := func(s Snapshot) bool { return protected(s) && scaleDownRetainsOld(l, s, target, oldBefore) }
	l.Wait(l.Case.Policy+": old requests decrease, old Prefill and Decode still serve", visible, 480*time.Second, 0, nil)
	l.Hold("Demand is lower, not full replacement: old Ready groups remain", 12, visible)
	l.Checkpoint("Release "+target+" in small batches; progressively replace old work.", "Finish at 28 Prefill / 14 Decode with unchanged budgets.")
	largeFinish(l, target, shrunk, floors, ceilings, protected, nil)
}
