package client_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fluencelabs/rauthy-go"
	"github.com/google/go-cmp/cmp"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// withUnmanagedFields returns the test client as Rauthy holds it, carrying
// settings the Client spec does not expose.
func withUnmanagedFields() *rauthy.ClientResponse {
	c := existing(true)
	c.AllowedResources = []string{"https://api.example.com"}
	c.DefaultAud = []string{"api"}
	c.Claims = json.RawMessage(`{"tenant":"acme"}`)
	c.ClaimsAtRoot = true
	return c
}

// unmanaged is the part of a client the provider does not manage.
type unmanaged struct {
	AllowedResources []string
	DefaultAud       []string
	Claims           string
	ClaimsAtRoot     bool
}

func unmanagedOfResponse(c *rauthy.ClientResponse) unmanaged {
	return unmanaged{c.AllowedResources, c.DefaultAud, string(c.Claims), c.ClaimsAtRoot}
}

func unmanagedOfRequest(r *rauthy.UpdateClientRequest) unmanaged {
	return unmanaged{r.AllowedResources, r.DefaultAud, string(r.Claims), r.ClaimsAtRoot}
}

// A PUT replaces the whole client, so it must carry every setting the spec
// does not manage as Rauthy holds it, and those settings must never read as
// drift.
func TestUpdateKeepsUnmanagedFields(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	server := withUnmanagedFields()
	f.put(server)
	want := unmanagedOfResponse(server)
	cr := newClient(owned, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	})
	e := newExternal(t, f, cr)

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.Diff != "fields differ from the spec: accessTokenLifetime" {
		t.Errorf("before the update: diff = %q, want only accessTokenLifetime", obs.Diff)
	}

	if _, err = e.Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}

	put := f.lastPutBody()
	if put == nil {
		t.Fatal("Update sent no PUT")
	}
	if put.AccessTokenLifetime != 600 {
		t.Errorf("PUT accessTokenLifetime = %d, want 600", put.AccessTokenLifetime)
	}
	if diff := cmp.Diff(want, unmanagedOfRequest(put)); diff != "" {
		t.Errorf("the PUT body dropped unmanaged fields; -want, +got:\n%s", diff)
	}
	if diff := cmp.Diff(want, unmanagedOfResponse(f.stored())); diff != "" {
		t.Errorf("Rauthy lost unmanaged fields; -want, +got:\n%s", diff)
	}

	obs, err = e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe after the update: %v", err)
	}
	if !obs.ResourceUpToDate || obs.Diff != "" {
		t.Errorf("after the update: upToDate = %v, diff = %q; want up to date", obs.ResourceUpToDate, obs.Diff)
	}
}
