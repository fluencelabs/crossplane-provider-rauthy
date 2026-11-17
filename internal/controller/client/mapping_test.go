package client //nolint:testpackage // Exercise the unexported issuer mapping directly.

import (
	"encoding/json"
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

// Each optional field preserves an absent desired value, applies a populated
// value, and handles an explicit empty value according to its field contract.
func TestUpdateRequestOptionalStates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field                               string
		observed, desired, empty, wantEmpty any
	}{
		{"Name", new("Stored App"), new("Spec App"), new(""), (*string)(nil)},
		{"PostLogoutRedirectURIs", []string{"https://stored.example/out"}, []string{"https://spec.example/out"}, []string{}, []string{}},
		{"AllowedOrigins", []string{"https://stored.example"}, []string{"https://spec.example"}, []string{}, []string{}},
		{"FlowsEnabled", []string{"authorization_code"}, []string{"client_credentials"}, []string{}, []string{}},
		{"AccessTokenAlg", "EdDSA", new("RS384"), new(""), ""},
		{"IDTokenAlg", "EdDSA", new("RS512"), new(""), ""},
		{"AuthCodeLifetime", int64(60), new(int64(120)), new(int64(0)), int64(0)},
		{"AccessTokenLifetime", int64(1800), new(int64(900)), new(int64(0)), int64(0)},
		{"Scopes", []string{"openid", "email"}, []string{"openid", "profile"}, []string{}, []string{"openid", "email"}},
		{"DefaultScopes", []string{"openid", "email"}, []string{"openid", "profile"}, []string{}, []string{"openid", "email"}},
		{"Challenges", []string{"S256"}, []string{"plain", "S256"}, []string{}, []string{}},
		{"ClientURI", new("https://stored.example"), new("https://spec.example"), new(""), (*string)(nil)},
		{"Contacts", []string{"stored@example.com"}, []string{"spec@example.com"}, []string{}, []string{}},
		{"BackchannelLogoutURI", new("https://stored.example/bcl"), new("https://spec.example/bcl"), new(""), (*string)(nil)},
		{"RestrictGroupPrefix", new("stored-"), new("spec-"), new(""), (*string)(nil)},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			populated := reflect.ValueOf(tc.desired)
			if populated.Kind() == reflect.Pointer && reflect.TypeOf(tc.observed).Kind() != reflect.Pointer {
				populated = populated.Elem()
			}
			for _, state := range []struct {
				name          string
				desired, want any
			}{
				{"absent", nil, tc.observed},
				{"populated", tc.desired, populated.Interface()},
				{"empty", tc.empty, tc.wantEmpty},
			} {
				t.Run(state.name, func(t *testing.T) {
					t.Parallel()
					var p v1alpha1.ClientParameters
					var obs rauthy.ClientResponse
					reflect.ValueOf(&obs).Elem().FieldByName(tc.field).Set(reflect.ValueOf(tc.observed))
					if state.desired != nil {
						reflect.ValueOf(&p).Elem().FieldByName(tc.field).Set(reflect.ValueOf(state.desired))
					}
					req := updateRequest("app", &p, &obs)
					got := reflect.ValueOf(req).FieldByName(tc.field).Interface()
					if !reflect.DeepEqual(got, state.want) {
						t.Errorf("PUT %s = %v, want %v", tc.field, got, state.want)
					}
				})
			}
		})
	}
}

func TestUpdateRequestBooleanStates(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"Enabled", "ForceMFA"} {
		for _, state := range []struct {
			name     string
			observed bool
			desired  *bool
			want     bool
		}{
			{"absent-false", false, nil, false},
			{"absent-true", true, nil, true},
			{"enable", false, new(true), true},
			{"disable", true, new(false), false},
			{"remain-enabled", true, new(true), true},
			{"remain-disabled", false, new(false), false},
		} {
			t.Run(field+"/"+state.name, func(t *testing.T) {
				t.Parallel()
				var p v1alpha1.ClientParameters
				var obs rauthy.ClientResponse
				reflect.ValueOf(&obs).Elem().FieldByName(field).SetBool(state.observed)
				reflect.ValueOf(&p).Elem().FieldByName(field).Set(reflect.ValueOf(state.desired))
				req := updateRequest("app", &p, &obs)
				if got := reflect.ValueOf(req).FieldByName(field).Bool(); got != state.want {
					t.Errorf("PUT %s = %v, want %v", field, got, state.want)
				}
			})
		}
	}
}

func TestUpdateRequestRequiredFields(t *testing.T) {
	t.Parallel()
	for _, confidential := range []bool{false, true} {
		for _, redirects := range [][]string{nil, {}, {"https://spec.example/cb"}} {
			p := v1alpha1.ClientParameters{Confidential: confidential, RedirectURIs: redirects}
			obs := fullObserved()
			obs.Confidential = !confidential
			req := updateRequest("desired-id", &p, &obs)
			if req.ID != "desired-id" || !reflect.DeepEqual(req.RedirectURIs, redirects) ||
				req.Confidential != confidential {
				t.Errorf("PUT ID = %q, redirects = %v, confidential = %v, want desired-id, %v, %v",
					req.ID, req.RedirectURIs, req.Confidential, redirects, confidential)
			}
		}
	}
}

// Settings outside ClientParameters always retain the observed value.
func TestUpdateRequestUnmanagedFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field  string
		values []any
	}{
		{"Scim", []any{(*rauthy.ScimClientRequestResponse)(nil), &rauthy.ScimClientRequestResponse{
			BaseURI: "https://scim.example", BearerToken: rauthy.NewSecret("synthetic-token"),
			SyncGroups: true, GroupSyncPrefix: new("team-"),
		}}},
		{"AllowedResources", []any{[]string(nil), []string{}, []string{"https://api.example"}}},
		{"DefaultAud", []any{[]string(nil), []string{}, []string{"audience"}}},
		{"Claims", []any{json.RawMessage(nil), json.RawMessage(`{}`), json.RawMessage(`{"team":"ops"}`)}},
		{"ClaimsAtRoot", []any{false, true}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			for _, want := range tc.values {
				var obs rauthy.ClientResponse
				reflect.ValueOf(&obs).Elem().FieldByName(tc.field).Set(reflect.ValueOf(want))
				p := fullSpec()
				req := updateRequest("app", &p, &obs)
				if got := reflect.ValueOf(req).FieldByName(tc.field).Interface(); !reflect.DeepEqual(got, want) {
					t.Errorf("PUT %s = %v, want observed %v", tc.field, got, want)
				}
			}
		})
	}
}
