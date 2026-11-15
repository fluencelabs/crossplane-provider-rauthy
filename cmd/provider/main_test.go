package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// spyCache stands in for the manager's informer cache. Any read through it
// would start an informer for the kind read.
type spyCache struct{ reads atomic.Int32 }

func (s *spyCache) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	s.reads.Add(1)
	return nil
}

func (s *spyCache) List(context.Context, client.ObjectList, ...client.ListOption) error {
	s.reads.Add(1)
	return nil
}

// TestSecretsBypassTheCache: the manager's client, built with the provider's
// client options the way controller-runtime builds it (the informer cache as
// its reader), reads Secrets from the API server, never from the cache. The
// connection Secrets crossplane-runtime publishes are read through this
// client, so no Secret informer is ever started.
func TestSecretsBypassTheCache(t *testing.T) {
	t.Parallel()
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/apps/secrets/app-oidc" {
			http.NotFound(w, r)
			return
		}
		served.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.Secret{
			APIVersion: "v1",
			Kind:       "Secret",
			Namespace:  "apps",
			Name:       "app-oidc",
		})
	}))
	t.Cleanup(srv.Close)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)

	spy := &spyCache{}
	opts := clientOptions()
	opts.Scheme = scheme
	opts.Mapper = mapper
	if opts.Cache == nil {
		opts.Cache = &client.CacheOptions{}
	}
	opts.Cache.Reader = spy
	c, err := client.New(&rest.Config{Host: srv.URL}, opts)
	if err != nil {
		t.Fatal(err)
	}

	err = c.Get(context.Background(), client.ObjectKey{Namespace: "apps", Name: "app-oidc"}, &corev1.Secret{})
	if err != nil {
		t.Fatalf("Get the Secret: %v", err)
	}
	if n := spy.reads.Load(); n != 0 {
		t.Errorf("%d Secret reads went through the cache, want none", n)
	}
	if n := served.Load(); n != 1 {
		t.Errorf("the API server answered %d Secret reads, want 1", n)
	}
}
