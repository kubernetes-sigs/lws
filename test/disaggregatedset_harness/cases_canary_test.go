// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package harness

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanarySettledRequiresExactSplitAndFreshReadyStatus(t *testing.T) {
	s := fleet(map[string]map[string]Counts{
		"A": {"prefill": {3, 3, 3}, "decode": {6, 6, 6}},
		"B": {"prefill": {1, 1, 1}, "decode": {2, 2, 2}},
	}, 1)
	s.DS["spec"] = Object{"rolloutStrategy": Object{"canaryPercent": 25}}
	obj(s.DS["status"])["conditions"] = []any{Object{"type": "Progressing", "reason": "CanaryHeld", "status": "True", "observedGeneration": 2}}
	require.True(t, canarySettled(s, pair(3, 6), pair(1, 2), 25, false))
	require.False(t, canarySettled(s, pair(2, 4), pair(2, 4), 25, false), "total Ready alone cannot prove the requested split")
	for name, change := range map[string]func(*Snapshot){
		"unready Pod":  func(s *Snapshot) { obj(objects(at(s.Pods[0], "status", "conditions"))[0])["status"] = "False" },
		"stale status": func(s *Snapshot) { obj(s.DS["status"])["observedGeneration"] = 1 },
		"stale hold":   func(s *Snapshot) { objects(at(s.DS, "status", "conditions"))[0]["observedGeneration"] = 1 },
		"pending assignments": func(s *Snapshot) {
			obj(s.LWS[0]["metadata"])["annotations"] = Object{Prefix + "subrole-plan": "pending"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(s)
			require.NoError(t, err)
			var changed Snapshot
			require.NoError(t, json.Unmarshal(data, &changed))
			change(&changed)
			require.False(t, canarySettled(changed, pair(3, 6), pair(1, 2), 25, false))
		})
	}
}

func TestCanarySafetyChecksPhysicalSurgeAndReadyFloor(t *testing.T) {
	before := fleet(map[string]map[string]Counts{"A": {"prefill": {4, 4, 4}, "decode": {8, 8, 8}}}, 1)
	for _, counts := range []Counts{{4, 3, 4}, {6, 4, 6}} {
		l := testLab(t, &fakeExecutor{})
		guard := canarySafety(l, before, pair(4, 8))
		bad := fleet(map[string]map[string]Counts{"A": {"prefill": counts, "decode": {8, 8, 8}}}, 1)
		require.Error(t, catch(func() { guard(bad) }))
	}
	// Raw4/8 Ready is insufficient when B's last Prefill has gone but its
	// remaining Decode is still needed to cover A's incomplete replacement.
	l := testLab(t, &fakeExecutor{})
	incomplete := fleet(map[string]map[string]Counts{
		"A": {"prefill": {4, 4, 4}, "decode": {7, 7, 7}},
		"B": {"prefill": {0, 0, 0}, "decode": {1, 1, 1}},
	}, 1)
	require.ErrorContains(t, catch(func() { canarySafety(l, before, pair(4, 8))(incomplete) }), "same-revision serving floor")
	// A protected A plus a complete C is valid after the intermediate B drains.
	chain := fleet(map[string]map[string]Counts{
		"A": {"prefill": {2, 2, 2}, "decode": {4, 4, 4}},
		"C": {"prefill": {2, 2, 2}, "decode": {4, 4, 4}},
	}, 1)
	require.NoError(t, catch(func() { canarySafety(l, before, pair(4, 8))(chain) }))
	chain.DS["spec"] = Object{"rolloutStrategy": Object{"canaryPercent": 50}}
	obj(chain.DS["status"])["conditions"] = []any{Object{"type": "Progressing", "reason": "CanaryHeld", "status": "True", "observedGeneration": 2}}
	require.True(t, canaryVersionsSettled(chain, map[string]Targets{"A": pair(2, 4), "B": pair(0, 0), "C": pair(2, 4)}, 50, false))
	frozen := canaryStableIdentity(chain)
	obj(chain.Pods[0]["metadata"])["uid"] = "replacement-pod"
	require.NotEqual(t, frozen, canaryStableIdentity(chain), "A replacement must not pass just because its Ready count is unchanged")
}

func TestCanaryUpdatePatchesControlsCountsAndTemplateTogether(t *testing.T) {
	parent := RoleSpec("model", 1, "Hash", "A", 1, 0, 1, false)
	delete(obj(parent["spec"]), "replicas")
	parent["subRoles"] = childList(pair(4, 8))
	ds := Object{"spec": Object{"roles": []any{parent}}}
	executor := &fakeExecutor{run: func(c command) ([]byte, []byte, error) {
		if slices.Contains(c.args, "get") {
			return []byte(jsonText(ds)), nil, nil
		}
		return nil, nil, nil
	}}
	l := testLab(t, executor)
	percent := 25
	require.NoError(t, catch(func() {
		l.Update(Update{Version: "B", CanaryPercent: &percent, Counts: Targets{"model/prefill": 8, "model/decode": 16}})
	}))
	require.Len(t, executor.calls, 2, "one read and one atomic patch")
	var patch Object
	call := executor.calls[1]
	require.NoError(t, json.Unmarshal([]byte(call.args[slices.Index(call.args, "-p")+1]), &patch))
	require.Equal(t, 25, integer(at(patch, "spec", "rolloutStrategy", "canaryPercent")))
	role := objects(at(patch, "spec", "roles"))[0]
	require.Nil(t, at(role, "spec", "replicas"), "subroles must not acquire parent replicas")
	require.Equal(t, "B", at(role, "spec", "leaderWorkerTemplate", "workerTemplate", "metadata", "labels", VersionLabel))
	children := objects(role["subRoles"])
	require.Equal(t, 16, integer(children[0]["replicas"])) // childList sorts names.
	require.Equal(t, 8, integer(children[1]["replicas"]))
	executor.calls = nil
	require.NoError(t, catch(func() { l.Update(Update{ExternalRoles: []string{"model/prefill", "model/decode"}}) }))
	call = executor.calls[1]
	require.NoError(t, json.Unmarshal([]byte(call.args[slices.Index(call.args, "-p")+1]), &patch))
	for _, child := range objects(objects(at(patch, "spec", "roles"))[0]["subRoles"]) {
		require.Nil(t, child["replicas"])
		require.Equal(t, "External", at(child, "scaling", "mode"))
	}
}
