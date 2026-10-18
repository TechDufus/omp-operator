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

package controller

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ompv1alpha1 "github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/actions"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/celx"
	"github.com/TechDufus/omp-operator/internal/config"
	"github.com/TechDufus/omp-operator/internal/gateway"
	"github.com/TechDufus/omp-operator/internal/notify"
	"github.com/TechDufus/omp-operator/internal/store"
	// +kubebuilder:scaffold:imports
)

// The envtest suite runs the four real reconcilers, wired as in cmd/main.go, against an
// API server without kubelet or controller-manager: tests play the kubelet by patching pod
// status and play the agent API by patching session status (or calling the real agentapi
// handler). Time is a shared fake clock; shortened requeues keep the specs quick.

const (
	etOperatorNS    = "omp-system"
	etClientsSecret = "omp-auth-clients"
	etRuntimeImage  = "example.com/omp-runtime:envtest"
	etUnserved      = "et-unserved"
)

// etNamespaces are the namespaces the operator serves (--agent-namespaces). Each spec group
// works in its own, so agents of different groups never meet.
var etNamespaces = []string{
	"et-actions", "et-agents", "et-triggers", "et-pods", "et-lifecycle", "et-queue",
	"et-gate", "et-toolchain", "et-cleanup", "et-flux", "et-samples", "et-clients",
}

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client

	etScheme  *runtime.Scheme
	etOpts    *config.Options
	etStore   *store.Store
	etDataDir string
	etClock   = &etFakeClock{}
	etSink    *etSinkServer
	etGateway *gateway.Manager
	etAPI     *httptest.Server
	etDone    = make(chan struct{})
)

// etFakeClock is real time plus an offset the specs move forward. Real time keeps flowing so
// object creation timestamps (always real) stay comparable with it; specs that advance the
// clock restore it with etClock.Reset when they are done.
type etFakeClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *etFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *etFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

func (c *etFakeClock) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = 0
}

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	SetDefaultEventuallyTimeout(20 * time.Second)
	SetDefaultEventuallyPollingInterval(50 * time.Millisecond)
	SetDefaultConsistentlyDuration(1500 * time.Millisecond)
	SetDefaultConsistentlyPollingInterval(100 * time.Millisecond)

	ctx, cancel = context.WithCancel(context.Background())

	etScheme = runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(etScheme)).To(Succeed())
	Expect(sourcev1.AddToScheme(etScheme)).To(Succeed())
	Expect(ompv1alpha1.AddToScheme(etScheme)).To(Succeed())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("testdata", "flux"), // source-controller api v1.9.6 CRDs
		},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: etScheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	for _, ns := range slices.Concat(etNamespaces, []string{etUnserved, etOperatorNS}) {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	}

	By("starting the manager")
	etDataDir, err = os.MkdirTemp("", "omp-operator-envtest-")
	Expect(err).NotTo(HaveOccurred())
	etStore, err = store.New(etDataDir)
	Expect(err).NotTo(HaveOccurred())
	etOpts = &config.Options{
		OperatorNamespace:    etOperatorNS,
		ServiceName:          "omp-operator",
		AgentAPIPort:         8083,
		DataDir:              etDataDir,
		DashboardURL:         "https://dash.example.com",
		GatewayClientsSecret: etClientsSecret,
		AgentNamespaces:      slices.Clone(etNamespaces),
	}
	etSink = newEtSinkServer()

	podCache := cache.ByObject{
		Label:      labels.SelectorFromSet(labels.Set{ompv1alpha1.LabelManagedBy: ompv1alpha1.ManagedByValue}),
		Namespaces: map[string]cache.Config{},
	}
	for _, ns := range etOpts.AgentNamespaces {
		podCache.Namespaces[ns] = cache.Config{}
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 etScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache:                  cache.Options{ByObject: map[client.Object]cache.ByObject{&corev1.Pod{}: podCache}},
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{
			&corev1.Secret{}, &corev1.ConfigMap{},
			&appsv1.Deployment{}, &appsv1.StatefulSet{}, &appsv1.DaemonSet{},
		}}},
	})
	Expect(err).NotTo(HaveOccurred())
	c, apiReader := mgr.GetClient(), mgr.GetAPIReader()

	dispatcher := &notify.Dispatcher{
		Client: c, APIReader: apiReader, Options: etOpts, Now: etClock.Now,
		Sender: &notify.Sender{Client: etSink.srv.Client(), RetryDelay: 10 * time.Millisecond},
	}
	gw := &gateway.Manager{Client: c, APIReader: apiReader, Options: etOpts, Now: etClock.Now}
	etGateway = gw
	runner := &actions.Runner{Client: c, APIReader: apiReader, Store: etStore, Options: etOpts, Now: etClock.Now}

	Expect((&OmpActionReconciler{Client: c, Scheme: mgr.GetScheme(), Options: etOpts}).SetupWithManager(mgr)).To(Succeed())
	Expect((&OmpAgentReconciler{
		Client: c, Scheme: mgr.GetScheme(), APIReader: apiReader, Options: etOpts, Store: etStore, Gateway: gw,
	}).SetupWithManager(mgr)).To(Succeed())
	Expect((&OmpTriggerReconciler{
		Client: c, Scheme: mgr.GetScheme(), APIReader: apiReader, Options: etOpts, CEL: celx.NewCache(), Now: etClock.Now,
	}).SetupWithManager(mgr)).To(Succeed())
	Expect((&OmpSessionReconciler{
		Client: c, Scheme: mgr.GetScheme(), APIReader: apiReader, Options: etOpts, Store: etStore,
		Notifier: dispatcher, Gateway: gw, Now: etClock.Now,
		// Shortened waits: the wake-ups that production spaces 10-60 s apart.
		NotReadyRequeue:  200 * time.Millisecond,
		QueueRequeue:     200 * time.Millisecond,
		ToolchainRequeue: 200 * time.Millisecond,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect(mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if err := gw.EnsureClientsSecret(ctx); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}))).To(Succeed())

	apiServer := &agentapi.Server{
		Client: c, APIReader: apiReader, Store: etStore, Options: etOpts, Runner: runner, Now: etClock.Now,
	}
	etAPI = httptest.NewServer(apiServer.Handler())

	go func() {
		defer GinkgoRecover()
		defer close(etDone)
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

	Eventually(func(g Gomega) {
		var s corev1.Secret
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: etOperatorNS, Name: etClientsSecret}, &s)).To(Succeed())
	}).Should(Succeed())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(etDone, 30*time.Second).Should(BeClosed())
	etAPI.Close()
	etSink.srv.Close()
	Expect(testEnv.Stop()).To(Succeed())
	Expect(os.RemoveAll(etDataDir)).To(Succeed())
})

// etNotFound reports whether err is a not-found API error.
func etNotFound(err error) bool { return apierrors.IsNotFound(err) }

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
