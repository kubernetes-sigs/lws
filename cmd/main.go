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

package main

import (
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/cert"
	"sigs.k8s.io/lws/pkg/controllers"
	"sigs.k8s.io/lws/pkg/webhooks"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(leaderworkersetv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr string
		probeAddr   string
		qps         float64
		burst       int
		namespace   string

		// leader election
		enableLeaderElection     bool
		leaderElectLeaseDuration time.Duration
		leaderElectRenewDeadline time.Duration
		leaderElectRetryPeriod   time.Duration
		leaderElectResourceLock  string
		leaderElectionID         string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.Float64Var(&qps, "kube-api-qps", 500, "Maximum QPS to use while talking with Kubernetes API")
	flag.IntVar(&burst, "kube-api-burst", 500, "Maximum burst for throttle while talking with Kubernetes API")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.DurationVar(&leaderElectLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"The duration that non-leader candidates will wait after observing a leadership renewal until attempting to acquire "+
			"leadership of a led but unrenewed leader slot. This is effectively the maximum duration that a leader can be stopped"+
			" before it is replaced by another candidate. This is only applicable if leader election is enabled.")
	flag.DurationVar(&leaderElectRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"The interval between attempts by the acting master to renew a leadership slot before it stops leading. This"+
			"must be less than or equal to the lease duration. This is only applicable if leader election is enabled.")
	flag.DurationVar(&leaderElectRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"The duration the clients should wait between attempting acquisition and renewal of a leadership. This is only"+
			"applicable if leader election is enabled.")
	flag.StringVar(&leaderElectResourceLock, "leader-elect-resource-lock", "leases",
		"The type of resource object that is used for locking during leader election. Supported options are "+
			"'endpoints', 'configmaps', 'leases', 'endpointsleases' and 'configmapsleases'")
	flag.StringVar(&leaderElectionID, "leader-elect-resource-name", "b8b2488c.x-k8s.io",
		"The name of resource object that is used for locking during leader election. ")
	flag.StringVar(&namespace, "namespace", "lws-system", "The namespace that is used to deploy leaderWorkerSet controller")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	kubeConfig := ctrl.GetConfigOrDie()
	kubeConfig.QPS = float32(qps)
	kubeConfig.Burst = burst

	mgr, err := ctrl.NewManager(kubeConfig, ctrl.Options{
		Scheme:                     scheme,
		Metrics:                    metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:     probeAddr,
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           leaderElectionID,
		LeaderElectionResourceLock: leaderElectResourceLock,
		LeaderElectionNamespace:    namespace, // Using namespace variable here
		LeaseDuration:              &leaderElectLeaseDuration,
		RenewDeadline:              &leaderElectRenewDeadline,
		RetryPeriod:                &leaderElectRetryPeriod,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})

	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	certsReady := make(chan struct{})

	if err = cert.CertsManager(mgr, namespace, certsReady); err != nil {
		setupLog.Error(err, "unable to setup cert rotation")
		os.Exit(1)
	}

	if err := controllers.SetupIndexes(mgr.GetFieldIndexer()); err != nil {
		setupLog.Error(err, "unable to setup indexes")
	}

	// Cert won't be ready until manager starts, so start a goroutine here which
	// will block until the cert is ready before setting up the controllers.
	// Controllers who register after manager starts will start directly.
	go setupControllers(mgr, certsReady)

	setupHealthzAndReadyzCheck(mgr)
	setupLog.Info("starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}

}
func setupControllers(mgr ctrl.Manager, certsReady chan struct{}) {
	// The controllers won't work until the webhooks are operating,
	// and the webhook won't work until the certs are all in places.
	setupLog.Info("waiting for the cert generation to complete")
	<-certsReady
	setupLog.Info("certs ready")

	if err := controllers.NewLeaderWorkerSetReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("leaderworkerset"),
	).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LeaderWorkerSet")
		os.Exit(1)
	}
	// Set up pod reconciler.
	podController := controllers.NewPodReconciler(mgr.GetClient(), mgr.GetScheme())
	if err := podController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pod")
		os.Exit(1)
	}
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhooks.SetupLeaderWorkerSetWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to create leaderworkerset webhook", "webhook", "LeaderWorkerSet")
			os.Exit(1)
		}
		if err := webhooks.SetupPodWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to create pod webhook", "webhook", "LeaderWorkerSet")
			os.Exit(1)
		}
	}
	//+kubebuilder:scaffold:builder
}

func setupHealthzAndReadyzCheck(mgr ctrl.Manager) {
	defer setupLog.Info("both healthz and readyz check are finished and configured")
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}
