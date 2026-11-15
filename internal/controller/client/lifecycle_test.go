package client_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/fluencelabs/rauthy-go"
	"github.com/google/go-cmp/cmp"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

func TestCreate(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		reason    string
		cr        *v1alpha1.Client
		server    *rauthy.ClientResponse
		failPost  bool
		wantErr   error
		wantErrIs error
		wantCalls []string
		// wantOwnedID is the created-client-id annotation Create must leave.
		wantOwnedID string
	}{
		"PostOnly": {
			reason:      "Create issues only the POST and records the created id.",
			cr:          newClient(),
			wantCalls:   []string{"POST /clients"},
			wantOwnedID: testID,
		},
		"ExistingIDIsAdoption": {
			reason:    "A POST refused because the id exists is an adoption error and records no ownership.",
			cr:        newClient(),
			server:    existing(true),
			wantErrIs: client.ErrAdoption,
			wantCalls: []string{"POST /clients", "GET /clients/app"},
		},
		"ExistingIDDropsStaleOwnership": {
			reason: "A re-create that hits a foreign client drops the record of the old client.",
			cr: newClient(owned, func(cr *v1alpha1.Client) {
				meta.SetExternalCreatePending(cr, meta.GetExternalCreateSucceeded(cr))
			}),
			server:    existing(true),
			wantErrIs: client.ErrAdoption,
			wantCalls: []string{"POST /clients", "GET /clients/app"},
		},
		"ServerError": {
			reason:    "Any other POST failure is a plain create error.",
			cr:        newClient(),
			failPost:  true,
			wantErr:   errors.New("cannot create Rauthy client"),
			wantCalls: []string{"POST /clients", "GET /clients/app"},
		},
		"InvalidID": {
			reason:  "An invalid id fails before any request.",
			cr:      newClient(func(cr *v1alpha1.Client) { meta.SetExternalName(cr, "x") }),
			wantErr: errors.New("invalid client id"),
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			f.failPost = tc.failPost
			if tc.server != nil {
				f.put(tc.server)
			}
			e := client.NewExternal(f.api(t), testIssuer)

			_, err := e.Create(context.Background(), tc.cr)

			checkErr(t, tc.reason, err, tc.wantErr, tc.wantErrIs)
			if diff := cmp.Diff(tc.wantCalls, f.callLog()); diff != "" {
				t.Errorf("%s\nRauthy calls: -want, +got:\n%s", tc.reason, diff)
			}
			if got := tc.cr.GetAnnotations()[client.AnnotationKeyCreatedClientID]; got != tc.wantOwnedID {
				t.Errorf("%s\ncreated-client-id = %q, want %q", tc.reason, got, tc.wantOwnedID)
			}
		})
	}
}

func TestCreateSendsOnlyCreateFields(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	cr := newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	})

	if err := runtimeCreate(t, client.NewExternal(f.api(t), testIssuer), cr); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := f.stored()
	if got == nil || got.AccessTokenLifetime != 1800 || *got.Name != "My App" {
		t.Errorf("after the POST Rauthy should hold its defaults plus the create fields, got %+v", got)
	}
}

// TestPartialCreateConverges walks the reconciler's path through a create
// whose configuring PUT fails: the resource must keep owning the client and
// converge once the PUT goes through.
func TestPartialCreateConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeRauthy(t)
	e := client.NewExternal(f.api(t), testIssuer)
	cr := newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
		cr.Spec.ForProvider.FlowsEnabled = []string{"authorization_code", "refresh_token"}
	})

	obs, err := e.Observe(ctx, cr)
	if err != nil || obs.ResourceExists {
		t.Fatalf("first Observe = %+v, %v; want a missing client", obs, err)
	}
	if err = runtimeCreate(t, e, cr); err != nil {
		t.Fatalf("Create: %v", err)
	}

	f.failPut = true
	obs, err = e.Observe(ctx, cr)
	if err != nil || !obs.ResourceExists || obs.ResourceUpToDate {
		t.Fatalf("Observe after the POST = %+v, %v; want an owned client that is not up to date", obs, err)
	}
	if _, err = e.Update(ctx, cr); err == nil {
		t.Fatal("Update succeeded although the PUT failed")
	}

	obs, err = e.Observe(ctx, cr)
	if err != nil || !obs.ResourceExists || obs.ResourceUpToDate {
		t.Fatalf("Observe after the failed PUT = %+v, %v; want still owned and not up to date", obs, err)
	}

	f.failPut = false
	if _, err = e.Update(ctx, cr); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if readyStatus(cr) != "True" {
		t.Errorf("Ready after a successful Update = %q, want True", readyStatus(cr))
	}
	obs, err = e.Observe(ctx, cr)
	if err != nil || !obs.ResourceUpToDate {
		t.Fatalf("Observe after the PUT = %+v, %v; want up to date", obs, err)
	}
	if got := f.stored(); got.AccessTokenLifetime != 600 {
		t.Errorf("accessTokenLifetime in Rauthy = %d, want 600", got.AccessTokenLifetime)
	}
}

