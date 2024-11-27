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

package controller

import (
	"testing"

	"sigs.k8s.io/lws/test/testutils"
)

func TestCreateApplyRevision(t *testing.T) {
	lws := testutils.BuildLeaderWorkerSet("default").Obj()
	lws.Status.CollisionCount = new(int32)
	revision, err := NewRevision(lws, 1, lws.Status.CollisionCount)

	lws.Spec.LeaderWorkerTemplate.LeaderTemplate.Spec.Containers[0].Name = "update-name"

	restoredLws, err := ApplyRevision(lws, re)

}
