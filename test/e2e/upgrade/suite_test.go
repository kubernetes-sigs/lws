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

package upgrade

import (
	"context"
	"os"
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
)

var (
	ctx             context.Context
	cancel          context.CancelFunc
	k8sClient       client.Client
	upgradePhase    string
	snapshotPath    string
	currentImageTag string
)

func TestUpgrade(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Upgrade E2E Suite")
}

var _ = ginkgo.BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(ginkgo.GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	upgradePhase = os.Getenv("LWS_UPGRADE_PHASE")
	if upgradePhase != "before" && upgradePhase != "after" {
		ginkgo.Fail("LWS_UPGRADE_PHASE must be either before or after")
	}

	snapshotPath = os.Getenv("LWS_UPGRADE_SNAPSHOT_PATH")
	gomega.Expect(snapshotPath).NotTo(gomega.BeEmpty())

	currentImageTag = os.Getenv("IMAGE_TAG")
	gomega.Expect(currentImageTag).NotTo(gomega.BeEmpty())

	testScheme := runtime.NewScheme()
	gomega.Expect(corev1.AddToScheme(testScheme)).To(gomega.Succeed())
	gomega.Expect(appsv1.AddToScheme(testScheme)).To(gomega.Succeed())
	gomega.Expect(leaderworkersetv1.AddToScheme(testScheme)).To(gomega.Succeed())
	gomega.Expect(disaggregatedsetv1.AddToScheme(testScheme)).To(gomega.Succeed())

	var err error
	k8sClient, err = client.New(config.GetConfigOrDie(), client.Options{Scheme: testScheme})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	waitForWebhooks()
})

var _ = ginkgo.AfterSuite(func() {
	cancel()
})
