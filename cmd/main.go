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
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	configapi "sigs.k8s.io/lws/api/config/v1"
	configapiv1alpha1 "sigs.k8s.io/lws/api/config/v1alpha1"
	disaggregatedsetv1 "sigs.k8s.io/lws/api/disaggregatedset/v1"
	leaderworkersetv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"
	"sigs.k8s.io/lws/pkg/cert"
	"sigs.k8s.io/lws/pkg/computedomain"
	"sigs.k8s.io/lws/pkg/config"
	"sigs.k8s.io/lws/pkg/controllers"
	disaggregatedsetcontroller "sigs.k8s.io/lws/pkg/controllers/disaggregatedset"
	"sigs.k8s.io/lws/pkg/features"
	"sigs.k8s.io/lws/pkg/schedulerprovider"
	"sigs.k8s.io/lws/pkg/utils"
	"sigs.k8s.io/lws/pkg/utils/useragent"
	"sigs.k8s.io/lws/pkg/version"
	"sigs.k8s.io/lws/pkg/webhooks"
	disaggregatedsetwebhook "sigs.k8s.io/lws/pkg/webhooks/disaggregatedset"
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
	utilruntime.Must(disaggregatedsetv1.AddToScheme(scheme))
	utilruntime.Must(configapi.AddToScheme(scheme))
	utilruntime.Must(configapiv1alpha1.AddToScheme(scheme))
	utilruntime.Must(volcanov1beta1.AddToScheme(scheme))
	utilruntime.Must(schedulingv1alpha3.AddToScheme(scheme))
	utilruntime.Must(schedulingv1beta1.AddToScheme(scheme))
	utilruntime.Must(computedomain.AddToScheme(scheme))
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

	if err := utilfeature.DefaultMutableFeatureGate.SetFromMap(cfg.FeatureGates); err != nil {
		setupLog.Error(err, "Unable to set flag gates for known features")
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

	enableWebhooks := os.Getenv("ENABLE_WEBHOOKS") != "false"
	certsReady := make(chan struct{})
	if enableWebhooks && cfg.InternalCertManagement != nil && *cfg.InternalCertManagement.Enable {
		if err = cert.CertsManager(mgr, options.LeaderElectionNamespace, *cfg.InternalCertManagement.WebhookServiceName, *cfg.InternalCertManagement.WebhookSecretName, cfg.Webhook.CertDir, certsReady); err != nil {
			setupLog.Error(err, "unable to setup cert rotation")
			os.Exit(1)
		}
	} else {
		close(certsReady)
	}

	if err := controllers.SetupIndexes(mgr.GetFieldIndexer()); err != nil {
		setupLog.Error(err, "unable to setup indexes")
		os.Exit(1)
	}
	if cfg.GangSchedulingManagement != nil &&
		cfg.GangSchedulingManagement.SchedulerProvider != nil &&
		*cfg.GangSchedulingManagement.SchedulerProvider == string(schedulerprovider.Kubernetes) {
		if err := schedulerprovider.SetupKubernetesIndexes(mgr.GetFieldIndexer()); err != nil {
			setupLog.Error(err, "unable to setup Kubernetes scheduler provider indexes")
			os.Exit(1)
		}
	}
	if features.Enabled(features.AutoComputeDomain) {
		if err := computedomain.SetupIndexes(mgr.GetFieldIndexer()); err != nil {
			setupLog.Error(err, "unable to setup ComputeDomain field indexes")
			os.Exit(1)
		}
	}

	// Register controllers before starting the manager so an elected leader does
	// not hold the lease without running reconcilers while waiting for its local
	// webhook certificate volume mount to project.
	sp, providerType, err := setupControllers(mgr, cfg)
	if err != nil {
		setupLog.Error(err, "unable to setup controllers")
		os.Exit(1)
	}

	webhooksReady := make(chan struct{})
	if enableWebhooks {
		// Cert won't be ready until manager starts, so start a goroutine here which
		// will block until the cert is ready before setting up the webhooks.
		go func() {
			if err := setupWebhooks(mgr, certsReady, webhooksReady, sp, providerType); err != nil {
				setupLog.Error(err, "unable to setup webhooks")
				os.Exit(1)
			}
		}()
	}

	setupHealthzAndReadyzCheck(mgr, webhooksReady, enableWebhooks)
	setupLog.Info("starting manager")

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}

}

