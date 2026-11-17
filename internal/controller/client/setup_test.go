package client //nolint:testpackage // Inspect the HTTP client wired by Connector.Connect.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

type timeoutTestUsageTracker struct{}

func (timeoutTestUsageTracker) Track(context.Context, resource.ModernManaged) error {
	return nil
}

func TestConnectorConfiguresHTTPTimeout(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	config := &v1alpha1.ClusterProviderConfig{
		Name: "rauthy",
		Spec: v1alpha1.ClusterProviderConfigSpec{
			URL: "https://rauthy.example",
			Credentials: v1alpha1.ProviderCredentials{
				Source: xpv2.CredentialsSourceSecret,
				SecretRef: xpv2.SecretKeySelector{
					SecretReference: xpv2.SecretReference{Namespace: "provider", Name: "rauthy"},
					Key:             "api-key",
				},
			},
		},
	}
	secret := &corev1.Secret{
		Namespace: "provider",
		Name:      "rauthy",
		Data:      map[string][]byte{"api-key": []byte("synthetic")},
	}
	cr := &v1alpha1.Client{}
	cr.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{
		Kind: v1alpha1.ClusterProviderConfigKind,
		Name: config.Name,
	}
	connector := &Connector{
		Kube:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(config, secret).Build(),
		Usage: timeoutTestUsageTracker{},
	}
	connected, err := connector.Connect(t.Context(), cr)
	if err != nil {
		t.Fatal(err)
	}
	sdk := connected.(*external).api.(*rauthy.Client)
	// The SDK keeps its HTTP client private. Read only the integer Timeout
	// field so the test measures the wired configuration without a request.
	httpClient := reflect.ValueOf(sdk).Elem().FieldByName("httpClient")
	if !httpClient.IsValid() || httpClient.IsNil() {
		t.Fatal("cannot inspect the Rauthy SDK's HTTP client")
	}
	timeout := time.Duration(httpClient.Elem().FieldByName("Timeout").Int())
	const wantTimeout, minQueueAllowance = 30 * time.Second, 30 * time.Second
	if timeout != wantTimeout {
		t.Errorf("HTTP timeout = %s, want %s", timeout, wantTimeout)
	}
	if window := rauthyHTTPTimeout + creationQueueAllowance; window < timeout+minQueueAllowance {
		t.Errorf("pending creation window = %s, want at least %s", window, timeout+minQueueAllowance)
	}
}
