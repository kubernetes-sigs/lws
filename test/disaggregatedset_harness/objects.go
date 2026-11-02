// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

const (
	Prefix        = "disaggregatedset.x-k8s.io/"
	SetLabel      = Prefix + "name"
	RoleLabel     = Prefix + "role"
	RevisionLabel = Prefix + "revision"
	SubroleLabel  = Prefix + "subrole"
	VersionLabel  = "harness.lws.x-k8s.io/version"
	GroupLabel    = "leaderworkerset.sigs.k8s.io/group-key"
	WorkerIndex   = "leaderworkerset.sigs.k8s.io/worker-index"
	LWSLabel      = "leaderworkerset.sigs.k8s.io/name"
	Slot          = "harness.lws.x-k8s.io/slot"
	DSResource    = "disaggregatedsets.disaggregatedset.x-k8s.io"
	ChildCounts   = Prefix + "subrole-replicas"
	Namespace     = "v1-harness"
	SetName       = "demo"
)

type Object = map[string]any
type Snapshot struct {
	DS      Object   `json:"ds"`
	LWS     []Object `json:"lws"`
	Pods    []Object `json:"pods"`
	Events  []Object `json:"events"`
	Scalers []Object `json:"scalers"`
}
type Counts struct {
	Spec      int `json:"spec"`
	Ready     int `json:"ready"`
	Scheduled int `json:"scheduled"`
}
type Targets = map[string]int

