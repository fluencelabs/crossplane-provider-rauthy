package client //nolint:testpackage // Exercise the unexported issuer mapping directly.

import (
	"reflect"
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

func TestBackendIdentityDistinguishesAPIRootPaths(t *testing.T) {
	t.Parallel()
	for _, paths := range [][2]string{
		{"/a/auth/v1", "/b/auth/v1"},
		{"/auth/v1", "/nested/auth/v1"},
		{"/a%2Fb/auth/v1", "/a/b/auth/v1"},
	} {
		t.Run(paths[0]+" vs "+paths[1], func(t *testing.T) {
			t.Parallel()
			a, err := backendIdentity("https://auth.example.com" + paths[0])
			if err != nil {
				t.Fatal(err)
			}
			b, err := backendIdentity("https://auth.example.com" + paths[1])
			if err != nil {
				t.Fatal(err)
			}
			if a == b {
				t.Fatalf("different API root paths share backend identity %q", a)
			}
		})
	}
}

// fullSpec sets every field of ClientParameters, each to a value other than
// the one fullObserved holds.
func fullSpec() v1alpha1.ClientParameters {
	return v1alpha1.ClientParameters{
		Name:                   new("Spec App"),
		Confidential:           true,
		Enabled:                new(false),
		RedirectURIs:           []string{"https://spec.example.com/cb"},
		PostLogoutRedirectURIs: []string{"https://spec.example.com/out"},
		AllowedOrigins:         []string{"https://spec.example.com"},
		FlowsEnabled:           []string{"client_credentials"},
		AccessTokenAlg:         new("RS384"),
		IDTokenAlg:             new("RS512"),
		AuthCodeLifetime:       new(int64(120)),
		AccessTokenLifetime:    new(int64(900)),
		Scopes:                 []string{"openid", "profile"},
		DefaultScopes:          []string{"openid", "groups"},
		Challenges:             []string{"plain", "S256"},
		ForceMFA:               new(true),
		ClientURI:              new("https://spec.example.com"),
		Contacts:               []string{"spec@example.com"},
		BackchannelLogoutURI:   new("https://spec.example.com/bcl"),
		RestrictGroupPrefix:    new("spec-"),
	}
}

func fullObserved() rauthy.ClientResponse {
	return rauthy.ClientResponse{
		ID:                     "app",
		Name:                   new("Stored App"),
		Enabled:                true,
		Confidential:           false,
		RedirectURIs:           []string{"https://stored.example.com/cb"},
		PostLogoutRedirectURIs: []string{"https://stored.example.com/out"},
		AllowedOrigins:         []string{"https://stored.example.com"},
		FlowsEnabled:           []string{"authorization_code"},
		AccessTokenAlg:         "EdDSA",
		IDTokenAlg:             "EdDSA",
		AuthCodeLifetime:       60,
		AccessTokenLifetime:    1800,
		Scopes:                 []string{"openid", "email"},
		DefaultScopes:          []string{"openid"},
		Challenges:             []string{"S256"},
		ForceMFA:               false,
		ClientURI:              new("https://stored.example.com"),
		Contacts:               []string{"stored@example.com"},
		BackchannelLogoutURI:   new("https://stored.example.com/bcl"),
		RestrictGroupPrefix:    new("stored-"),
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Every field of ClientParameters the spec sets must reach the PUT body; a
// field dropped from updateRequest would keep the stored value instead.
func TestUpdateRequestSendsEverySpecField(t *testing.T) {
	t.Parallel()
	p, obs := fullSpec(), fullObserved()
	req := updateRequest("app", &p, &obs)

	for _, f := range []struct {
		field     string
		got, want any
	}{
		{"name", deref(req.Name), "Spec App"},
		{"confidential", req.Confidential, true},
		{"enabled", req.Enabled, false},
		{"redirectUris", req.RedirectURIs, p.RedirectURIs},
		{"postLogoutRedirectUris", req.PostLogoutRedirectURIs, p.PostLogoutRedirectURIs},
		{"allowedOrigins", req.AllowedOrigins, p.AllowedOrigins},
		{"flowsEnabled", req.FlowsEnabled, p.FlowsEnabled},
		{"accessTokenAlg", req.AccessTokenAlg, "RS384"},
		{"idTokenAlg", req.IDTokenAlg, "RS512"},
		{"authCodeLifetime", req.AuthCodeLifetime, int64(120)},
		{"accessTokenLifetime", req.AccessTokenLifetime, int64(900)},
		{"scopes", req.Scopes, p.Scopes},
		{"defaultScopes", req.DefaultScopes, p.DefaultScopes},
		{"challenges", req.Challenges, p.Challenges},
		{"forceMfa", req.ForceMFA, true},
		{"clientUri", deref(req.ClientURI), "https://spec.example.com"},
		{"contacts", req.Contacts, p.Contacts},
		{"backchannelLogoutUri", deref(req.BackchannelLogoutURI), "https://spec.example.com/bcl"},
		{"restrictGroupPrefix", deref(req.RestrictGroupPrefix), "spec-"},
	} {
		if !reflect.DeepEqual(f.got, f.want) {
			t.Errorf("PUT %s = %v, want the spec's %v", f.field, f.got, f.want)
		}
	}
}

// Every compared field must report drift on its own: starting from a spec
// that matches the stored client, changing one field names exactly that field.
func TestDifferencesNamesEachField(t *testing.T) {
	t.Parallel()
	obs := fullObserved()
	matching := func() v1alpha1.ClientParameters {
		return v1alpha1.ClientParameters{
			Name:                   new("Stored App"),
			Confidential:           false,
			Enabled:                new(true),
			RedirectURIs:           []string{"https://stored.example.com/cb"},
			PostLogoutRedirectURIs: []string{"https://stored.example.com/out"},
			AllowedOrigins:         []string{"https://stored.example.com"},
			FlowsEnabled:           []string{"authorization_code"},
			AccessTokenAlg:         new("EdDSA"),
			IDTokenAlg:             new("EdDSA"),
			AuthCodeLifetime:       new(int64(60)),
			AccessTokenLifetime:    new(int64(1800)),
			Scopes:                 []string{"openid", "email"},
			DefaultScopes:          []string{"openid"},
			Challenges:             []string{"S256"},
			ForceMFA:               new(false),
			ClientURI:              new("https://stored.example.com"),
			Contacts:               []string{"stored@example.com"},
			BackchannelLogoutURI:   new("https://stored.example.com/bcl"),
			RestrictGroupPrefix:    new("stored-"),
		}
	}
	base := matching()
	if diff := differences(&base, &obs); len(diff) > 0 {
		t.Fatalf("differences of the matching spec = %v, want none", diff)
	}

	spec := fullSpec()
	for field, change := range map[string]func(*v1alpha1.ClientParameters){
		"name":                   func(p *v1alpha1.ClientParameters) { p.Name = spec.Name },
		"confidential":           func(p *v1alpha1.ClientParameters) { p.Confidential = spec.Confidential },
		"enabled":                func(p *v1alpha1.ClientParameters) { p.Enabled = spec.Enabled },
		"redirectUris":           func(p *v1alpha1.ClientParameters) { p.RedirectURIs = spec.RedirectURIs },
		"postLogoutRedirectUris": func(p *v1alpha1.ClientParameters) { p.PostLogoutRedirectURIs = spec.PostLogoutRedirectURIs },
		"allowedOrigins":         func(p *v1alpha1.ClientParameters) { p.AllowedOrigins = spec.AllowedOrigins },
		"flowsEnabled":           func(p *v1alpha1.ClientParameters) { p.FlowsEnabled = spec.FlowsEnabled },
		"accessTokenAlg":         func(p *v1alpha1.ClientParameters) { p.AccessTokenAlg = spec.AccessTokenAlg },
		"idTokenAlg":             func(p *v1alpha1.ClientParameters) { p.IDTokenAlg = spec.IDTokenAlg },
		"authCodeLifetime":       func(p *v1alpha1.ClientParameters) { p.AuthCodeLifetime = spec.AuthCodeLifetime },
		"accessTokenLifetime":    func(p *v1alpha1.ClientParameters) { p.AccessTokenLifetime = spec.AccessTokenLifetime },
		"scopes":                 func(p *v1alpha1.ClientParameters) { p.Scopes = spec.Scopes },
		"defaultScopes":          func(p *v1alpha1.ClientParameters) { p.DefaultScopes = spec.DefaultScopes },
		"challenges":             func(p *v1alpha1.ClientParameters) { p.Challenges = spec.Challenges },
		"forceMfa":               func(p *v1alpha1.ClientParameters) { p.ForceMFA = spec.ForceMFA },
		"clientUri":              func(p *v1alpha1.ClientParameters) { p.ClientURI = spec.ClientURI },
		"contacts":               func(p *v1alpha1.ClientParameters) { p.Contacts = spec.Contacts },
		"backchannelLogoutUri":   func(p *v1alpha1.ClientParameters) { p.BackchannelLogoutURI = spec.BackchannelLogoutURI },
		"restrictGroupPrefix":    func(p *v1alpha1.ClientParameters) { p.RestrictGroupPrefix = spec.RestrictGroupPrefix },
	} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			p := matching()
			change(&p)
			if got := differences(&p, &obs); !slices.Equal(got, []string{field}) {
				t.Errorf("differences = %v, want [%s]", got, field)
			}
		})
	}
}
