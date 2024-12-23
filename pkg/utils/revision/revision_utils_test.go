/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package revision

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/history"
	testutils "sigs.k8s.io/lws/test/testutils"
)

func TestApplyRevision(t *testing.T) {

	lws := testutils.BuildLeaderWorkerSet("default").Obj()
	revision, err := NewRevision(lws, 1, LeaderWorkerTemplateHash(lws))
	if err != nil {
		t.Fatal(err)
	}
	currentLws := lws.DeepCopy()

	lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Name = "update-name"
	subdomainPolicy := leaderworkerset.SubdomainUniquePerReplica
	lws.Spec.NetworkConfig = &leaderworkerset.NetworkConfig{
		SubdomainPolicy: &subdomainPolicy,
	}
	restoredLws, err := ApplyRevision(lws, revision)
	if err != nil {
		t.Fatal(err)
	}

	restoredRevision, err := NewRevision(restoredLws, 2, LeaderWorkerTemplateHash(restoredLws))
	if err != nil {
		t.Fatal(err)
	}

	if !history.EqualRevision(revision, restoredRevision) {
		t.Errorf("expected value %v, got %v", revision, restoredRevision)
	}

	if diff := cmp.Diff(currentLws.Spec.LeaderWorkerTemplate, restoredLws.Spec.LeaderWorkerTemplate); diff != "" {
		t.Errorf("unexpected restored LeaderWorkerTemplate: %s", diff)
	}

	if diff := cmp.Diff(currentLws.Spec.NetworkConfig, restoredLws.Spec.NetworkConfig); diff != "" {
		t.Errorf("NetworkConfig should be restored %s", diff)
	}
}
