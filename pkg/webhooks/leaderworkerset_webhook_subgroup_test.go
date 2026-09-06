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
	"strings"
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

// runValidateUpdate is the ValidateUpdate counterpart of runValidateCreate.
func runValidateUpdate(t *testing.T, oldLws, newLws *v1.LeaderWorkerSet) error {
	t.Helper()
	webhook := &LeaderWorkerSetWebhook{}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ValidateUpdate panicked: %v", r)
			}
		}()
		_, err = webhook.ValidateUpdate(context.TODO(), oldLws, newLws)
	}()
	return err
}

// subGroupSize has no CRD default and is not a required schema field (see
// config/crd/bases/leaderworkerset.x-k8s.io_leaderworkersets.yaml), so a
// spec-valid request can set `subGroupPolicy: {}` without it. Before this
// fix, validateUpdateSubGroupPolicy dereferenced SubGroupSize unconditionally
// and panicked instead of returning a validation error.
func TestValidateCreateSubGroupPolicyWithoutSubGroupSize(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &v1.SubGroupPolicy{}

	err := runValidateCreate(t, lws)
	if err == nil {
		t.Fatal("expected an error for subGroupPolicy without subGroupSize, got nil")
	}
	if !strings.Contains(err.Error(), "subGroupSize") {
		t.Errorf("expected error to mention subGroupSize, got: %v", err)
	}
}

// subGroupSize: 0 is schema-valid (no minimum is enforced), but the size %
// subGroupSize checks that follow the "must be >= 1" rejection divide by
// subGroupSize unconditionally. Before this fix that was a divide-by-zero
// panic instead of the intended "must be equal or greater than 1" error.
func TestValidateCreateSubGroupPolicyZeroSubGroupSize(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &v1.SubGroupPolicy{
		SubGroupSize: ptr.To[int32](0),
	}

	err := runValidateCreate(t, lws)
	if err == nil {
		t.Fatal("expected an error for subGroupSize: 0, got nil")
	}
	if !strings.Contains(err.Error(), "subGroupSize must be equal or greater than 1") {
		t.Errorf("expected 'subGroupSize must be equal or greater than 1' error, got: %v", err)
	}
}

// A valid, positive subGroupSize must still be accepted; this guards against
// the nil/zero-handling fix rejecting legitimate specs.
func TestValidateCreateSubGroupPolicyValid(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Size(4).Obj()
	lws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &v1.SubGroupPolicy{
		SubGroupSize: ptr.To[int32](2),
	}

	if err := runValidateCreate(t, lws); err != nil {
		t.Errorf("expected valid subGroupPolicy to be accepted, got: %v", err)
	}
}

// A nil rollingUpdateConfiguration must not panic: its fields were read before
// the nil check that was meant to guard them.
func TestValidateCreateNilRollingUpdateConfiguration(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	lws.Spec.RolloutStrategy.RollingUpdateConfiguration = nil

	if err := runValidateCreate(t, lws); err != nil {
		t.Errorf("expected nil rollingUpdateConfiguration to be accepted, got: %v", err)
	}
}

// generalValidate reads replicas as a bare dereference, relying on the CRD
// schema default and on Default() having run. Neither holds for an object that
// reaches validation without them: a CRD installed from an older revision, or a
// caller invoking ValidateCreate directly, as the controller's own tests do.
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

// ValidateUpdate's immutability check dereferenced SubGroupSize on both the
// old and new object guarded only by "SubGroupPolicy != nil", not
// "SubGroupSize != nil". Exercise it directly so a future change to the nil
// guard is caught even though generalValidate's own check on the new object
// would otherwise mask this on the common path.
func TestValidateUpdateSubGroupPolicyNilSubGroupSizeDoesNotPanic(t *testing.T) {
	oldLws := wrappers.BuildLeaderWorkerSet("default").Size(4).Obj()
	oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy = &v1.SubGroupPolicy{
		SubGroupSize: ptr.To[int32](2),
	}
	newLws := oldLws.DeepCopy()
	// Simulate an old object that predates the subGroupSize-required
	// validation alongside an otherwise-valid new object.
	oldLws.Spec.LeaderWorkerTemplate.SubGroupPolicy.SubGroupSize = nil

	// Whether this is accepted or rejected is not the point of this test;
	// what matters is that it returns rather than panicking.
	_ = runValidateUpdate(t, oldLws, newLws)
}

// The "cannot set subdomainPolicy as null" branch is guarded on the new
// object's networkConfig only. An object stored before subdomainPolicy was
// defaulted has no networkConfig at all, so the old object must not be read
// unguarded on this path.
func TestValidateUpdateNilOldNetworkConfigDoesNotPanic(t *testing.T) {
	oldLws := wrappers.BuildLeaderWorkerSet("default").Obj()
	newLws := oldLws.DeepCopy()
	oldLws.Spec.NetworkConfig = nil
	newLws.Spec.NetworkConfig = &v1.NetworkConfig{}

	err := runValidateUpdate(t, oldLws, newLws)
	if err == nil {
		t.Error("expected an error for subdomainPolicy set to null, got nil")
	}
}
