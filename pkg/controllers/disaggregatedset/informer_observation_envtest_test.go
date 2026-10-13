//go:build integration

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

package disaggregatedset

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

type observationTransport struct {
	http.RoundTripper
	mu    sync.Mutex
	reads []string
}

func (t *observationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet && request.URL.Query().Get("watch") != "true" {
		t.mu.Lock()
		t.reads = append(t.reads, request.URL.Path)
		t.mu.Unlock()
	}
	return t.RoundTripper.RoundTrip(request)
}

// Run with KUBEBUILDER_ASSETS=<envtest binaries> go test -tags=integration
// ./pkg/controllers/disaggregatedset -run TestInformerObservationWithAPIServer.
func TestInformerObservationWithAPIServer(t *testing.T) {
	environment := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	scheme := testSchemeForUnit()
	writer, err := client.NewWithWatch(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	for _, identity := range []leaderworkersetv1.GroupIdentityType{leaderworkersetv1.GroupIdentityOrdinal, leaderworkersetv1.GroupIdentityHash} {
		t.Run(string(identity), func(t *testing.T) {
			lws := revisionLWS(string(identity), testRolePrefill, 2, 0, time.Time{}, 2)
			lws.Name = "test-" + map[bool]string{true: "hash", false: "ordinal"}[identity == leaderworkersetv1.GroupIdentityHash]
			lws.Spec.GroupIdentity = identity
			lws.Spec.LeaderWorkerTemplate.Size = ptr.To[int32](1)
			objects := replicaGroupObjects(lws, 2, 0)
			ids := make(map[types.UID]types.UID)
			var victim *corev1.Pod
			// Create a real owner graph. envtest runs no native controllers, so
			// this test can advance each acknowledgement/watch independently.
			for _, object := range objects {
				oldUID := object.GetUID()
				object.SetUID("")
				for i := range object.GetOwnerReferences() {
					owner := &object.GetOwnerReferences()[i]
					if uid, ok := ids[owner.UID]; ok {
						owner.UID = uid
					}
				}
				labels := map[string]string{leaderworkersetv1.SetNameLabelKey: lws.Name}
				template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "test", Image: "test.invalid/image"}}}}
				switch object := object.(type) {
				case *leaderworkersetv1.LeaderWorkerSet:
					object.Spec.LeaderWorkerTemplate.WorkerTemplate = template
					object.Spec.StartupPolicy = leaderworkersetv1.LeaderCreatedStartupPolicy
					object.Spec.RolloutStrategy.Type = leaderworkersetv1.RollingUpdateStrategyType
				case *appsv1.StatefulSet:
					object.Spec.Template, object.Spec.Selector = template, &metav1.LabelSelector{MatchLabels: labels}
					object.Spec.ServiceName = lws.Name
				case *appsv1.Deployment:
					object.Spec.Template, object.Spec.Selector = template, &metav1.LabelSelector{MatchLabels: labels}
				case *appsv1.ReplicaSet:
					object.Spec.Template, object.Spec.Selector = template, &metav1.LabelSelector{MatchLabels: labels}
				case *corev1.Pod:
					object.Spec.Containers = template.Spec.Containers
					object.Finalizers = []string{"test.example/hold"}
					victim = object
				}
				require.NoError(t, writer.Create(t.Context(), object))
				ids[oldUID] = object.GetUID()
			}
			acknowledge := func(replicas int32) {
				for _, object := range objects {
					require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(object), object))
					switch object := object.(type) {
					case *leaderworkersetv1.LeaderWorkerSet:
						object.Status.ObservedGeneration = object.Generation
					case *appsv1.StatefulSet:
						object.Spec.Replicas = ptr.To(replicas)
						require.NoError(t, writer.Update(t.Context(), object))
						object.Status.ObservedGeneration = object.Generation
					case *appsv1.Deployment:
						object.Spec.Replicas = ptr.To(replicas)
						require.NoError(t, writer.Update(t.Context(), object))
						object.Status.ObservedGeneration = object.Generation
					case *appsv1.ReplicaSet:
						object.Spec.Replicas = ptr.To(replicas)
						require.NoError(t, writer.Update(t.Context(), object))
						object.Status.ObservedGeneration = object.Generation
					default:
						continue // Pods stay Pending and are never made Ready.
					}
					require.NoError(t, writer.Status().Update(t.Context(), object))
				}
			}
			acknowledge(2)
			start := func() (*informerObservation, *observationTransport, func()) {
				cfg := rest.CopyConfig(config)
				trace := &observationTransport{}
				cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { trace.RoundTripper = rt; return trace }
				mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
				require.NoError(t, err)
				o, err := newInformerObservation(mgr)
				require.NoError(t, err)
				require.True(t, o.NeedLeaderElection())
				ctx, cancel := context.WithCancel(t.Context())
				finished := make(chan error, 1)
				go func() { finished <- o.Start(ctx) }()
				wait, stopWaiting := context.WithTimeout(ctx, 20*time.Second)
				defer stopWaiting()
				require.NoError(t, o.waitForStartup(wait))
				stop := func() { cancel(); require.NoError(t, <-finished) }
				trace.mu.Lock()
				trace.reads = nil
				trace.mu.Unlock()
				return o, trace, stop
			}
			o, trace, stop := start()
			manager := NewLeaderWorkerSetManager(interceptor.NewClient(writer, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					t.Fatal("the mutation client must not be used for observation GETs")
					return nil
				},
				List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
					t.Fatal("the mutation client must not be used for observation LISTs")
					return nil
				},
			}))
			manager.reader, manager.observations = o, o
			observe := func() error {
				current := &leaderworkersetv1.LeaderWorkerSet{}
				if err := o.Get(t.Context(), client.ObjectKeyFromObject(lws), current); err != nil {
					return err
				}
				_, err := manager.observeCachedReadiness(t.Context(), []*leaderworkersetv1.LeaderWorkerSet{current})
				return err
			}
			require.NoError(t, observe(), "Pending Pods do not block scaling")
			require.NoError(t, manager.Scale(t.Context(), newTestDisaggregatedSet(), lws, 1))
			require.Eventually(t, func() bool { return o.observed(t.Context(), metav1.GetControllerOf(lws).UID, "0") == nil }, 5*time.Second, 10*time.Millisecond)
			require.ErrorIs(t, observe(), errReplicaGroupsPending, "the LWS receipt alone is insufficient")
			acknowledge(1)
			require.ErrorIs(t, observe(), errReplicaGroupsPending, "native acknowledgement alone is insufficient")
			trace.mu.Lock()
			reads := append([]string(nil), trace.reads...)
			trace.mu.Unlock()
			require.Empty(t, reads, "all observation reads after startup are served by informers")
			stop()
			// A restart loses local receipts; a cold, ordered bootstrap must still
			// see the pending deletion left by the previous process.
			o, _, stop = start()
			defer stop()
			manager.reader, manager.observations = o, o
			require.ErrorIs(t, observe(), errReplicaGroupsPending)
			require.NoError(t, writer.Delete(t.Context(), victim))
			require.Eventually(t, func() bool { return observe() == nil }, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, writer.Get(t.Context(), client.ObjectKeyFromObject(victim), victim))
			require.False(t, victim.DeletionTimestamp.IsZero(), "physical removal is not required")
		})
	}
}
