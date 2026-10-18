/*
Copyright 2026 TechDufus.

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

// Command manager runs the omp-operator: the reconcilers, the dashboard, the hooks server and
// the agent API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/actions"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/celx"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/controller"
	"github.com/TechDufus/omp-operator/internal/dashboard"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/hooks"
	"github.com/TechDufus/omp-operator/internal/metrics"
	"github.com/TechDufus/omp-operator/internal/notify"
	"github.com/TechDufus/omp-operator/internal/store"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sourcev1.AddToScheme(scheme))
	utilruntime.Must(ompv1alpha1.AddToScheme(scheme))
}

type flags struct {
	metricsAddr, probeAddr, webAddr, hooksAddr, agentAPIAddr string
	secureMetrics                                            bool
	serviceName, dataDir, defaultRuntimeImage                string
	dashboardURL, hooksPublicURL, gatewayClientsSecret       string
	agentNamespaces, actionTargetNamespaces, accessConfig    string
}

func main() {
	var f flags
	flag.StringVar(&f.metricsAddr, "metrics-bind-address", ":9090", "Metrics endpoint address; 0 disables it.")
	flag.BoolVar(&f.secureMetrics, "metrics-secure", false, "Serve metrics over HTTPS with authn/authz.")
	flag.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.StringVar(&f.webAddr, "web-bind-address", ":8080", "Address the dashboard binds to.")
	flag.StringVar(&f.hooksAddr, "hooks-bind-address", ":8082", "Address the webhook receiver binds to.")
	flag.StringVar(&f.agentAPIAddr, "agent-api-bind-address", ":8083", "Address the session/toolsync API binds to.")
	flag.StringVar(&f.serviceName, "service-name", "omp-operator", "Service through which pods reach the agent API.")
	flag.StringVar(&f.dataDir, "data-dir", "/data", "Directory for session files, continuity and toolchains.")
	flag.StringVar(&f.defaultRuntimeImage, "default-runtime-image", "", "Image used when an agent sets no image.")
	flag.StringVar(&f.dashboardURL, "dashboard-url", "", "External dashboard URL, for notification links.")
	flag.StringVar(&f.hooksPublicURL, "hooks-public-url", "", "External hooks URL shown in the dashboard.")
	flag.StringVar(&f.gatewayClientsSecret, "gateway-clients-secret", "",
		"Gateway clients Secret; enables managed gateway tokens.")
	flag.StringVar(&f.agentNamespaces, "agent-namespaces", "", "Comma list of namespaces served; empty means all.")
	flag.StringVar(&f.actionTargetNamespaces, "action-target-namespaces", "",
		"Comma list (or *) of namespaces actions may target besides their own.")
	flag.StringVar(&f.accessConfig, "access-config", "/etc/omp-operator/access.yaml", "Dashboard access file.")
	zapOpts := zap.Options{}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	if err := run(f); err != nil {
		setupLog.Error(err, "operator failed")
		os.Exit(1)
	}
}

func run(f flags) error {
	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		return errors.New("POD_NAMESPACE must be set")
	}
	apiPort, err := portOf(f.agentAPIAddr)
	if err != nil {
		return fmt.Errorf("--agent-api-bind-address: %w", err)
	}
	opts := &config.Options{
		OperatorNamespace:      operatorNamespace,
		ServiceName:            f.serviceName,
		AgentAPIPort:           apiPort,
		DataDir:                f.dataDir,
		DefaultRuntimeImage:    f.defaultRuntimeImage,
		DashboardURL:           f.dashboardURL,
		HooksPublicURL:         f.hooksPublicURL,
		GatewayClientsSecret:   f.gatewayClientsSecret,
		AgentNamespaces:        config.SplitList(f.agentNamespaces),
		ActionTargetNamespaces: config.SplitList(f.actionTargetNamespaces),
	}
	metrics.SetBuildInfo(version)

	st, err := store.New(opts.DataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	access, err := dashboard.LoadAccess(f.accessConfig)
	if err != nil {
		return fmt.Errorf("access config: %w", err)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: f.metricsAddr, SecureServing: f.secureMetrics},
		HealthProbeBindAddress: f.probeAddr,
		Cache:                  cacheOptions(opts),
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{
			&corev1.Secret{}, &corev1.ConfigMap{},
			&appsv1.Deployment{}, &appsv1.StatefulSet{}, &appsv1.DaemonSet{},
		}}},
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	c, apiReader := mgr.GetClient(), mgr.GetAPIReader()

	celCache := celx.NewCache()
	dispatcher := &notify.Dispatcher{Client: c, APIReader: apiReader, Sender: notify.NewSender(), Options: opts}
	var gw *gateway.Manager
	if opts.GatewayManaged() {
		gw = &gateway.Manager{Client: c, APIReader: apiReader, Options: opts}
	}
	runner := &actions.Runner{Client: c, APIReader: apiReader, Store: st, Options: opts}

	if err := (&controller.OmpActionReconciler{Client: c, Scheme: mgr.GetScheme(), Options: opts}).
		SetupWithManager(mgr); err != nil {
		return fmt.Errorf("ompaction controller: %w", err)
	}
	if err := (&controller.OmpAgentReconciler{
		Client: c, Scheme: mgr.GetScheme(), APIReader: apiReader, Options: opts, Store: st, Gateway: gw,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("ompagent controller: %w", err)
	}
	if err := (&controller.OmpTriggerReconciler{
		Client: c, Scheme: mgr.GetScheme(), APIReader: apiReader, Options: opts, CEL: celCache,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("omptrigger controller: %w", err)
	}
	sessions := &controller.OmpSessionReconciler{
		Client: c, Scheme: mgr.GetScheme(), APIReader: apiReader, Options: opts, Store: st, Notifier: dispatcher,
	}
	if gw != nil {
		sessions.Gateway = gw
	}
	if err := sessions.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("ompsession controller: %w", err)
	}

	if gw != nil {
		if err := mgr.Add(nonLeader(func(ctx context.Context) error {
			if err := gw.EnsureClientsSecret(ctx); err != nil {
				return fmt.Errorf("gateway clients secret: %w", err)
			}
			<-ctx.Done()
			return nil
		})); err != nil {
			return err
		}
	}
	if err := mgr.Add(nonLeader(func(ctx context.Context) error { return statfsLoop(ctx, st) })); err != nil {
		return err
	}

	hooksHandler := &hooks.Handler{Client: c, APIReader: apiReader, Options: opts, CEL: celCache, Notify: dispatcher}
	hooksMux := http.NewServeMux()
	hooksMux.Handle(hooks.PathPrefix, hooksHandler)
	apiServer := &agentapi.Server{Client: c, APIReader: apiReader, Store: st, Options: opts, Runner: runner}

	servers := []struct {
		name, addr string
		handler    func(ctx context.Context) (http.Handler, error)
		after      func()
	}{
		{name: "dashboard", addr: f.webAddr, handler: func(ctx context.Context) (http.Handler, error) {
			key, err := dashboard.EnsureSessionKey(ctx, c, apiReader, operatorNamespace)
			if err != nil {
				return nil, fmt.Errorf("dashboard session key: %w", err)
			}
			return (&dashboard.Server{
				Client: c, APIReader: apiReader, Store: st, Options: opts, Access: access, Key: key,
			}).Handler(), nil
		}},
		{name: "hooks", addr: f.hooksAddr, handler: func(context.Context) (http.Handler, error) {
			return hooksMux, nil
		}, after: hooksHandler.Wait},
		{name: "agent-api", addr: f.agentAPIAddr, handler: func(context.Context) (http.Handler, error) {
			return apiServer.Handler(), nil
		}},
	}
	for _, s := range servers {
		if err := mgr.Add(nonLeader(func(ctx context.Context) error {
			h, err := s.handler(ctx)
			if err != nil {
				return err
			}
			err = serve(ctx, s.name, s.addr, h)
			if s.after != nil {
				s.after()
			}
			return err
		})); err != nil {
			return err
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}

	setupLog.Info("starting manager", "version", version, "namespace", operatorNamespace)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// cacheOptions caches only operator-managed pods, and only in the served namespaces.
func cacheOptions(opts *config.Options) cache.Options {
	podCfg := cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{ompv1alpha1.LabelManagedBy: ompv1alpha1.ManagedByValue}),
	}
	if len(opts.AgentNamespaces) > 0 {
		podCfg.Namespaces = map[string]cache.Config{}
		for _, ns := range opts.AgentNamespaces {
			podCfg.Namespaces[ns] = cache.Config{}
		}
	}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{&corev1.Pod{}: podCfg}}
}

// nonLeader wraps fn as a runnable that runs without leader election, after the caches sync.
func nonLeader(fn func(ctx context.Context) error) manager.Runnable {
	return runnable{fn: fn}
}

type runnable struct {
	fn func(ctx context.Context) error
}

func (r runnable) Start(ctx context.Context) error { return r.fn(ctx) }
func (runnable) NeedLeaderElection() bool          { return false }

// serve runs an HTTP server until ctx ends, then shuts it down gracefully.
func serve(ctx context.Context, name, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		setupLog.Info("serving", "server", name, "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return fmt.Errorf("%s server: %w", name, err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("%s server shutdown: %w", name, err)
	}
	return nil
}

// statfsLoop publishes the data volume's free and total bytes every minute.
func statfsLoop(ctx context.Context, st *store.Store) error {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if free, capacity, err := st.Statfs(); err != nil {
			setupLog.Error(err, "statfs of data dir failed")
		} else {
			metrics.DataFreeBytes.Set(float64(free))
			metrics.DataCapacityBytes.Set(float64(capacity))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func portOf(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(p)
}
