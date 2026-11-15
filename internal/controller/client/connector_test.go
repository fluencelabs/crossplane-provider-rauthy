package client_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

type noopTracker struct{ tracked int }

func (n *noopTracker) Track(context.Context, resource.ModernManaged) error {
	n.tracked++
	return nil
}

func providerConfig(url string, issuer *string) *v1alpha1.ClusterProviderConfig {
	return &v1alpha1.ClusterProviderConfig{
		Name: "default",
		Spec: v1alpha1.ClusterProviderConfigSpec{
			URL:       url,
			IssuerURL: issuer,
			Credentials: v1alpha1.ProviderCredentials{
				Source: xpv2.CredentialsSourceSecret,
				SecretRef: xpv2.SecretKeySelector{
					SecretReference: xpv2.SecretReference{Namespace: "crossplane-system", Name: "rauthy"},
					Key:             "api-key",
				},
			},
		},
	}
}

func apiKeySecret() *corev1.Secret {
	return &corev1.Secret{
		Namespace: "crossplane-system",
		Name:      "rauthy",
		Data:      map[string][]byte{"api-key": []byte("crossplane$abc123\n")},
	}
}

func withConfigRef(kind string) func(*v1alpha1.Client) {
	return func(cr *v1alpha1.Client) {
		cr.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: kind, Name: "default"}
	}
}

func TestConnect(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(existing(false))

	cases := map[string]struct {
		reason     string
		objects    []runtime.Object
		cr         *v1alpha1.Client
		wantErr    error
		wantIssuer string
	}{
		"DefaultIssuer": {
			reason:     "The issuer defaults to the Rauthy URL plus /auth/v1, and the trimmed key is sent.",
			objects:    []runtime.Object{providerConfig(f.srv.URL+"/", nil), apiKeySecret()},
			cr:         newClient(owned, public, withConfigRef("ClusterProviderConfig")),
			wantIssuer: f.srv.URL + "/auth/v1",
		},
		"ExplicitIssuer": {
			reason:     "An explicit issuerURL is published as is.",
			objects:    []runtime.Object{providerConfig(f.srv.URL, new(testIssuer)), apiKeySecret()},
			cr:         newClient(owned, public, withConfigRef("ClusterProviderConfig")),
			wantIssuer: testIssuer,
		},
		"NamespacedKind": {
			reason:  "There is no namespaced ProviderConfig.",
			objects: []runtime.Object{providerConfig(f.srv.URL, nil), apiKeySecret()},
			cr:      newClient(withConfigRef("ProviderConfig")),
			wantErr: errors.New("unsupported provider config kind"),
		},
		"NoRef": {
			reason:  "A resource without a providerConfigRef cannot connect.",
			cr:      newClient(),
			wantErr: errors.New("no providerConfigRef"),
		},
		"MissingConfig": {
			reason:  "A missing ClusterProviderConfig is reported.",
			cr:      newClient(withConfigRef("ClusterProviderConfig")),
			wantErr: errors.New("cannot get ClusterProviderConfig"),
		},
		"MissingSecret": {
			reason:  "A missing API key Secret is reported.",
			objects: []runtime.Object{providerConfig(f.srv.URL, nil)},
			cr:      newClient(withConfigRef("ClusterProviderConfig")),
			wantErr: errors.New("cannot read the Rauthy API key"),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			connectAndObserve(t, f, tc.reason, tc.objects, tc.cr, tc.wantErr, tc.wantIssuer)
		})
	}
}

func connectAndObserve(
	t *testing.T,
	f *fakeRauthy,
	reason string,
	objects []runtime.Object,
	cr *v1alpha1.Client,
	wantErr error,
	wantIssuer string,
) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objects...).Build()
	tracker := &noopTracker{}
	c := &client.Connector{Kube: kube, Usage: tracker}

	e, err := c.Connect(context.Background(), cr)
	checkErr(t, reason, err, wantErr, nil)
	if wantErr != nil {
		return
	}

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("%s\nObserve: %v", reason, err)
	}
	if got := string(obs.ConnectionDetails[client.ConnectionKeyIssuer]); got != wantIssuer {
		t.Errorf("%s\nissuer = %q, want %q", reason, got, wantIssuer)
	}
	if got := f.lastAuthorization(); got != "API-Key crossplane$abc123" {
		t.Errorf("%s\nAuthorization header = %q, want the trimmed API key", reason, got)
	}
	if tracker.tracked != 1 {
		t.Errorf("%s\nusage tracked %d times, want 1", reason, tracker.tracked)
	}
}
