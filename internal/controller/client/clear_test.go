package client_test

import (
	"context"
	"testing"

	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// withSettings is a client in Rauthy holding a CORS origin, a group
// restriction and default scopes, all of which a spec may clear.
func withSettings() *rauthy.ClientResponse {
	c := existing(true)
	c.AllowedOrigins = []string{"https://app.example.com"}
	c.RestrictGroupPrefix = new("app-")
	c.DefaultScopes = []string{"openid", "email"}
	return c
}

// TestExplicitEmptyClears: an explicit empty list or empty string means
// "none". The spec reports drift while Rauthy holds a value, the PUT sends the
// field cleared, and the next observation is up to date.
func TestExplicitEmptyClears(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(withSettings())
	cr := newClient(owned, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AllowedOrigins = []string{}
		cr.Spec.ForProvider.RestrictGroupPrefix = new("")
		cr.Spec.ForProvider.DefaultScopes = []string{}
	})
	e := newExternal(t, f, cr)

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.ResourceUpToDate {
		t.Fatal("Observe reports up to date while Rauthy still holds the values the spec clears")
	}
	if _, err = e.Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}

	put := f.lastPutBody()
	if put == nil {
		t.Fatal("no PUT was sent")
	}
	if len(put.AllowedOrigins) != 0 {
		t.Errorf("PUT allowed_origins = %v, want none", put.AllowedOrigins)
	}
	if put.RestrictGroupPrefix != nil {
		t.Errorf("PUT restrict_group_prefix = %q, want none", *put.RestrictGroupPrefix)
	}
	// default_scopes is a required array in Rauthy's API: "none" is [], not
	// null.
	if put.DefaultScopes == nil || len(put.DefaultScopes) != 0 {
		t.Errorf("PUT default_scopes = %#v, want []", put.DefaultScopes)
	}

	obs, err = e.Observe(context.Background(), cr)
	if err != nil || !obs.ResourceUpToDate {
		t.Fatalf("Observe after the PUT = %+v, %v; want up to date", obs, err)
	}
}

// TestAbsentKeeps: a field the spec leaves out keeps what Rauthy holds.
func TestAbsentKeeps(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(withSettings())
	cr := newClient(owned, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Enabled = new(false)
	})
	e := newExternal(t, f, cr)

	if _, err := e.Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := f.stored()
	if len(got.AllowedOrigins) != 1 || got.RestrictGroupPrefix == nil || *got.RestrictGroupPrefix != "app-" ||
		len(got.DefaultScopes) != 2 {
		t.Errorf("Rauthy client = %+v, want the origin, the group restriction and the default scopes kept", got)
	}
	obs, err := e.Observe(context.Background(), cr)
	if err != nil || !obs.ResourceUpToDate {
		t.Fatalf("Observe = %+v, %v; want up to date", obs, err)
	}
}

// TestReconcilerKeepsExplicitEmpty: an explicit empty list survives the
// runtime's own writes of the object, so "none" is not turned into "absent"
// (keep) on the way through the API server.
func TestReconcilerKeepsExplicitEmpty(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AllowedOrigins = []string{}
		cr.Spec.ForProvider.ClientURI = new("")
	}))

	got := h.reconcile(t, 5)

	if got.Spec.ForProvider.AllowedOrigins == nil {
		t.Error("spec.forProvider.allowedOrigins: the explicit [] was dropped")
	}
	if got.Spec.ForProvider.ClientURI == nil {
		t.Error(`spec.forProvider.clientUri: the explicit "" was dropped`)
	}
	if got.Spec.ForProvider.Scopes != nil {
		t.Errorf("spec.forProvider.scopes = %#v, want absent", got.Spec.ForProvider.Scopes)
	}
}