func obj(v any) Object {
	m, _ := v.(map[string]any)
	return m
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func objects(v any) []Object {
	var out []Object
	for _, x := range arr(v) {
		if m := obj(x); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func at(o Object, path ...string) any {
	var v any = o
	for _, k := range path {
		v = obj(v)[k]
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func integer(v any, fallback ...int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, e := strconv.Atoi(n)
		if e == nil {
			return i
		}
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return 0
}

func label(o Object, key string) string { return str(at(o, "metadata", "labels", key)) }

func name(o Object) string { return str(at(o, "metadata", "name")) }

func uid(o Object) string { return str(at(o, "metadata", "uid")) }

func deleting(o Object) bool {
	return at(o, "metadata", "deletionTimestamp") != nil && at(o, "metadata", "deletionTimestamp") != ""
}

func clone(o Object) Object {
	b, _ := json.Marshal(o)
	var c Object
	_ = json.Unmarshal(b, &c)
	return c
}

func jsonText(v any) string {
	b, e := json.Marshal(v)
	abort(e)
	return string(b)
}

func keys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

func Ready(p Object) bool {
	if deleting(p) {
		return false
	}
	for _, c := range objects(at(p, "status", "conditions")) {
		if c["type"] == "Ready" && c["status"] == "True" {
			return true
		}
	}
	return false
}

func VersionOf(lws Object) string {
	return str(at(lws, "spec", "leaderWorkerTemplate", "workerTemplate", "metadata", "labels", VersionLabel))
}

func Pods(s Snapshot, version, role string) []Object {
	out := []Object{}
	for _, p := range s.Pods {
		if !deleting(p) && (version == "" || label(p, VersionLabel) == version) && (role == "" || label(p, RoleLabel) == role) {
			out = append(out, p)
		}
	}
	return out
}

func Groups(s Snapshot, version, role string) [][]Object {
	groups := map[string][]Object{}
	for _, p := range Pods(s, version, role) {
		k := label(p, GroupLabel)
		if k == "" {
			k = label(p, LWSLabel) + ":" + label(p, "leaderworkerset.sigs.k8s.io/group-index")
		}
		groups[k] = append(groups[k], p)
	}
	out := [][]Object{}
	for _, k := range keys(groups) {
		out = append(out, groups[k])
	}
	return out
}

func allPods(pods []Object, test func(Object) bool) bool {
	for _, p := range pods {
		if !test(p) {
			return false
		}
	}
	return true
}

func GroupCounts(s Snapshot, version string) map[string]Counts {
	out := map[string]Counts{}
	for _, lws := range s.LWS {
		if VersionOf(lws) != version {
			continue
		}
		role := label(lws, RoleLabel)
		c := out[role]
		c.Spec += integer(at(lws, "spec", "replicas"), 1)
		size := integer(at(lws, "spec", "leaderWorkerTemplate", "size"), 1)
		for _, g := range Groups(s, version, role) {
			if label(g[0], LWSLabel) != name(lws) {
				continue
			}
			if len(g) == size && allPods(g, Ready) {
				c.Ready++
			}
			if len(g) == size && allPods(g, func(p Object) bool { return str(at(p, "spec", "nodeName")) != "" }) {
				c.Scheduled++
			}
		}
		out[role] = c
	}
	return out
}

func SubroleCounts(s Snapshot, version string, qualified bool) map[string]Counts {
	out := map[string]Counts{}
	for _, lws := range s.LWS {
		if VersionOf(lws) != version {
			continue
		}
		parent := label(lws, RoleLabel)
		childKey := func(child string) string {
			if qualified {
				return parent + "/" + child
			}
			return child
		}
		var issued map[string]int
		_ = json.Unmarshal([]byte(str(at(lws, "metadata", "annotations", ChildCounts))), &issued)
		for child, n := range issued {
			key := childKey(child)
			c := out[key]
			c.Spec += n
			out[key] = c
		}
		size := integer(at(lws, "spec", "leaderWorkerTemplate", "size"), 1)
		for _, g := range Groups(s, version, parent) {
			if label(g[0], LWSLabel) != name(lws) {
				continue
			}
			child := label(g[0], SubroleLabel)
			if child == "" || !allPods(g, func(p Object) bool { return label(p, SubroleLabel) == child }) {
				continue
			}
			key := childKey(child)
			c := out[key]
			if len(g) == size && allPods(g, Ready) {
				c.Ready++
			}
			if len(g) == size && allPods(g, func(p Object) bool { return str(at(p, "spec", "nodeName")) != "" }) {
				c.Scheduled++
			}
			out[key] = c
		}
	}
	return out
}

func RoleSpec(name string, replicas int, identity, version string, surge, unavailable any, size int, autoReady bool) Object {
	value := "0"
	if autoReady {
		value = "1"
	}
	return Object{
		"name": name,
		"spec": Object{
			"replicas":      replicas,
			"groupIdentity": identity,
			"rolloutStrategy": Object{
				"rollingUpdateConfiguration": Object{"maxSurge": surge, "maxUnavailable": unavailable},
			},
			"leaderWorkerTemplate": Object{
				"size":          size,
				"restartPolicy": "RecreateGroupOnPodRestart",
				"workerTemplate": Object{
					"metadata": Object{
						"labels": Object{VersionLabel: version},
					},
					"spec": Object{
						"terminationGracePeriodSeconds": 1,
						"containers": []any{
							Object{
								"name":            "demo",
								"image":           "busybox:1.36",
								"imagePullPolicy": "IfNotPresent",
								"env": []any{
									Object{"name": "AUTO_READY", "value": value},
								},
								"command": []any{
									"sh",
									"-c",
									`trap "exit 0" TERM INT; [ "$AUTO_READY" = 1 ] && touch /tmp/ready; while :; do if [ -f /tmp/crash ]; then echo HARNESS_CRASH; exit 42; fi; sleep 1; done`,
								},
								"readinessProbe": Object{
									"exec": Object{
										"command": []any{"test", "-f", "/tmp/ready"},
									},
									"periodSeconds":    1,
									"timeoutSeconds":   2,
									"failureThreshold": 1,
								},
								"resources": Object{
									"requests": Object{"cpu": "5m", "memory": "8Mi", Slot: "1"},
									"limits":   Object{Slot: "1"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func uidSet(objects []Object) map[string]bool {
	m := map[string]bool{}
	for _, o := range objects {
		if !deleting(o) {
			m[uid(o)] = true
		}
	}
	return m
}

func allUIDs(objects []Object) map[string]bool {
	out := map[string]bool{}
	for _, o := range objects {
		out[uid(o)] = true
	}
	return out
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sameSet(a, b map[string]bool) bool { return len(a) == len(b) && subset(a, b) }

func revisions(s Snapshot, version string) map[string]bool {
	m := map[string]bool{}
	for _, lws := range s.LWS {
		if version == "" || VersionOf(lws) == version {
			revision := label(lws, RevisionLabel)
			if revision == "" {
				abort(fmt.Errorf("LWS %s is missing its revision label", name(lws)))
			}
			m[revision] = true
		}
	}
	return m
}

func podRevisions(s Snapshot, version string) map[string]bool {
	out := map[string]bool{}
	for _, pod := range Pods(s, version, "") {
		revision := label(pod, RevisionLabel)
		if revision == "" {
			abort(fmt.Errorf("live Pod %s is missing its revision label", name(pod)))
		}
		out[revision] = true
	}
	return out
}

func eventReason(s Snapshot, reason string) bool {
	for _, e := range s.Events {
		if e["reason"] == reason {
			return true
		}
	}
	return false
}

func pair(p, d int) Targets { return Targets{"prefill": p, "decode": d} }

func totalTargets(t Targets) int {
	n := 0
	for _, v := range t {
		n += v
	}
	return n
}

func percentBounds(t Targets) (Targets, Targets) {
	floor, ceil := Targets{}, Targets{}
	for k, n := range t {
		floor[k] = n - n/10
		ceil[k] = n + (n+9)/10
	}
	return floor, ceil
}

func annotatedCounts(lws Object) Targets {
	var t Targets
	_ = json.Unmarshal([]byte(str(at(lws, "metadata", "annotations", ChildCounts))), &t)
	return t
}

func sameTargets(a, b Targets) bool { return maps.Equal(a, b) }

func describe(t Targets) string {
	var cells []string
	for _, k := range keys(t) {
		cells = append(cells, fmt.Sprintf("%s=%d", k, t[k]))
	}
	return strings.Join(cells, ", ")
}
