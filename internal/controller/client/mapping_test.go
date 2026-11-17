package client //nolint:testpackage // Exercise the unexported issuer mapping directly.

import (
	"slices"
	"strings"
	"testing"

	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

func TestIssuerFor(t *testing.T) {
	explicit := "https://public.example.com/custom"
	empty := ""
	for _, tt := range []struct {
		name   string
		url    string
		issuer *string
		want   string
	}{
		{"base", "https://auth.example.com", nil, "https://auth.example.com/auth/v1/"},
		{"base slash", "https://auth.example.com/", nil, "https://auth.example.com/auth/v1/"},
		{"API path", "https://auth.example.com/auth/v1", nil, "https://auth.example.com/auth/v1/"},
		{"API path slash", "https://auth.example.com/auth/v1/", nil, "https://auth.example.com/auth/v1/"},
		{"explicit", "https://auth.example.com", &explicit, explicit},
		{"empty override", "https://auth.example.com", &empty, "https://auth.example.com/auth/v1/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := issuerFor(tt.url, tt.issuer); got != tt.want {
				t.Fatalf("issuerFor() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizedScopesAndRedirects(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    v1alpha1.ClientParameters
		obs  rauthy.ClientResponse
		want []string
	}{
		{name: "scopes add openid", p: v1alpha1.ClientParameters{Scopes: []string{"email"}, DefaultScopes: []string{"profile"}}, obs: rauthy.ClientResponse{Scopes: []string{"openid", "email"}, DefaultScopes: []string{"profile", "openid"}}},
		{name: "custom openid scope", p: v1alpha1.ClientParameters{Scopes: []string{"myopenid"}}, obs: rauthy.ClientResponse{Scopes: []string{"myopenid"}}},
		{name: "real scope drift", p: v1alpha1.ClientParameters{Scopes: []string{"email"}}, obs: rauthy.ClientResponse{Scopes: []string{"openid", "profile"}}, want: []string{"scopes"}},
		{name: "empty post logout entries", p: v1alpha1.ClientParameters{PostLogoutRedirectURIs: []string{"https://app.example/c", ""}}, obs: rauthy.ClientResponse{PostLogoutRedirectURIs: []string{"https://app.example/c"}}},
		{name: "empty contacts", p: v1alpha1.ClientParameters{Contacts: []string{""}}, obs: rauthy.ClientResponse{}},
		{name: "URI comma storage", p: v1alpha1.ClientParameters{RedirectURIs: []string{"https://app.example/a,b"}, PostLogoutRedirectURIs: []string{"https://app.example/c,d"}}, obs: rauthy.ClientResponse{RedirectURIs: []string{"https://app.example/a", "b"}, PostLogoutRedirectURIs: []string{"https://app.example/c", "d"}}, want: []string{"redirectUris", "postLogoutRedirectUris"}},
		{name: "contacts drift", p: v1alpha1.ClientParameters{Contacts: []string{"ops@example.com"}}, obs: rauthy.ClientResponse{Contacts: []string{"other@example.com"}}, want: []string{"contacts"}},
		{name: "post logout drift", p: v1alpha1.ClientParameters{PostLogoutRedirectURIs: []string{"https://app.example/c"}}, obs: rauthy.ClientResponse{PostLogoutRedirectURIs: []string{"https://other.example/c"}}, want: []string{"postLogoutRedirectUris"}},
		{name: "default scopes drift", p: v1alpha1.ClientParameters{DefaultScopes: []string{"email"}}, obs: rauthy.ClientResponse{DefaultScopes: []string{"openid", "profile"}}, want: []string{"defaultScopes"}},
		{name: "clear contacts", p: v1alpha1.ClientParameters{Contacts: []string{}}, obs: rauthy.ClientResponse{Contacts: []string{"ops@example.com"}}, want: []string{"contacts"}},
		{name: "clear post logout", p: v1alpha1.ClientParameters{PostLogoutRedirectURIs: []string{}}, obs: rauthy.ClientResponse{PostLogoutRedirectURIs: []string{"https://app.example/c"}}, want: []string{"postLogoutRedirectUris"}},
		{name: "unset lists", obs: rauthy.ClientResponse{Contacts: []string{"ops@example.com"}, PostLogoutRedirectURIs: []string{"https://app.example/c"}, DefaultScopes: []string{"openid", "profile"}}},
		{name: "list order", p: v1alpha1.ClientParameters{RedirectURIs: []string{"https://app.example/a", "https://app.example/b"}, Contacts: []string{"a@example.com", "b@example.com"}, PostLogoutRedirectURIs: []string{"https://app.example/a", "https://app.example/b"}}, obs: rauthy.ClientResponse{RedirectURIs: []string{"https://app.example/b", "https://app.example/a"}, Contacts: []string{"b@example.com", "a@example.com"}, PostLogoutRedirectURIs: []string{"https://app.example/b", "https://app.example/a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := differences(&tc.p, &tc.obs); !slices.Equal(got, tc.want) {
				t.Fatalf("differences = %v, want %v", got, tc.want)
			}
		})
	}
}

// Rauthy stores scopes as one CSV and splits it on read, so a scope name
// with a comma comes back as two. That must still count as converged.
func TestScopesWithCommaConverge(t *testing.T) {
	t.Parallel()
	p := v1alpha1.ClientParameters{Scopes: []string{"team,read"}, DefaultScopes: []string{"team,read"}}
	observed := rauthy.ClientResponse{
		Scopes:        []string{"openid", "team", "read"},
		DefaultScopes: []string{"openid", "team", "read"},
	}
	if diff := differences(&p, &observed); len(diff) > 0 {
		t.Fatalf("differences = %v, want none", diff)
	}
}

// The backend fingerprint publishes no part of the URL, and userinfo does
// not change which instance it names.
func TestBackendIdentityPublishesNoURL(t *testing.T) {
	t.Parallel()
	a, err := backendIdentity("https://operator:syntheticpassword@auth.example.com/syntheticprefix/auth/v1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := backendIdentity("https://auth.example.com/syntheticprefix/auth/v1")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("userinfo changed the identity: %q != %q", a, b)
	}
	for _, part := range []string{"operator", "syntheticpassword", "auth.example.com", "syntheticprefix"} {
		if strings.Contains(a, part) {
			t.Errorf("identity %q contains %q", a, part)
		}
	}
}
