package client_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// requireScopeArrays fails unless the last PUT carried both scope lists as
// JSON arrays: Rauthy answers null with a 400.
func requireScopeArrays(t *testing.T, f *fakeRauthy) {
	t.Helper()
	for _, field := range []string{"scopes", "default_scopes"} {
		raw := f.lastPutField(t, field)
		var got []string
		if err := json.Unmarshal(raw, &got); err != nil || got == nil {
			t.Fatalf("PUT %s = %s, want a JSON array", field, raw)
		}
	}
}

// updateScopesCase is one case of TestUpdateSendsScopesAsArrays.
type updateScopesCase struct {
	observed    func(*rauthy.ClientResponse)
	spec        func(*v1alpha1.ClientParameters)
	wantScopes  []string
	wantDefault []string
}

func (tc updateScopesCase) run(t *testing.T) {
	t.Helper()
	f := newFakeRauthy(t)
	c := existing(true)
	if tc.observed != nil {
		tc.observed(c)
	}
	f.put(c)
	cr := newClient(owned, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Enabled = new(false)
		if tc.spec != nil {
			tc.spec(&cr.Spec.ForProvider)
		}
	})
	e := newExternal(t, f, cr)

	if _, err := e.Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}
	requireScopeArrays(t, f)
	got := f.stored()
	if !sameSet(got.Scopes, tc.wantScopes) || !sameSet(got.DefaultScopes, tc.wantDefault) {
		t.Errorf("Rauthy holds scopes %v, default scopes %v; want %v, %v",
			got.Scopes, got.DefaultScopes, tc.wantScopes, tc.wantDefault)
	}
	obs, err := e.Observe(context.Background(), cr)
	if err != nil || !obs.ResourceUpToDate {
		t.Fatalf("Observe after the PUT = %+v, %v; want up to date", obs, err)
	}
}

// TestUpdateSendsScopesAsArrays: whatever the spec and the observed client
// hold, the PUT carries both scope lists as non-empty JSON arrays, and the
// next observation is up to date.
func TestUpdateSendsScopesAsArrays(t *testing.T) {
	t.Parallel()
	cases := map[string]updateScopesCase{
		"spec leaves them unset": {
			wantScopes:  []string{"openid", "email"},
			wantDefault: []string{"openid"},
		},
		"spec omits openid": {
			spec: func(p *v1alpha1.ClientParameters) {
				p.Scopes = []string{"email"}
				p.DefaultScopes = []string{"profile"}
			},
			wantScopes:  []string{"openid", "email"},
			wantDefault: []string{"openid", "profile"},
		},
		"spec sets them": {
			spec: func(p *v1alpha1.ClientParameters) {
				p.Scopes = []string{"openid", "profile"}
				p.DefaultScopes = []string{"openid", "profile"}
			},
			wantScopes:  []string{"openid", "profile"},
			wantDefault: []string{"openid", "profile"},
		},
		"spec sets empty lists, which the CRD refuses": {
			spec: func(p *v1alpha1.ClientParameters) {
				p.Scopes = []string{}
				p.DefaultScopes = []string{}
			},
			wantScopes:  []string{"openid", "email"},
			wantDefault: []string{"openid"},
		},
		"observed client reports no scopes": {
			observed: func(c *rauthy.ClientResponse) {
				c.Scopes = nil
				c.DefaultScopes = nil
			},
			wantScopes:  []string{"openid"},
			wantDefault: []string{"openid"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// TestCreateThenUpdateSendsScopesAsArrays: the first PUT after a create, built
// from the client Rauthy just created, carries both scope lists as arrays.
func TestCreateThenUpdateSendsScopesAsArrays(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*v1alpha1.Client){
		"confidential, scopes unset": func(*v1alpha1.Client) {},
		"public, scopes unset":       public,
		"scopes set": func(cr *v1alpha1.Client) {
			cr.Spec.ForProvider.Scopes = []string{"openid", "email"}
			cr.Spec.ForProvider.DefaultScopes = []string{"openid", "email"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			h := newHarness(t, f, newClient(mod, func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.Enabled = new(false)
			}))

			got := h.reconcile(t, 5)

			if f.lastPutBody() == nil {
				t.Fatal("no PUT was sent after the create")
			}
			requireScopeArrays(t, f)
			if readyStatus(got) != "True" {
				t.Errorf("Ready = %q, want True; conditions %+v", readyStatus(got), got.Status.Conditions)
			}
		})
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
