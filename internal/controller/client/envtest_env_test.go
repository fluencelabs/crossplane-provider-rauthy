//go:build envtest

package client_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// These tests run against a real kube-apiserver and etcd started by envtest,
// with the CRDs from package/crds installed, and the in-memory fake Rauthy.
// They check what controller-runtime's fake client cannot show: pruning,
// admission, the status subresource, field indexes in a real cache and
// resourceVersion conflicts. Run them with `make test.envtest`.

// crdDir holds the CRDs the provider package installs.
func crdDir() string { return filepath.Join("..", "..", "..", "package", "crds") }

//nolint:gochecknoglobals // TestMain starts one API server for the whole package.
var (
	envCfg  *rest.Config
	envKube kube.Client
	envSeq  atomic.Int32
)

func TestMain(m *testing.M) {
	logf.SetLogger(logr.Discard())
	env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir()}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest (is KUBEBUILDER_ASSETS set? see `make test.envtest`): %v\n", err)
		os.Exit(1)
	}
	envCfg = cfg
	if envKube, err = kube.New(cfg, kube.Options{Scheme: envScheme()}); err != nil {
		fmt.Fprintf(os.Stderr, "build the API client: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err = env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

func envScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// envSetup is one test's share of the cluster: its own namespace, a
// ClusterProviderConfig named after it that points at its own fake Rauthy,
// and its own client id (also the namespace's name). Ids are global, so tests
// sharing one would refuse each other's Clients.
type envSetup struct {
	ns, pc, id string
	f          *fakeRauthy
}

func newEnvSetup(t *testing.T, kc kube.Client) *envSetup {
	t.Helper()
	ctx := context.Background()
	n := envSeq.Add(1)
	s := &envSetup{
		ns: fmt.Sprintf("t%d-%s", n, strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))),
		f:  newFakeRauthy(t),
	}
	if len(s.ns) > 63 {
		s.ns = s.ns[:63]
	}
	s.ns = strings.TrimRight(s.ns, "-")
	s.pc, s.id = s.ns, s.ns
	mustCreate(t, kc, &corev1.Namespace{Name: s.ns})
	key := apiKeySecret()
	key.Namespace, key.Name = s.ns, "rauthy-api-key"
	mustCreate(t, kc, key)
	pc := providerConfig(s.f.srv.URL, new(testIssuer))
	pc.Name = s.pc
	pc.Spec.Credentials.SecretRef.Namespace, pc.Spec.Credentials.SecretRef.Name = s.ns, key.Name
	mustCreate(t, kc, pc)
	t.Cleanup(func() { _ = kc.Delete(ctx, pc) })
	return s
}

// client returns a Client named name in the setup's namespace for the
// setup's client id, built like newClient and pointing at the setup's
// provider config.
func (s *envSetup) client(name string, mods ...func(*v1alpha1.Client)) *v1alpha1.Client {
	cr := newClient(mods...)
	cr.Name, cr.Namespace = name, s.ns
	meta.SetExternalName(cr, s.id)
	cr.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: s.pc}
	cr.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: name + "-oidc"}
	return cr
}

// stored returns the setup's client as the fake Rauthy holds it, or nil.
func (s *envSetup) stored() *rauthy.ClientResponse {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.clients[s.id]
}

// secret is the secret the fake Rauthy issued for the setup's client.
func (s *envSetup) secret() string {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return s.f.secrets[s.id]
}

func mustCreate(t *testing.T, kc kube.Client, o kube.Object) {
	t.Helper()
	if err := kc.Create(context.Background(), o); err != nil {
		t.Fatalf("create %T %s: %v", o, o.GetName(), err)
	}
}

// startManager starts a manager on the test cluster whose cache indexes
// Clients by ClientIDIndex, as Setup does, and keeps Secrets out of the cache,
// as the provider does. A namespace, when given, limits the cache (and so any
// controller) to it. setup, when set, runs before the manager starts.
func startManager(t *testing.T, ns string, setup func(ctrl.Manager)) ctrl.Manager {
	t.Helper()
	skip := true
	var cacheOpts cache.Options
	if ns != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{ns: {}}
	}
	mgr, err := ctrl.NewManager(envCfg, ctrl.Options{
		Cache:                  cacheOpts,
		Scheme:                 envScheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             config.Controller{SkipNameValidation: &skip},
		Client: kube.Options{
			Cache: &kube.CacheOptions{DisableFor: []kube.Object{&corev1.Secret{}}},
		},
	})
	if err != nil {
		t.Fatalf("build the manager: %v", err)
	}
	if setup != nil {
		setup(mgr)
	} else if err = mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Client{},
		client.ClientIDIndex, client.IndexClientID); err != nil {
		t.Fatalf("index Clients: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if startErr := mgr.Start(ctx); startErr != nil {
			t.Errorf("manager stopped: %v", startErr)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("the manager's cache did not sync")
	}
	return mgr
}

// eventually polls cond every 100ms for up to 30s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stored reads cr from the API server; nil once it is gone.
func stored(t *testing.T, cr *v1alpha1.Client) *v1alpha1.Client {
	t.Helper()
	return storedIn(t, envKube, cr)
}

// storedIn reads cr through kc; nil once it is gone. The kind is set as the
// manager's cache sets it, and the runtime relies on it (the provider config
// usage names its owner by it); a direct typed read leaves it empty.
func storedIn(t *testing.T, kc kube.Client, cr *v1alpha1.Client) *v1alpha1.Client {
	t.Helper()
	got := &v1alpha1.Client{}
	if err := kc.Get(context.Background(), kube.ObjectKeyFromObject(cr), got); err != nil {
		if kube.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatalf("get the Client: %v", err)
	}
	got.SetGroupVersionKind(v1alpha1.ClientGroupVersionKind)
	return got
}
