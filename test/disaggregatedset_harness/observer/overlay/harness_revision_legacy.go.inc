//go:build harness && harness_legacy_revision

// Copyright 2026 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0
package disaggregatedset

import (
	"fmt"

	disaggv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	disaggutils "sigs.k8s.io/lws/pkg/utils/disaggregatedset"
)

// Selected explicitly for source snapshots predating whole-set hashing.
func harnessTargetRevision(ds *disaggv1.DisaggregatedSet, hasExistingLWS bool) (string, error) {
	switch ds.Annotations[disaggv1.RevisionHashVersionAnnotationKey] {
	case disaggv1.RevisionHashVersion:
		return disaggutils.ComputeRevision(ds.Spec.Roles), nil
	case "":
		if hasExistingLWS {
			return disaggutils.ComputeRevisionV1(ds.Spec.Roles), nil
		}
		return disaggutils.ComputeRevision(ds.Spec.Roles), nil
	default:
		return "", fmt.Errorf("unsupported revision hash version")
	}
}