func setupControllers(mgr ctrl.Manager, cfg configapi.Configuration) (schedulerprovider.SchedulerProvider, schedulerprovider.ProviderType, error) {
	// Set up scheduler provider before controllers and webhooks so every
	// component observes the same provider.
	var (
		err          error
		sp           schedulerprovider.SchedulerProvider
		providerType schedulerprovider.ProviderType
	)
	if cfg.GangSchedulingManagement != nil && cfg.GangSchedulingManagement.SchedulerProvider != nil {
		providerType = schedulerprovider.ProviderType(*cfg.GangSchedulingManagement.SchedulerProvider)
		sp, err = schedulerprovider.NewSchedulerProvider(providerType, mgr.GetClient())
		if err != nil {
			return nil, "", fmt.Errorf("creating scheduler provider %q: %w", providerType, err)
		}
		setupLog.Info("Gang scheduling enabled", "provider", providerType)
	}
	if features.Enabled(features.AutoComputeDomain) {
		if err := computedomain.VerifyAPI(mgr.GetRESTMapper()); err != nil {
			return nil, "", fmt.Errorf("AutoComputeDomain feature gate requires the NVIDIA ComputeDomain CRD: %w", err)
		}
		if err := computedomain.NewReconciler(mgr.GetClient()).SetupWithManager(mgr); err != nil {
			return nil, "", fmt.Errorf("creating ComputeDomain controller: %w", err)
		}
		setupLog.Info("AutoComputeDomain management enabled")
	}

	lwsController := controllers.NewLeaderWorkerSetReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorder("leaderworkerset"),
		sp,
	)
	if err := lwsController.SetupWithManager(mgr); err != nil {
		return nil, "", fmt.Errorf("creating LeaderWorkerSet controller: %w", err)
	}
	// Set up DisaggregatedSet controller
	disaggregatedSetController := disaggregatedsetcontroller.NewDisaggregatedSetReconciler(
		mgr.GetClient(), mgr.GetScheme(), mgr.GetEventRecorder("disaggregatedset"),
	)
	if err := disaggregatedSetController.SetupWithManager(mgr); err != nil {
		return nil, "", fmt.Errorf("creating DisaggregatedSet controller: %w", err)
	}
	// Set up pod reconciler.
	podController := controllers.NewPodReconciler(mgr.GetClient(), mgr.GetScheme(), mgr.GetEventRecorder("leaderworkerset"), sp)
	if err := podController.SetupWithManager(mgr); err != nil {
		return nil, "", fmt.Errorf("creating Pod controller: %w", err)
	}
	//+kubebuilder:scaffold:builder
	return sp, providerType, nil
}

func setupWebhooks(mgr ctrl.Manager, certsReady <-chan struct{}, webhooksReady chan<- struct{}, sp schedulerprovider.SchedulerProvider, providerType schedulerprovider.ProviderType) error {
	setupLog.Info("waiting for the cert generation to complete")
	<-certsReady
	setupLog.Info("certs ready")

	if err := webhooks.SetupLeaderWorkerSetWebhook(mgr, webhooks.LeaderWorkerSetWebhook{
		SchedulerProvider: providerType,
	}); err != nil {
		return fmt.Errorf("creating LeaderWorkerSet webhook: %w", err)
	}
	pw := webhooks.NewPodWebhook(sp)
	if err := pw.Setup(mgr); err != nil {
		return fmt.Errorf("creating Pod webhook: %w", err)
	}
	if err := disaggregatedsetwebhook.SetupDisaggregatedSetWebhook(mgr); err != nil {
		return fmt.Errorf("creating DisaggregatedSet webhook: %w", err)
	}
	close(webhooksReady)
	return nil
}

func setupHealthzAndReadyzCheck(mgr ctrl.Manager, webhooksReady <-chan struct{}, enableWebhooks bool) {
	defer setupLog.Info("both healthz and readyz check are finished and configured")
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	readyz := healthz.Ping
	if enableWebhooks {
		// The first GetWebhookServer call starts the webhook server, so the
		// check waits until setupWebhooks has registered the webhooks.
		readyz = func(req *http.Request) error {
			select {
			case <-webhooksReady:
				return mgr.GetWebhookServer().StartedChecker()(req)
			default:
				return errors.New("webhooks are not registered yet")
			}
		}
	}
	if err := mgr.AddReadyzCheck("readyz", readyz); err != nil {
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

	// Parse TLS options from the configuration.
	var parsedTLSConfig *config.TLS
	if cfg.TLS != nil { // Note: cfg.TLS, not cfg.Webhook.TLS
		var err error
		parsedTLSConfig, err = config.ParseTLSOptions(cfg.TLS)
		if err != nil {
			return options, cfg, fmt.Errorf("unable to parse TLS options from configuration: %w", err)
		}
	}

	// Disabling http/2 to prevent being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	serverTLSOpts := []func(*tls.Config){disableHTTP2}

	// Append TLS options parsed from the configuration.
	if parsedTLSConfig != nil {
		tlsOpts := config.BuildTLSOptions(parsedTLSConfig)
		serverTLSOpts = append(serverTLSOpts, tlsOpts...)
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
		TLSOpts:        serverTLSOpts,
	}

	options.Metrics = metricsServerOptions
	options.LeaderElectionNamespace = namespace
	if err := controllers.ConfigureCacheOptions(&options.Cache); err != nil {
		return options, cfg, fmt.Errorf("unable to configure manager cache options: %w", err)
	}

	config.AddWebhookSettingsTo(&options, &cfg, serverTLSOpts)

	setupLog.Info("Successfully loaded configuration", "config", cfgStr)
	return options, cfg, nil
}
