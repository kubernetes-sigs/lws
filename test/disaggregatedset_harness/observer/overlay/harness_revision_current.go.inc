//go:build harness && !harness_legacy_revision

// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package disaggregatedset

import (
	"fmt"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	"sigs.k8s.io/lws/pkg/computedomain"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

// Match resolveRevision without writing its migration marker. Slice enrollment
// participates in the target hash; enrolled legacy sets migrate to that hash.
func harnessTargetRevision(ds *disaggv1.DisaggregatedSet, hasExistingLWS bool) (string, error) {
	switch ds.Annotations[disaggv1.RevisionHashVersionAnnotationKey] {
	case disaggv1.RevisionHashVersion:
		return disaggutils.ComputeDisaggregatedSetRevision(ds), nil
	case "":
		if hasExistingLWS && !computedomain.DisaggregatedSetUsesAutoComputeDomain(ds) {
			return disaggutils.ComputeRevisionV1(ds.Spec.Roles), nil
		}
		return disaggutils.ComputeDisaggregatedSetRevision(ds), nil
	default:
		return "", fmt.Errorf("unsupported revision hash version")
	}
}
