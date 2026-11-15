package client_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/fluencelabs/rauthy-go"
	"github.com/google/go-cmp/cmp"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

func TestCreate(t *testing.T) {
	t.Parallel()

	statusFails := interceptor.Funcs{
		SubResourcePatch: func(context.Context, kube.Client, string, kube.Object, kube.Patch, ...kube.SubResourcePatchOption) error {
			return errors.New("status write refused")
		},
	}

	cases := map[string]struct {
		reason    string
		cr        *v1alpha1.Client
		server    *rauthy.ClientResponse
		failPost  bool
		failDel   bool
		intercept *interceptor.Funcs
		wantErr   error
		wantErrIs error
		wantCalls []string
		// wantOwnedID is the status.ownership the API server holds afterwards.
		wantOwnedID string
		wantRemain  bool
	}{
		"PostOnly": {
			reason:      "Create issues only the POST and records ownership in status.",
			cr:          newClient(),
			wantCalls:   []string{"POST /clients"},
			wantOwnedID: testID,
			wantRemain:  true,
		},
		"ExistingIDIsAdoption": {
			reason:     "A POST refused (4xx) because the id exists is an adoption error and records nothing.",
			cr:         newClient(),
			server:     existing(true),
			wantErrIs:  client.ErrAdoption,
			wantErr:    errors.New("was not created by this resource"),
			wantCalls:  []string{"POST /clients", "GET /clients/app"},
			wantRemain: true,
		},
		"ExistingIDDropsStaleOwnership": {
			reason:     "A re-create clears the record of the vanished client before the POST.",
			cr:         newClient(owned),
			server:     existing(true),
			wantErrIs:  client.ErrAdoption,
			wantCalls:  []string{"POST /clients", "GET /clients/app"},
			wantRemain: true,
		},
		"AmbiguousFailure": {
			reason: "A 5xx POST after which the client exists is an unknown outcome, never ownership.",
			cr:     newClient(),
			server: existing(true),
			// failPost answers 500 even though the client exists.
			failPost:   true,
			wantErrIs:  client.ErrAdoption,
			wantErr:    errors.New("create outcome unknown"),
			wantCalls:  []string{"POST /clients", "GET /clients/app"},
			wantRemain: true,
		},
		"ServerError": {
			reason:    "A failed POST after which no client exists is a plain create error.",
			cr:        newClient(),
			failPost:  true,
			wantErr:   errors.New("cannot create Rauthy client"),
			wantCalls: []string{"POST /clients", "GET /clients/app"},
		},
		"OwnershipNotRecordedRollsBack": {
			reason:    "If ownership cannot be recorded, the new client is deleted again so a retry starts clean.",
			cr:        newClient(),
			intercept: &statusFails,
			wantErr:   errors.New("was deleted again and will be recreated"),
			wantCalls: []string{"POST /clients", "DELETE /clients/app"},
		},
		"OwnershipNotRecordedNoRollback": {
			reason:     "If the rollback fails too, the error says what a human must delete.",
			cr:         newClient(),
			intercept:  &statusFails,
			failDel:    true,
			wantErr:    errors.New(`delete client "app" in Rauthy to retry`),
			wantCalls:  []string{"POST /clients", "DELETE /clients/app"},
			wantRemain: true,
		},
		"StaleOwnershipNotCleared": {
			reason:      "If the stale record cannot be cleared, nothing is created.",
			cr:          newClient(owned),
			intercept:   &statusFails,
			wantErr:     errors.New("not creating client"),
			wantOwnedID: testID,
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
			f.failPost, f.failDelete = tc.failPost, tc.failDel
			if tc.server != nil {
				f.put(tc.server)
			}
			k := newKube(t, tc.cr)
			if tc.intercept != nil {
				k = interceptor.NewClient(k.(kube.WithWatch), *tc.intercept)
			}
			e := client.NewExternal(f.api(t), k.Status(), k, testIssuer)

			_, err := e.Create(context.Background(), tc.cr)

			checkErr(t, tc.reason, err, tc.wantErr, tc.wantErrIs)
			if diff := cmp.Diff(tc.wantCalls, f.callLog()); diff != "" {
				t.Errorf("%s\nRauthy calls: -want, +got:\n%s", tc.reason, diff)
			}
			// The in-memory record must match the stored one: the runtime's
			// final status update writes the in-memory object.
			if got := inMemoryOwnership(tc.cr); got != tc.wantOwnedID {
				t.Errorf("%s\nstatus.ownership in memory = %q, want %q", tc.reason, got, tc.wantOwnedID)
			}
			if got := storedOwnership(t, k, tc.cr); got != tc.wantOwnedID {
				t.Errorf("%s\nstatus.ownership on the API server = %q, want %q", tc.reason, got, tc.wantOwnedID)
			}
			if remains := f.stored() != nil; remains != tc.wantRemain {
				t.Errorf("%s\nclient remains in Rauthy = %v, want %v", tc.reason, remains, tc.wantRemain)
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

	if err := runtimeCreate(t, newExternal(t, f, cr), cr); err != nil {
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
	cr := newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
		cr.Spec.ForProvider.FlowsEnabled = []string{"authorization_code", "refresh_token"}
	})
	e := newExternal(t, f, cr)

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

	if _, err := newExternal(t, f, cr).Update(context.Background(), cr); err != nil {
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

	cr := newClient(forged)
	_, err := newExternal(t, f, cr).Update(context.Background(), cr)

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
			reason:     "A client this resource did not create is never deleted, whatever its annotations say.",
			cr:         newClient(forged, deleting),
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

			_, err := newExternal(t, f, tc.cr).Delete(context.Background(), tc.cr)
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
	// The manifest carries every annotation a successful create would leave.
	cr := newClient(forged, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	})
	e := newExternal(t, f, cr)

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

// storedOwnership returns status.ownership.clientID as the API server holds it.
func storedOwnership(t *testing.T, k kube.Client, cr *v1alpha1.Client) string {
	t.Helper()
	got := &v1alpha1.Client{}
	if err := k.Get(context.Background(), kube.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatalf("get the Client: %v", err)
	}
	if got.Status.Ownership == nil {
		return ""
	}
	return got.Status.Ownership.ClientID
}

func inMemoryOwnership(cr *v1alpha1.Client) string {
	if cr.Status.Ownership == nil {
		return ""
	}
	return cr.Status.Ownership.ClientID
}