func TestUpdatePreservesUnsetFields(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	server := existing(true)
	server.AccessTokenLifetime = 999
	server.ForceMFA = true
	server.ClientURI = new("https://app.example.com")
	server.Contacts = []string{"ops@example.com"}
	server.Scim = &rauthy.ScimClientRequestResponse{BearerToken: "tok", BaseURI: "https://scim.example.com"}
	f.put(server)
	cr := newClient(owned, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Enabled = new(false)
	})

	if _, err := client.NewExternal(f.api(t), testIssuer).Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := f.stored()
	want := *server
	want.Enabled = false
	want.RedirectURIs = cr.Spec.ForProvider.RedirectURIs
	if diff := cmp.Diff(&want, got); diff != "" {
		t.Errorf("only enabled should change; -want, +got:\n%s", diff)
	}
}

func TestUpdateRefusesForeignClient(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(existing(true))

	_, err := client.NewExternal(f.api(t), testIssuer).Update(context.Background(), newClient())

	if err == nil {
		t.Fatal("Update of a client this resource did not create succeeded")
	}
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("Update of a foreign client talked to Rauthy: %v", calls)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		reason     string
		cr         *v1alpha1.Client
		server     *rauthy.ClientResponse
		wantCalls  []string
		wantRemain bool
	}{
		"Owned": {
			reason:    "An owned client is deleted.",
			cr:        newClient(owned, deleting),
			server:    existing(true),
			wantCalls: []string{"DELETE /clients/app"},
		},
		"NotOwned": {
			reason:     "A client this resource did not create is never deleted.",
			cr:         newClient(deleting),
			server:     existing(true),
			wantRemain: true,
		},
		"AlreadyGone": {
			reason:    "A 404 on delete is success.",
			cr:        newClient(owned, deleting),
			wantCalls: []string{"DELETE /clients/app"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			if tc.server != nil {
				f.put(tc.server)
			}

			_, err := client.NewExternal(f.api(t), testIssuer).Delete(context.Background(), tc.cr)
			if err != nil {
				t.Errorf("%s\nDelete: %v", tc.reason, err)
			}
			if diff := cmp.Diff(tc.wantCalls, f.callLog()); diff != "" {
				t.Errorf("%s\nRauthy calls: -want, +got:\n%s", tc.reason, diff)
			}
			if remains := f.stored() != nil; remains != tc.wantRemain {
				t.Errorf("%s\nclient remains = %v, want %v", tc.reason, remains, tc.wantRemain)
			}
		})
	}
}

// TestNoAdoptionEndToEnd runs a foreign client through every entry point the
// reconciler has and checks that Rauthy is never written to.
func TestNoAdoptionEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeRauthy(t)
	foreign := existing(true)
	foreign.AccessTokenLifetime = 42
	f.put(foreign)
	e := client.NewExternal(f.api(t), testIssuer)
	cr := newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	})

	obs, err := e.Observe(ctx, cr)
	if !errors.Is(err, client.ErrAdoption) || obs.ResourceExists || obs.ConnectionDetails != nil {
		t.Errorf("Observe = %+v, %v; want an adoption error and no connection details", obs, err)
	}
	if err = runtimeCreate(t, e, cr); !errors.Is(err, client.ErrAdoption) {
		t.Errorf("Create error = %v, want an adoption error", err)
	}
	obs, err = e.Observe(ctx, cr)
	if !errors.Is(err, client.ErrAdoption) || obs.ResourceExists {
		t.Errorf("Observe after a refused create = %+v, %v; want an adoption error", obs, err)
	}
	if _, err = e.Update(ctx, cr); err == nil {
		t.Error("Update of a foreign client succeeded")
	}
	deleting(cr)
	if obs, err = e.Observe(ctx, cr); err != nil || obs.ResourceExists {
		t.Errorf("Observe while deleting = %+v, %v; want a clean exit", obs, err)
	}
	if _, err = e.Delete(ctx, cr); err != nil {
		t.Errorf("Delete: %v", err)
	}

	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("the foreign client changed: -want, +got:\n%s", diff)
	}
	for _, c := range f.callLog() {
		if c != "GET /clients/app" && c != "POST /clients" {
			t.Errorf("unexpected call to Rauthy: %s", c)
		}
	}
}
