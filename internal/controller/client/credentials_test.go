package client_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// TestConnectReadsKeyWithoutCache: the API key Secret is read through the
// uncached reader. A read of it through the cached client would make the
// manager start a cluster-wide Secret informer.
func TestConnectReadsKeyWithoutCache(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cr := newClient(withConfigRef("ClusterProviderConfig"))
	cached := fake.NewClientBuilder().WithScheme(s).
		WithObjects(providerConfig(f.srv.URL, nil), apiKeySecret(), cr.DeepCopy()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c kube.WithWatch, key kube.ObjectKey, obj kube.Object,
				opts ...kube.GetOption,
			) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return errors.New("a Secret was read through the cache")
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	uncached := fake.NewClientBuilder().WithScheme(s).WithObjects(apiKeySecret(), cr.DeepCopy()).Build()

	c := &client.Connector{Kube: cached, Reader: uncached, Usage: &noopTracker{}}
	if _, err := c.Connect(context.Background(), cr); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}
