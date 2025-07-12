/*
Copyright 2024 The Kubernetes Authors.
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

package e2e

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/lws/client-go/clientset/versioned/scheme"
	"sigs.k8s.io/lws/pkg/controllers"
	"sigs.k8s.io/lws/test/wrappers"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

const (
	timeout  = 30 * time.Second
	interval = time.Millisecond * 250
)

var cfg *rest.Config
var k8sClient client.Client
var ctx context.Context
var cancel context.CancelFunc

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	// cfg is defined in this file globally.
	cfg = config.GetConfigOrDie()
	Expect(cfg).NotTo(BeNil())

	err := leaderworkerset.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = admissionv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = appsv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = corev1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:         scheme.Scheme,
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"}, // Disable metrics
	})
	Expect(err).NotTo(HaveOccurred())

	err = controllers.SetupIndexes(mgr.GetFieldIndexer())
	Expect(err).NotTo(HaveOccurred())

	lwsController := controllers.NewLeaderWorkerSetReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("leaderworkerset"),
	)
	err = lwsController.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	podController := controllers.NewPodReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("pod"),
	)
	err = podController.SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	// Start the manager
	go func() {
		defer GinkgoRecover()
		err := mgr.Start(ctx)
		Expect(err).NotTo(HaveOccurred())
	}()

	Eventually(func() error {
		// Test if field indexer is available
		var testList appsv1.StatefulSetList
		matchingFields := client.MatchingFields{".metadata.controller": "nonexistent"}
		return mgr.GetClient().List(ctx, &testList, matchingFields)
	}, 30*time.Second, 1*time.Second).Should(Succeed())

	LwsReadyForTesting(k8sClient)
})

var _ = AfterSuite(func() {
	cancel()
})

func LwsReadyForTesting(client client.Client) {
	By("waiting for webhooks to come up")

	// To verify that webhooks are ready, let's create a simple lws.
	lws := wrappers.BuildLeaderWorkerSet(metav1.NamespaceDefault).Replica(3).Obj()

	// Once the creation succeeds, that means the webhooks are ready
	// and we can begin testing.
	Eventually(func() error {
		return client.Create(ctx, lws)
	}, timeout, interval).Should(Succeed())

	// Delete this leaderworkerset before beginning tests.
	Expect(client.Delete(ctx, lws))
	Eventually(func() error {
		return client.Get(ctx, types.NamespacedName{Name: lws.Name, Namespace: lws.Namespace}, &leaderworkerset.LeaderWorkerSet{})
	}).ShouldNot(Succeed())
}
