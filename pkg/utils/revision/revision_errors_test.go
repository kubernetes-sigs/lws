/*
Copyright 2026 The Kubernetes Authors.

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
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"sigs.k8s.io/lws/test/wrappers"
)

var errBoom = errors.New("boom")

func TestCreateRevisionError(t *testing.T) {
	ctx := context.Background()
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return errBoom
		},
	}).Build()

	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	cr, err := NewRevision(ctx, fake.NewClientBuilder().Build(), lws, "")
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}

	got, err := CreateRevision(ctx, failing, cr)
	if !errors.Is(err, errBoom) {
		t.Errorf("CreateRevision() error = %v, want %v", err, errBoom)
	}
	if got != nil {
		t.Errorf("CreateRevision() = %v, want nil on error", got)
	}
}

func TestGetRevisionListError(t *testing.T) {
	ctx := context.Background()
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return errBoom
		},
	}).Build()

	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	if _, err := GetRevision(ctx, failing, lws, "key"); !errors.Is(err, errBoom) {
		t.Errorf("GetRevision() error = %v, want %v", err, errBoom)
	}
	if err := TruncateRevisions(ctx, failing, lws, "key"); !errors.Is(err, errBoom) {
		t.Errorf("TruncateRevisions() error = %v, want %v", err, errBoom)
	}
}

func TestTruncateRevisionsDeleteError(t *testing.T) {
	ctx := context.Background()
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()

	seed := fake.NewClientBuilder().Build()
	cr, err := NewRevision(ctx, seed, lws, "stale")
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	cr.Name = revisionName(lws.Name, "stale", 1)

	failing := fake.NewClientBuilder().
		WithObjects(cr).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return errBoom
			},
		}).Build()

	if err := TruncateRevisions(ctx, failing, lws, "current"); !errors.Is(err, errBoom) {
		t.Errorf("TruncateRevisions() error = %v, want %v", err, errBoom)
	}
}

func TestApplyRevisionInvalidPatch(t *testing.T) {
	lws := wrappers.BuildLeaderWorkerSet("default").Obj()
	revision := &appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: []byte("not json")}}

	got, err := ApplyRevision(lws, revision)
	if err == nil {
		t.Fatalf("ApplyRevision() = %v, want an error for a malformed patch", got)
	}
}

func TestHashRevisionUsesDataObject(t *testing.T) {
	// A revision may carry a decoded object instead of raw bytes; the hash must
	// still be stable and depend on the object's contents.
	objectA := &appsv1.ControllerRevision{Revision: 1}
	objectB := &appsv1.ControllerRevision{Revision: 2}

	a := &appsv1.ControllerRevision{Data: runtime.RawExtension{Object: objectA}}
	b := &appsv1.ControllerRevision{Data: runtime.RawExtension{Object: objectA}}
	c := &appsv1.ControllerRevision{Data: runtime.RawExtension{Object: objectB}}

	if hashRevision(a) != hashRevision(b) {
		t.Error("identical data objects must hash identically")
	}
	if hashRevision(a) == hashRevision(c) {
		t.Error("different data objects must hash differently")
	}
}
