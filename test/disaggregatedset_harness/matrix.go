// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

// Package harness implements the v1 live regression scenarios without using
// the ambient Kubernetes context. The shell launcher
// owns cluster provisioning; this package owns scenarios and portable evidence.
package harness

import (
	"fmt"
	"slices"
	"strings"
)

type Selection struct {
	Names                                   []string
	Identity, Policy, Variant, RolloutChain string
	IncludeLarge                            bool
}
type Case struct{ Name, Identity, Policy, Variant string }

func (c Case) DisplayName() string {
	if c.Variant != "default" {
		return c.Name + "-" + c.Variant
	}
	return c.Name
}

func (c Case) ID() string { return c.DisplayName() + "--" + c.Identity + "--" + c.Policy }

func (c Case) DirectoryName() string {
	return c.DisplayName() + "-" + strings.ToLower(c.Identity) + "-" + strings.ToLower(c.Policy)
}

type Definition struct {
	Name, Description string
	Large             bool
}

var Definitions = []Definition{
	{"ordinary", "asymmetric rollout + steady scaling; 2P/4D, surge 1, unavailable 0", false},
	{"scale-up", "raise demand during unequal rollout progress; 4→6", false},
	{"scale-down", "lower demand; preserve needed loading groups; 4→2, both policies", false},
	{"autoscaler-handoff", "Static→External without draining; 3P/2D, then /scale P 3→4→2", false},
	{"rename", "renamed roles share released slots; 2P/2D, surge 0, unavailable 1", false},
	{"rename-outage", "full-downtime rename; 2P/3D, surge 0, unavailable 100%", false},
	{"restart-rollback", "interrupt A→B with C, restart manager, roll back B", false},
	{"subroles", "relabel, shrink, restart, grow, roll one physical LWS", false},
	{"native-restart", "worker and leader crashes recreate only their affected group", false},
	{"outage", "1P/2D in three slots; integer/percentage permitted outage", false},
	{"emergency", "bootstrap + real ≥60s Unschedulable fallback; 1P/4D", false},
	{"large-rename", "optional 9P/4D→8P/3D rename in thirteen slots", false},
	{"large-scale-up", "37P/19D→55P/29D while rolling out; Immediate", true},
	{"large-scale-down", "37P/19D→28P/14D; 10% budgets; both policies", true},
	{"large-ordinary", "37P/19D rollout; surge 12/6, unavailable 0", true},
	{"large-subroles", "37P/19D; four children sharing two physical LWSs", true},
}

func Matrix(s Selection) ([]Case, error) {
	if s.Identity == "" {
		s.Identity = "both"
	}
	if s.RolloutChain == "" {
		s.RolloutChain = "both"
	}
	if !oneOf(s.Identity, "ordinal", "hash", "both") || !oneOf(s.Policy, "", "Deferred", "Immediate") || !oneOf(s.Variant, "", "integer", "percentage") || !oneOf(s.RolloutChain, "ab", "abc", "both") {
		return nil, fmt.Errorf("invalid identity, policy, variant, or rollout-chain selector")
	}
	names := s.Names
	if len(names) == 0 {
		names = []string{"all"}
	}
	known := map[string]Definition{}
	for _, d := range Definitions {
		known[d.Name] = d
	}
	for _, n := range names {
		if _, ok := known[n]; !ok && n != "all" && n != "large-all" {
			return nil, fmt.Errorf("unknown case %q", n)
		}
	}
	var expanded []string
	if slices.Contains(names, "all") {
		for _, d := range Definitions {
			if !d.Large && d.Name != "large-rename" {
				expanded = append(expanded, d.Name)
			}
		}
	}
	for _, n := range names {
		if n != "all" && n != "large-all" {
			expanded = append(expanded, n)
		}
	}
	if slices.Contains(names, "large-all") {
		for _, d := range Definitions {
			if d.Large {
				expanded = append(expanded, d.Name)
			}
		}
	}
	if s.IncludeLarge {
		expanded = append(expanded, "large-rename")
	}
	identities := []string{"Ordinal", "Hash"}
	if s.Identity != "both" {
		identities = []string{strings.ToUpper(s.Identity[:1]) + s.Identity[1:]}
	}
	seen := map[string]bool{}
	var out []Case
	for _, n := range expanded {
		if seen[n] {
			continue
		}
		seen[n] = true
		policies := []string{"Deferred"}
		if n == "scale-down" || n == "large-scale-down" {
			policies = []string{"Deferred", "Immediate"}
			if s.Policy != "" {
				policies = []string{s.Policy}
			}
		}
		if n == "large-scale-up" {
			policies = []string{"Immediate"}
		}
		variants := []string{"default"}
		if n == "outage" {
			variants = []string{"integer", "percentage"}
			if s.Variant != "" {
				variants = []string{s.Variant}
			}
		}
		if known[n].Large {
			variants = []string{"ab", "abc"}
			if s.RolloutChain != "both" {
				variants = []string{s.RolloutChain}
			}
		}
		for _, i := range identities {
			for _, p := range policies {
				for _, v := range variants {
					out = append(out, Case{n, i, p, v})
				}
			}
		}
	}
	return out, nil
}

func oneOf(s string, values ...string) bool { return slices.Contains(values, s) }
