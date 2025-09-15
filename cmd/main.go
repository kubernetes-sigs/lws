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
	"crypto/tls"
	"flag"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	configapi "sigs.k8s.io/lws/api/config/v1alpha1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/cert"
	"sigs.k8s.io/lws/pkg/config"
	"sigs.k8s.io/lws/pkg/controllers"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/pkg/utils"
	"sigs.k8s.io/lws/pkg/utils/useragent"
	"sigs.k8s.io/lws/pkg/version"
	"sigs.k8s.io/lws/pkg/webhooks"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	flagsSet = make(map[string]bool)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(leaderworkersetv1.AddToScheme(scheme))
	utilruntime.Must(configapi.AddToScheme(scheme))
	utilruntime.Must(volcanov1beta1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr string
		probeAddr   string
		qps         float64
		burst       int

		// leader election
		enableLeaderElection     bool
		leaderElectLeaseDuration time.Duration
		leaderElectRenewDeadline time.Duration
		leaderElectRetryPeriod   time.Duration
		leaderElectResourceLock  string
		leaderElectionID         string
		configFile               string
		printVersion             bool
	)

	flag.BoolVar(&printVersion, "version", false, "Print version information and exit")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "DEPRECATED(please pass configuration file via --config flag): The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "DEPRECATED(please pass configuration file via --config flag): The address the probe endpoint binds to.")
	flag.Float64Var(&qps, "kube-api-qps", 500, "Maximum QPS to use while talking with Kubernetes API")
	flag.IntVar(&burst, "kube-api-burst", 500, "Maximum burst for throttle while talking with Kubernetes API")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.DurationVar(&leaderElectLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"DEPRECATED(please pass configuration file via --config flag): The duration that non-leader candidates will wait after observing a leadership renewal until attempting to acquire "+
			"leadership of a led but unrenewed leader slot. This is effectively the maximum duration that a leader can be stopped"+
			" before it is replaced by another candidate. This is only applicable if leader election is enabled.")
	flag.DurationVar(&leaderElectRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"DEPRECATED(please pass configuration file via --config flag): The interval between attempts by the acting master to renew a leadership slot before it stops leading. This"+
			"must be less than or equal to the lease duration. This is only applicable if leader election is enabled.")
	flag.DurationVar(&leaderElectRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"DEPRECATED(please pass configuration file via --config flag): The duration the clients should wait between attempting acquisition and renewal of a leadership. This is only"+
			"applicable if leader election is enabled.")
	flag.StringVar(&leaderElectResourceLock, "leader-elect-resource-lock", "leases",
		"DEPRECATED(please pass configuration file via --config flag): The type of resource object that is used for locking during leader election. Supported options are "+
			"'endpoints', 'configmaps', 'leases', 'endpointsleases' and 'configmapsleases'")
	flag.StringVar(&leaderElectionID, "leader-elect-resource-name", "b8b2488c.x-k8s.io",
		"DEPRECATED(please pass configuration file via --config flag): The name of resource object that is used for locking during leader election. ")
	flag.StringVar(&configFile, "config", "",
		"The controller will load its initial configuration from this file. "+
			"Command-line flags will override any configurations set in this file. "+
			"Omit this flag to use the default configuration values.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	flag.Visit(func(f *flag.Flag) {
		flagsSet[f.Name] = true
	})

	if printVersion {
		fmt.Printf("Version: %s\n", version.GitVersion)
		fmt.Printf("Build Date: %s\n", version.BuildDate)
		fmt.Printf("Git Commit: %s\n", version.GitCommit)
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	options, cfg, err := apply(configFile, probeAddr, enableLeaderElection, leaderElectLeaseDuration, leaderElectRenewDeadline, leaderElectRetryPeriod, leaderElectResourceLock, leaderElectionID, metricsAddr)
	if err != nil {
		setupLog.Error(err, "unable to load the configuration")
		os.Exit(1)
	}

	kubeConfig := ctrl.GetConfigOrDie()

	kubeConfig.QPS = *cfg.ClientConnection.QPS
	if flagsSet["kube-api-qps"] {
		kubeConfig.QPS = float32(qps)
	}
	kubeConfig.Burst = int(*cfg.ClientConnection.Burst)
	if flagsSet["kube-api-burst"] {
		kubeConfig.Burst = burst
	}
	if kubeConfig.UserAgent == "" {
		kubeConfig.UserAgent = useragent.Default()
	}
	setupLog.Info("Initializing", "gitVersion", version.GitVersion, "buildDate", version.BuildDate, "gitCommit", version.GitCommit, "userAgent", kubeConfig.UserAgent)

	mgr, err := ctrl.NewManager(kubeConfig, options)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	certsReady := make(chan struct{})
	if cfg.InternalCertManagement != nil && *cfg.InternalCertManagement.Enable {
		if err = cert.CertsManager(mgr, options.LeaderElectionNamespace, *cfg.InternalCertManagement.WebhookServiceName, *cfg.InternalCertManagement.WebhookSecretName, cfg.Webhook.CertDir, certsReady); err != nil {
			setupLog.Error(err, "unable to setup cert rotation")
			os.Exit(1)
		}
	} else {
		close(certsReady)
	}

	if err := controllers.SetupIndexes(mgr.GetFieldIndexer()); err != nil {
		setupLog.Error(err, "unable to setup indexes")
	}

	// Cert won't be ready until manager starts, so start a goroutine here which
	// will block until the cert is ready before setting up the controllers.
	// Controllers who register after manager starts will start directly.
	go setupControllers(mgr, certsReady, cfg)

	setupHealthzAndReadyzCheck(mgr)
	setupLog.Info("starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}

}
func setupControllers(mgr ctrl.Manager, certsReady chan struct{}, cfg configapi.Configuration) {
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
	// Set up scheduler provider
	var sp schedulerprovider.SchedulerProvider
	if cfg.GangSchedulingManagement != nil {
		var err error
		sp, err = schedulerprovider.NewSchedulerProvider(schedulerprovider.ProviderType(*cfg.GangSchedulingManagement.SchedulerProvider), mgr.GetClient())
		if err != nil {
			setupLog.Error(err, "unable to create scheduler provider", "provider", *cfg.GangSchedulingManagement.SchedulerProvider)
			os.Exit(1)
		}
		setupLog.Info("Gang scheduling enabled", "provider", *cfg.GangSchedulingManagement.SchedulerProvider)
	}
	// Set up pod reconciler.
	podController := controllers.NewPodReconciler(mgr.GetClient(), mgr.GetScheme(), mgr.GetEventRecorderFor("leaderworkerset"), sp)
	if err := podController.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Pod")
		os.Exit(1)
	}
	// Set up webhooks
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhooks.SetupLeaderWorkerSetWebhook(mgr); err != nil {
			setupLog.Error(err, "unable to create leaderworkerset webhook", "webhook", "LeaderWorkerSet")
			os.Exit(1)
		}
		pw := webhooks.NewPodWebhook(sp)
		if err := pw.Setup(mgr); err != nil {
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

func apply(configFile string,
	probeAddr string,
	enableLeaderElection bool,
	leaderElectLeaseDuration time.Duration,
	leaderElectRenewDeadline time.Duration,
	leaderElectRetryPeriod time.Duration,
	leaderElectResourceLock,
	leaderElectionID string,
	metricsAddr string) (ctrl.Options, configapi.Configuration, error) {
	namespace := utils.GetOperatorNamespace()

	options, cfg, err := config.Load(scheme, configFile)
	if err != nil {
		return options, cfg, err
	}
	cfgStr, err := config.Encode(scheme, &cfg)
	if err != nil {
		return options, cfg, err
	}

	if flagsSet["health-probe-bind-address"] {
		options.HealthProbeBindAddress = probeAddr
	}
	if flagsSet["leader-elect"] {
		options.LeaderElection = enableLeaderElection
	}
	if flagsSet["leader-elect-lease-duration"] {
		options.LeaseDuration = &leaderElectLeaseDuration
	}
	if flagsSet["leader-elect-renew-deadline"] {
		options.RenewDeadline = &leaderElectRenewDeadline
	}
	if flagsSet["leader-elect-retry-period"] {
		options.RetryPeriod = &leaderElectRetryPeriod
	}
	if flagsSet["leader-elect-resource-lock"] {
		options.LeaderElectionResourceLock = leaderElectResourceLock
	}
	if flagsSet["leader-elect-resource-name"] {
		options.LeaderElectionID = leaderElectionID
	}

	// Disabling http/2 to prevent being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !flagsSet["metrics-bind-address"] {
		metricsAddr = cfg.Metrics.BindAddress
	}

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:    metricsAddr,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
		TLSOpts:        []func(*tls.Config){disableHTTP2},
	}

	options.Metrics = metricsServerOptions
	options.LeaderElectionNamespace = namespace

	setupLog.Info("Successfully loaded configuration", "config", cfgStr)
	return options, cfg, nil
}
