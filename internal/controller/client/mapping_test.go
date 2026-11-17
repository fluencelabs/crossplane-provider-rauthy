package client //nolint:testpackage // Exercise the unexported issuer mapping directly.

import (
	"slices"
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
		{name: "empty post logout entries", p: v1alpha1.ClientParameters{PostLogoutRedirectURIs: []string{"https://app.example/c,"}}, obs: rauthy.ClientResponse{PostLogoutRedirectURIs: []string{"https://app.example/c"}}},
		{name: "empty contacts", p: v1alpha1.ClientParameters{Contacts: []string{""}}, obs: rauthy.ClientResponse{}},
		{name: "URI comma storage", p: v1alpha1.ClientParameters{RedirectURIs: []string{"https://app.example/a,b"}, PostLogoutRedirectURIs: []string{"https://app.example/c,d"}}, obs: rauthy.ClientResponse{RedirectURIs: []string{"https://app.example/a", "b"}, PostLogoutRedirectURIs: []string{"https://app.example/c", "d"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := differences(&tc.p, &tc.obs); !slices.Equal(got, tc.want) {
				t.Fatalf("differences = %v, want %v", got, tc.want)
			}
		})
	}
}
