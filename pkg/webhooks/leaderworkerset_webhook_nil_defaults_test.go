/*
Copyright 2026.

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

package webhooks

import (
	"context"
	"testing"

	"k8s.io/utils/ptr"

	v1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/test/wrappers"
)

// runValidateCreate calls ValidateCreate and turns a panic into a normal test
// failure instead of crashing the whole test binary, so a regression is
// reported as a clear assertion failure rather than a fatal signal.
func runValidateCreate(t *testing.T, lws *v1.LeaderWorkerSet) error {
	t.Helper()
	webhook := &LeaderWorkerSetWebhook{}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ValidateCreate panicked: %v", r)
			}
		}()
		_, err = webhook.ValidateCreate(context.TODO(), lws)
	}()
	return err
}

// generalValidate reads replicas as a bare dereference, relying on the CRD
// schema default and on Default() having run. Neither holds for an object that
// reaches validation without them: a CRD installed from an older revision, or a
// caller invoking ValidateCreate directly.
func TestValidateCreateNilReplicas(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.Replicas = nil

	if err := runValidateCreate(t, lws); err != nil {
		t.Errorf("expected nil replicas to be accepted, got: %v", err)
	}
}

// size has the same dereference-before-nil-check shape as replicas, and unlike
// replicas it is not backfilled by Default() either.
func TestValidateCreateNilSize(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.LeaderWorkerTemplate.Size = nil

	if err := runValidateCreate(t, lws); err != nil {
		t.Errorf("expected nil size to be accepted, got: %v", err)
	}
}

// validateUpdateSubGroupPolicy dereferences size as well, on a path reached
// only when subGroupPolicy is set, so it needs its own case.
func TestValidateCreateNilSizeWithSubGroupPolicy(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.LeaderWorkerTemplate.Size = nil
	lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &v1.SubGroupPolicy{
		SubGroupSize: ptr.To[int32](1),
	}

	if err := runValidateCreate(t, lws); err != nil {
		t.Errorf("expected nil size with subGroupPolicy to be accepted, got: %v", err)
	}
}
