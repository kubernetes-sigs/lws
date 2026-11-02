// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Only ownership/revision metadata is seeded. Pod readiness, native scale
// acknowledgements and scheduler Unschedulable timestamps are never forged.
func oldASurvives(l *Lab, s Snapshot) bool {
	for r, n := range pair(1, 3) {
		c := l.Count(s, "A", r)
		if c.Spec != n || c.Ready != n {
			return false
		}
	}
	return true
}
func capacityRejection(s Snapshot) bool {
	for _, p := range Pods(s, "C", "prefill") {
		for _, c := range objects(at(p, "status", "conditions")) {
			if c["type"] == "PodScheduled" && c["status"] == "False" && c["reason"] == "Unschedulable" && strings.Contains(str(c["message"]), "Insufficient "+Slot) {
				return true
			}
		}
	}
	return false
}

type webhookRestore struct {
	Resource string   `json:"resource"`
	Name     string   `json:"name"`
	Patch    []Object `json:"patch"`
}

func pausedForAdoption(l *Lab, adopt func()) {
	manager := l.JSON("-n", "lws-system", "get", "deployment", "lws-controller-manager", "-o", "json")
	replicas := integer(at(manager, "spec", "replicas"), 1)
	restores := []webhookRestore{}
	// Restoration is a true unwind handler: even an unexpected programming
	// panic cannot bypass the exact webhook/replica restoration attempts.
	defer func() {
		originalPanic := recover()
		originalCtx := l.ctx
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 150*time.Second)
		defer cancel()
		l.ctx = cleanup
		defer func() { l.ctx = originalCtx }()
		var restoreErr error
		for i := len(restores) - 1; i >= 0; i-- {
			r := restores[i]
			restoreErr = errors.Join(restoreErr, cleanupAttempt(func() { l.K("patch", r.Resource, r.Name, "--type=json", "-p", jsonText(r.Patch)) }))
		}
		restoreErr = errors.Join(restoreErr, cleanupAttempt(func() {
			l.K("-n", "lws-system", "scale", "deployment/lws-controller-manager", fmt.Sprintf("--replicas=%d", replicas))
			l.K("-n", "lws-system", "rollout", "status", "deployment/lws-controller-manager", "--timeout=120s")
		}))
		if originalPanic != nil {
			if f, ok := originalPanic.(failure); ok {
				abort(errors.Join(f.err, restoreErr))
			}
			if restoreErr != nil {
				fmt.Fprintln(l.Options.Output, "Emergency restoration failed:", restoreErr)
			}
			panic(originalPanic)
		}
		abort(restoreErr)
	}()
	err := catch(func() {
		for _, resource := range []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"} {
			configurations := objects(l.JSON("get", resource, "-o", "json")["items"])
			for _, configuration := range configurations {
				patch, undo := []Object{}, []Object{}
				for i, webhook := range objects(configuration["webhooks"]) {
					if !oneOf(str(webhook["name"]), "vdisaggregatedset.kb.io", "vleaderworkerset.kb.io", "mleaderworkerset.kb.io") || at(webhook, "clientConfig", "service", "namespace") != "lws-system" {
						continue
					}
					path := fmt.Sprintf("/webhooks/%d/namespaceSelector", i)
					selector := clone(obj(webhook["namespaceSelector"]))
					if selector == nil {
						selector = Object{}
					}
					selector["matchExpressions"] = append(arr(selector["matchExpressions"]), Object{"key": "kubernetes.io/metadata.name", "operator": "NotIn", "values": []any{Namespace}})
					patch = append(patch, Object{"op": "add", "path": path, "value": selector})
					if original, ok := webhook["namespaceSelector"]; ok {
						undo = append(undo, Object{"op": "add", "path": path, "value": original})
					} else {
						undo = append(undo, Object{"op": "remove", "path": path})
					}
				}
				if len(patch) > 0 {
					restores = append(restores, webhookRestore{resource, name(configuration), undo})
					abort(writeJSON(filepath.Join(l.Directory, "emergency-webhook-restoration.json"), restores))
					l.K("patch", resource, name(configuration), "--type=json", "-p", jsonText(patch))
				}
			}
		}
		n := 0
		for _, r := range restores {
			n += len(r.Patch)
		}
		l.Assert(n == 3, "Exactly three required admission hooks scoped to this namespace")
		l.K("-n", "lws-system", "scale", "deployment/lws-controller-manager", "--replicas=0")
		deadline := l.clock.Now().Add(90 * time.Second)
		for len(objects(l.JSON("-n", "lws-system", "get", "pods", "-l", "control-plane=controller-manager", "-o", "json")["items"])) > 0 {
			l.Assert(l.clock.Now().Before(deadline), "Local controller did not stop before adoption")
			l.Sleep(time.Second)
		}
		adopt()
	})
	abort(err)
}
func cleanupAttempt(f func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if failed, ok := p.(failure); ok {
				err = failed.err
			} else {
				err = fmt.Errorf("unexpected cleanup panic: %v", p)
			}
		}
	}()
	f()
	return nil
}
func seedHistory(l *Lab) {
	l.SetSlots(7)
	type seed struct{ Name, Version, Role string }
	seeds := []seed{}
	for i, counts := range []Targets{pair(1, 3), pair(1, 1), pair(0, 1)} {
		version := versions[i]
		for _, role := range roles {
			initialCount := 1
			if role == "decode" {
				initialCount = 4
			}
			spec := obj(RoleSpec(role, counts[role], l.Case.Identity, version, 1, 0, 1, false)["spec"])
			labels := obj(at(spec, "leaderWorkerTemplate", "workerTemplate", "metadata", "labels"))
			labels[RoleLabel], labels[SetLabel], labels[Prefix+"slice"] = role, SetName, "0"
			name := "emergency-" + strings.ToLower(version) + "-" + role
			l.Apply(Object{"apiVersion": "leaderworkerset.x-k8s.io/v1", "kind": "LeaderWorkerSet", "metadata": Object{"name": name, "labels": Object{RoleLabel: role}, "annotations": Object{Prefix + "initial-replicas": fmt.Sprint(initialCount)}}, "spec": spec})
			seeds = append(seeds, seed{name, version, role})
		}
	}
	l.Wait("Seven real Ready groups occupy seven scheduler slots", func(s Snapshot) bool {
		return len(Pods(s, "", "")) == 7 && allPods(Pods(s, "", ""), Ready) && oldASurvives(l, s) && both(func(r string) bool { return l.Count(s, "B", r).Ready == 1 }) && l.Count(s, "C", "decode").Ready == 1
	}, 240*time.Second, 0, func(s Snapshot) { l.Release("", "", -1, &s) })
	abort(writeJSON(filepath.Join(l.Directory, "emergency-seeded.json"), l.Snapshot()))
	l.Checkpoint("Attach interrupted history to controller.", "Pause local manager, create parent, attach six LWSs, restore admission, resume.")
	pausedForAdoption(l, func() {
		rs := []any{RoleSpec("prefill", 1, l.Case.Identity, "C", 1, 0, 1, false), RoleSpec("decode", 4, l.Case.Identity, "C", 1, 0, 1, false)}
		l.Apply(Object{"apiVersion": "disaggregatedset.x-k8s.io/v1", "kind": "DisaggregatedSet", "metadata": Object{"name": SetName, "annotations": Object{Prefix + "revision-hash-version": "v2"}}, "spec": Object{"roles": rs, "scalingPolicy": Object{"scaleDownDuringRollout": l.Case.Policy}}})
		ds := l.JSON("-n", Namespace, "get", DSResource, SetName, "-o", "json")
		ctx, cancel := context.WithTimeout(l.ctx, 60*time.Second)
		defer cancel()
		out, stderr, err := l.Options.Executor.Run(ctx, l.Options.Observer, []string{"--kubeconfig", l.Options.Kubeconfig, "--context", l.Options.Context, "--namespace", Namespace, "--name", SetName}, nil)
		if err != nil {
			abort(fmt.Errorf("revision observer: %w: %s", err, stderr))
		}
		var observed Object
		abort(json.Unmarshal(out, &observed))
		target := str(observed["targetRevision"])
		l.Assert(target != "", "Read-only observer returned persisted target revision")
		owner := Object{"apiVersion": ds["apiVersion"], "kind": ds["kind"], "name": SetName, "uid": uid(ds), "controller": true}
		for _, s := range seeds {
			revision := "seed-" + strings.ToLower(s.Version)
			if s.Version == "C" {
				revision = target
			}
			l.K("-n", Namespace, "patch", "lws", s.Name, "--type=merge", "-p", jsonText(Object{"metadata": Object{"labels": Object{SetLabel: SetName, RoleLabel: s.Role, Prefix + "slice": "0", RevisionLabel: revision}, "ownerReferences": []any{owner}}}))
		}
	})
	l.StartObserver()
}
func emergency(l *Lab) {
	l.Note("Emergency recovery: desired 1P/4D; maxSurge=1 / maxUnavailable=0. Seven equal slots.")
	l.Note("Seeded A=1P/3D + B=1P/1D + C=0P/1D. Normal floors 1P/4D; emergency Decode floor 3.")
	seedHistory(l)
	s := l.Wait("One bootstrap Prefill genuinely Unschedulable: no eighth slot", func(s Snapshot) bool {
		return eventReason(s, "BootstrapSurge") && capacityRejection(s) && oldASurvives(l, s) && both(func(r string) bool { return l.Count(s, "B", r).Spec == 1 })
	}, 90*time.Second, 0, nil)
	originalA := uidSet(Pods(s, "A", ""))
	abort(writeJSON(filepath.Join(l.Directory, "emergency-unschedulable.json"), s))
	l.Note("Real scheduler clock runs at least 60 seconds; its age is not forged or shortened.")
	l.Note("Automatic bounded fallback may retire B, keeping A complete.")
	s = l.Wait("Fallback retires B, schedules C Prefill; A retains 1P/3D Ready", func(s Snapshot) bool {
		return eventReason(s, "AvailabilityFallback") && oldASurvives(l, s) && both(func(r string) bool { return l.Count(s, "B", r).Spec == 0 }) && l.Count(s, "C", "prefill").Scheduled == 1 && l.Count(s, "C", "prefill").Ready == 0
	}, 150*time.Second, 0, nil)
	abort(writeJSON(filepath.Join(l.Directory, "emergency-fallback.json"), s))
	l.Hold("Loading Prefill does not stack bootstrap replicas or cascade old drains", 15, func(s Snapshot) bool {
		return oldASurvives(l, s) && sameSet(originalA, uidSet(Pods(s, "A", ""))) && l.Count(s, "C", "prefill").Spec == 1 && l.Count(s, "C", "prefill").Ready == 0
	})
	l.Checkpoint("Replacement has a slot; let it become Ready.", "Open C readiness probes; ordinary rollout resumes.")
	l.Converge("C", pair(1, 4), 0)
	s = l.Snapshot()
	l.Assert(eventReason(s, "BootstrapSurge") && eventReason(s, "AvailabilityFallback"), "Both emergency mechanisms observed as real controller Events")
}
