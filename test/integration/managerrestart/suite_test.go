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

// Package managerrestart holds integration tests that stop and start the LWS
// controller manager. Unlike test/integration/controllers, no manager runs for
// the whole suite, so each spec decides when controllers are running and can
// change the cluster while they are down.
package managerrestart

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/controllers"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestManagerRestart(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Manager Restart Suite")
}

var _ = ginkgo.BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(ginkgo.GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		ControlPlane: envtest.ControlPlane{
			APIServer: &envtest.APIServer{Args: []string{
				"--feature-gates=GenericWorkload=true",
				"--runtime-config=scheduling.k8s.io/v1beta1=true",
			}},
		},
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "..", "..", "bin", "k8s",
			fmt.Sprintf("1.37.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}
	var err error
	cfg, err = testEnv.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(leaderworkerset.AddToScheme(scheme.Scheme)).To(gomega.Succeed())
	gomega.Expect(schedulingv1beta1.AddToScheme(scheme.Scheme)).To(gomega.Succeed())
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
})

var _ = ginkgo.AfterSuite(func() {
	cancel()
	gomega.Expect(testEnv.Stop()).To(gomega.Succeed())
})

// startManager starts a fresh controller manager, like a new controller
// process, with the LeaderWorkerSet and Pod controllers. It returns once the
// manager's caches are synced. The returned function stops the manager and
// waits for it to exit; it is also registered with DeferCleanup.
func startManager() (stop func()) {
	mgrOpts := ctrl.Options{
		Scheme: scheme.Scheme,
		// Several managers run in this process over the suite's lifetime, so
		// reuse the production controller names and leave metrics off.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		Metrics:    metricsserver.Options{BindAddress: "0"},
	}
	gomega.Expect(controllers.ConfigureCacheOptions(&mgrOpts.Cache)).To(gomega.Succeed())
	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	provider := schedulerprovider.NewKubernetesProvider(mgr.GetClient())
	gomega.Expect(controllers.SetupIndexes(mgr.GetFieldIndexer())).To(gomega.Succeed())
	gomega.Expect(schedulerprovider.SetupKubernetesIndexes(mgr.GetFieldIndexer())).To(gomega.Succeed())
	gomega.Expect(controllers.NewLeaderWorkerSetReconciler(mgr.GetClient(), mgr.GetScheme(),
		mgr.GetEventRecorder("leaderworkerset"), provider).SetupWithManager(mgr)).To(gomega.Succeed())
	gomega.Expect(controllers.NewPodReconciler(mgr.GetClient(), mgr.GetScheme(),
		mgr.GetEventRecorder("pod"), provider).SetupWithManager(mgr)).To(gomega.Succeed())

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer ginkgo.GinkgoRecover()
		defer close(done)
		gomega.Expect(mgr.Start(mgrCtx)).To(gomega.Succeed())
	}()
	gomega.Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(gomega.BeTrue())

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		mgrCancel()
		gomega.Eventually(done, "30s").Should(gomega.BeClosed())
	}
	ginkgo.DeferCleanup(stop)
	return stop
}
