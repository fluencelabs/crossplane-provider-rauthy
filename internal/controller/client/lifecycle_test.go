package client_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// appliedThenFails writes the patch and still reports an error, as when
	// the answer is lost after the write.
	appliedThenFails := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch,
			opts ...kube.SubResourcePatchOption,
		) error {
			if err := c.SubResource(sub).Patch(ctx, obj, p, opts...); err != nil {
				return err
			}
			return errors.New("timeout after the write")
		},
	}

	cases := map[string]struct {
		reason   string
		cr       *v1alpha1.Client
		server   *rauthy.ClientResponse
		failPost bool
		failDel  bool
		// failSecret makes every secret read fail.
		failSecret bool
		intercept  *interceptor.Funcs
		// readerFails makes every uncached read fail.
		readerFails bool
		// cancelOnPatch cancels the reconcile context inside the status
		// patch and fails it, as when the deadline is what made it fail.
		cancelOnPatch bool
		wantErr       error
		wantErrIs     error
		wantCalls     []string
		// wantOwnedID is the status.ownership the API server holds afterwards.
		wantOwnedID string
		wantRemain  bool
	}{
		"PostOnly": {
			reason:      "Create issues only the POST and records ownership in status.",
			cr:          newClient(),
			wantCalls:   []string{"POST /clients", "POST /clients/app/secret"},
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
			wantCalls: []string{"POST /clients", "POST /clients/app/secret", "DELETE /clients/app"},
		},
		"OwnershipNotRecordedNoRollback": {
			reason:     "If the rollback fails too, the error says what a human must delete.",
			cr:         newClient(),
			intercept:  &statusFails,
			failDel:    true,
			wantErr:    errors.New(`delete client "app" in Rauthy to retry`),
			wantCalls:  []string{"POST /clients", "POST /clients/app/secret", "DELETE /clients/app"},
			wantRemain: true,
		},
		"RecordAppliedDespiteError": {
			reason:      "A patch that landed but answered with an error is confirmed by a re-read: created, no rollback.",
			cr:          newClient(),
			intercept:   &appliedThenFails,
			wantCalls:   []string{"POST /clients", "POST /clients/app/secret"},
			wantOwnedID: testID,
			wantRemain:  true,
		},
		"RecordOutcomeUnknown": {
			reason:      "If the re-read fails too, the client is neither deleted nor claimed.",
			cr:          newClient(),
			intercept:   &statusFails,
			readerFails: true,
			wantErr:     errors.New("outcome of recording its ownership is unknown"),
			wantCalls:   []string{"POST /clients", "POST /clients/app/secret"},
			wantRemain:  true,
		},
		"RollbackSurvivesExpiredContext": {
			reason:        "The rollback runs on a fresh context when the reconcile's has ended.",
			cr:            newClient(),
			cancelOnPatch: true,
			wantErr:       errors.New("was deleted again and will be recreated"),
			wantCalls:     []string{"POST /clients", "POST /clients/app/secret", "DELETE /clients/app"},
		},
		"SecretUnreadableRollsBack": {
			reason:     "Without the new client's secret there is no binding: the client is deleted again.",
			cr:         newClient(),
			failSecret: true,
			wantErr:    errors.New("cannot read the secret of the new client"),
			wantCalls:  []string{"POST /clients", "POST /clients/app/secret", "DELETE /clients/app"},
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
			f.failPost, f.failDelete, f.failSecret = tc.failPost, tc.failDel, tc.failSecret
			if tc.server != nil {
				f.put(tc.server)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base, k, reader := createClients(t, f, tc.cr, tc.intercept, tc.readerFails, tc.cancelOnPatch, cancel)
			e := client.NewExternal(f.api(t), k.Status(), reader, testIssuer)

			_, err := e.Create(ctx, tc.cr)

			checkErr(t, tc.reason, err, tc.wantErr, tc.wantErrIs)
			if diff := cmp.Diff(tc.wantCalls, f.callLog()); diff != "" {
				t.Errorf("%s\nRauthy calls: -want, +got:\n%s", tc.reason, diff)
			}
			// The in-memory record must match the stored one: the runtime's
			// final status update writes the in-memory object.
			if got := inMemoryOwnership(tc.cr); got != tc.wantOwnedID {
				t.Errorf("%s\nstatus.ownership in memory = %q, want %q", tc.reason, got, tc.wantOwnedID)
			}
			if got := storedOwnership(t, base, tc.cr); got != tc.wantOwnedID {
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
	server.Scim = &rauthy.ScimClientRequestResponse{
		BearerToken: rauthy.NewSecret("tok"),
		BaseURI:     "https://scim.example.com",
	}
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
	if diff := cmp.Diff(&want, got, cmp.Comparer(func(a, b rauthy.Secret) bool {
		return a.Reveal() == b.Reveal()
	})); diff != "" {
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
	for _, c := range f.callLog() {
		if c != "GET /clients/app" {
			t.Errorf("Update of a foreign client did more than read it: %s", c)
		}
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
			wantCalls: []string{"GET /clients/app", "POST /clients/app/secret", "DELETE /clients/app"},
		},
		"NotOwned": {
			reason:     "A client this resource did not create is never deleted, whatever its annotations say.",
			cr:         newClient(forged, deleting),
			server:     existing(true),
			wantCalls:  []string{"GET /clients/app"},
			wantRemain: true,
		},
		"AlreadyGone": {
			reason:    "A client that is already gone needs no delete.",
			cr:        newClient(owned, deleting),
			wantCalls: []string{"GET /clients/app"},
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

// createClients returns the API server's truth, the client the status writes
// go through and the uncached reader, with the failures a case asks for.
func createClients(
	t *testing.T,
	f *fakeRauthy,
	cr *v1alpha1.Client,
	intercept *interceptor.Funcs,
	readerFails, cancelOnPatch bool,
	cancel context.CancelFunc,
) (kube.Client, kube.Client, kube.Reader) {
	t.Helper()
	base := newKube(t, f, cr)
	k := base
	switch {
	case cancelOnPatch:
		k = interceptor.NewClient(base.(kube.WithWatch), interceptor.Funcs{
			SubResourcePatch: func(context.Context, kube.Client, string, kube.Object, kube.Patch,
				...kube.SubResourcePatchOption,
			) error {
				cancel()
				return context.Canceled
			},
		})
	case intercept != nil:
		k = interceptor.NewClient(base.(kube.WithWatch), *intercept)
	}
	var reader kube.Reader = base
	if readerFails {
		initialRead := true
		reader = interceptor.NewClient(base.(kube.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c kube.WithWatch, key kube.ObjectKey, obj kube.Object, opts ...kube.GetOption) error {
				if initialRead {
					initialRead = false
					return c.Get(ctx, key, obj, opts...)
				}
				return errors.New("API server unreachable")
			},
		})
	}
	return base, k, reader
}

func TestCreateRecordsBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("Confidential", func(t *testing.T) {
		t.Parallel()
		f := newFakeRauthy(t)
		cr := newClient()
		k := newKube(t, f, cr)
		if _, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Create(ctx, cr); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got := &v1alpha1.Client{}
		if err := k.Get(ctx, kube.ObjectKeyFromObject(cr), got); err != nil {
			t.Fatal(err)
		}
		want := fingerprintOf(testID, f.secretOf())
		if got.Status.Ownership == nil || got.Status.Ownership.SecretFingerprint != want {
			t.Errorf("stored record = %+v, want the fingerprint of the issued secret", got.Status.Ownership)
		}
		if strings.Contains(fmt.Sprintf("%+v", got.Status), f.secretOf()) {
			t.Error("the secret itself is stored in status")
		}
	})

	t.Run("Public", func(t *testing.T) {
		t.Parallel()
		f := newFakeRauthy(t)
		cr := newClient(public)
		k := newKube(t, f, cr)
		if _, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Create(ctx, cr); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if diff := cmp.Diff([]string{"POST /clients"}, f.callLog()); diff != "" {
			t.Errorf("a public client's create reads no secret: -want, +got:\n%s", diff)
		}
		if got := inMemoryOwnership(cr); got != testID || cr.Status.Ownership.SecretFingerprint != "" {
			t.Errorf("record = %+v, want the id and no fingerprint", cr.Status.Ownership)
		}
	})
}

func TestUpdateRefusesConfidentialFlip(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(existing(true))
	cr := newClient(owned)
	ownership := cr.Status.Ownership
	cr.Spec.ForProvider.Confidential = false
	cr.Status.Ownership = ownership
	k := newKube(t, f, cr)

	_, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Update(context.Background(), cr)

	checkErr(t, "Update refuses a change of confidentiality", err,
		errors.New("confidential cannot change after creation"), nil)
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("confidentiality change reached Rauthy: %s", c)
		}
	}
}

func TestFailedDeleteRetainsOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeRauthy(t)
	f.put(existing(true))
	f.failDelete = true
	cr := newClient(owned, deleting)
	k := newKube(t, f, cr)
	e := client.NewExternal(f.api(t), k.Status(), k, testIssuer)
	if err := k.Get(ctx, kube.ObjectKeyFromObject(cr), cr); err != nil {
		t.Fatal(err)
	}
	before := cr.Status.Ownership
	if _, err := e.Delete(ctx, cr); err == nil {
		t.Fatal("failed DELETE returned no error")
	}
	stored := &v1alpha1.Client{}
	if err := k.Get(ctx, kube.ObjectKeyFromObject(cr), stored); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, stored.Status.Ownership); diff != "" {
		t.Fatalf("stored ownership changed: %s", diff)
	}
	if diff := cmp.Diff(before, cr.Status.Ownership); diff != "" {
		t.Fatalf("in-memory ownership changed: %s", diff)
	}
	if f.stored() == nil {
		t.Fatal("failed DELETE removed remote client")
	}
	obs, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Observe(ctx, stored)
	if err != nil || !obs.ResourceExists {
		t.Fatalf("deletion no longer retries: %+v, %v", obs, err)
	}
	f.mu.Lock()
	f.failDelete = false
	f.mu.Unlock()
	if _, err = client.NewExternal(f.api(t), k.Status(), k, testIssuer).Delete(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if f.stored() != nil || storedOwnership(t, k, cr) != "" {
		t.Fatal("retry failed to delete client and clear ownership")
	}
}
