package client_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	"github.com/google/go-cmp/cmp"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// existing returns a client as it sits in Rauthy, matching newClient's spec.
func existing(confidential bool) *rauthy.ClientResponse {
	c := &rauthy.ClientResponse{
		ID:                  testID,
		Name:                new("My App"),
		Enabled:             true,
		Confidential:        confidential,
		RedirectURIs:        []string{"https://app.example.com/alt", "https://app.example.com/callback"},
		FlowsEnabled:        []string{"authorization_code", "refresh_token"},
		AccessTokenAlg:      "EdDSA",
		IDTokenAlg:          "EdDSA",
		AuthCodeLifetime:    60,
		AccessTokenLifetime: 1800,
		Scopes:              []string{"openid", "email"},
		DefaultScopes:       []string{"openid"},
	}
	if !confidential {
		c.Challenges = []string{"S256"}
	}
	return c
}

func TestObserve(t *testing.T) {
	t.Parallel()

	type want struct {
		obs   managed.ExternalObservation
		err   error
		errIs error
		// calls is the exact sequence of requests made to Rauthy.
		calls []string
		ready string
	}

	cases := map[string]struct {
		reason string
		cr     *v1alpha1.Client
		server *rauthy.ClientResponse
		// serverSecret, when set, is the secret of the stored client instead
		// of fakeSecret.
		serverSecret string
		want         want
	}{
		"NotCreatedNotFound": {
			reason: "A resource that created nothing and finds nothing asks for a create.",
			cr:     newClient(),
			want: want{
				obs:   managed.ExternalObservation{ResourceExists: false},
				calls: []string{"GET /clients/app"},
			},
		},
		"ForeignClientIsNotAdopted": {
			reason: "A client that exists without this resource having created it is an adoption error.",
			cr:     newClient(),
			server: existing(true),
			want: want{
				errIs: client.ErrAdoption,
				calls: []string{"GET /clients/app"},
			},
		},
		"ForgedAnnotationsAreNotOwnership": {
			reason: "Annotations a manifest can carry never grant ownership: no exists, no secret.",
			cr:     newClient(forged),
			server: existing(true),
			want: want{
				errIs: client.ErrAdoption,
				calls: []string{"GET /clients/app"},
			},
		},
		"InterruptedCreate": {
			reason: "A create interrupted before ownership was recorded is refused with the neutral message.",
			cr:     newClient(func(cr *v1alpha1.Client) { meta.SetExternalCreatePending(cr, time.Now()) }),
			server: existing(true),
			want: want{
				err:   errors.New("if an interrupted create of this resource made it, delete it in Rauthy"),
				calls: []string{"GET /clients/app"},
			},
		},
		"CreateAttemptsOnRecordDoNotChangeWording": {
			reason: "Create attempts on record (annotations) influence neither behaviour nor wording.",
			cr: newClient(func(cr *v1alpha1.Client) {
				meta.SetExternalCreatePending(cr, time.Now())
				meta.SetExternalCreateFailed(cr, time.Now())
			}),
			server: existing(true),
			want: want{
				err:   errors.New("holds no ownership record for it"),
				calls: []string{"GET /clients/app"},
			},
		},
		"RecreatedWithAnotherSecret": {
			reason:       "A client with the recorded id but another secret is not ours: nothing is published.",
			cr:           newClient(owned),
			server:       existing(true),
			serverSecret: "third-party-secret",
			want: want{
				err:   errors.New("is not the one this resource created"),
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
			},
		},
		"ConfidentialRecordWithoutFingerprint": {
			reason: "A record without a fingerprint for a confidential client is not ownership.",
			cr: func() *v1alpha1.Client {
				cr := newClient(owned)
				cr.Status.Ownership.SecretFingerprint = ""
				return cr
			}(),
			server: existing(true),
			want: want{
				err:   errors.New("is not the one this resource created"),
				calls: []string{"GET /clients/app"},
			},
		},
		"FingerprintedRecordButPublicClient": {
			reason: "A record made for a confidential client does not match a public client with the id.",
			cr: func() *v1alpha1.Client {
				cr := newClient(owned)
				cr.Spec.ForProvider.Confidential = false
				return cr
			}(),
			server: existing(false),
			want: want{
				err:   errors.New("is not the one this resource created"),
				calls: []string{"GET /clients/app"},
			},
		},
		"DeletingMismatchFinalizesWithoutTouching": {
			reason:       "A resource being deleted whose record names another client lets go of it untouched.",
			cr:           newClient(owned, deleting),
			server:       existing(true),
			serverSecret: "third-party-secret",
			want: want{
				obs:   managed.ExternalObservation{ResourceExists: false},
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
			},
		},
		"ConfidentialChanged": {
			reason: "Confidentiality cannot change after creation: refused before any update.",
			cr: func() *v1alpha1.Client {
				cr := newClient(owned)
				ownership := cr.Status.Ownership
				cr.Spec.ForProvider.Confidential = false
				cr.Status.Ownership = ownership
				return cr
			}(),
			server: existing(true),
			want: want{
				err:   errors.New("confidential cannot change after creation"),
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
			},
		},
		"OwnedConfidentialUpToDate": {
			reason: "An owned, matching confidential client is up to date and publishes its secret.",
			cr:     newClient(owned),
			server: existing(true),
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: true,
					ConnectionDetails: managed.ConnectionDetails{
						client.ConnectionKeyClientID:     []byte(testID),
						client.ConnectionKeyClientSecret: []byte(fakeSecret),
						client.ConnectionKeyIssuer:       []byte(testIssuer),
					},
				},
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
				ready: "True",
			},
		},
		"OwnedPublicUpToDate": {
			reason: "A public client publishes its id and the issuer but never asks for a secret.",
			cr:     newClient(owned, public),
			server: existing(false),
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: true,
					ConnectionDetails: managed.ConnectionDetails{
						client.ConnectionKeyClientID: []byte(testID),
						client.ConnectionKeyIssuer:   []byte(testIssuer),
					},
				},
				calls: []string{"GET /clients/app"},
				ready: "True",
			},
		},
		"OwnedDiffers": {
			reason: "A spec field that differs from Rauthy makes the resource not up to date and not ready.",
			cr: newClient(owned, func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
				cr.Spec.ForProvider.Scopes = []string{"openid", "profile"}
			}),
			server: existing(true),
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: false,
					Diff:             "fields differ from the spec: accessTokenLifetime, scopes",
					ConnectionDetails: managed.ConnectionDetails{
						client.ConnectionKeyClientID:     []byte(testID),
						client.ConnectionKeyClientSecret: []byte(fakeSecret),
						client.ConnectionKeyIssuer:       []byte(testIssuer),
					},
				},
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
				ready: "False",
			},
		},
		"ListOrderDoesNotMatter": {
			reason: "Lists are compared as sets: a reordered spec list is up to date.",
			cr: newClient(owned, public, func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.FlowsEnabled = []string{"refresh_token", "authorization_code"}
				cr.Spec.ForProvider.Scopes = []string{"email", "openid"}
			}),
			server: existing(false),
			want: want{
				obs: managed.ExternalObservation{
					ResourceExists:   true,
					ResourceUpToDate: true,
					ConnectionDetails: managed.ConnectionDetails{
						client.ConnectionKeyClientID: []byte(testID),
						client.ConnectionKeyIssuer:   []byte(testIssuer),
					},
				},
				calls: []string{"GET /clients/app"},
				ready: "True",
			},
		},
		"OwnedClientVanished": {
			reason: "An owned client that disappeared from Rauthy is created again.",
			cr:     newClient(owned),
			want: want{
				obs:   managed.ExternalObservation{ResourceExists: false},
				calls: []string{"GET /clients/app"},
			},
		},
		"InvalidID": {
			reason: "An external name that is not a valid Rauthy client id fails before any request.",
			cr:     newClient(func(cr *v1alpha1.Client) { meta.SetExternalName(cr, "bad/id") }),
			want:   want{err: errors.New("invalid client id")},
		},
		"ExternalNameChanged": {
			reason: "Changing the external name of an owned resource is refused: the id is immutable.",
			cr:     newClient(owned, func(cr *v1alpha1.Client) { meta.SetExternalName(cr, "other") }),
			server: existing(true),
			want:   want{err: errors.New("the client id is immutable")},
		},
		"PolicyWithoutUpdate": {
			reason: "Policies without Update cannot bring the client to the spec and are refused.",
			cr: newClient(owned, policies(
				xpv2.ManagementActionObserve, xpv2.ManagementActionCreate, xpv2.ManagementActionDelete)),
			server: existing(true),
			want:   want{err: errors.New("lack Update")},
		},
		"ObserveOnlyPolicyNotCreated": {
			reason: "Management policies without Create are refused for a resource that created nothing.",
			cr:     newClient(policies(xpv2.ManagementActionObserve, xpv2.ManagementActionUpdate)),
			server: existing(true),
			want:   want{err: errors.New("lack Create")},
		},
		"ObserveOnlyPolicyOwned": {
			reason: "Observe-only policies are refused even for an owned client: they lack Update.",
			cr:     newClient(owned, public, policies(xpv2.ManagementActionObserve)),
			server: existing(false),
			want:   want{err: errors.New("lack Update")},
		},
		"DeletingWithoutUpdatePolicy": {
			reason: "Policies are not checked while deleting, so deletion is never blocked by them.",
			cr:     newClient(owned, deleting, policies(xpv2.ManagementActionObserve, xpv2.ManagementActionDelete)),
			server: existing(true),
			want: want{
				obs:   managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
			},
		},
		"DeletingNotOwned": {
			reason: "A resource being deleted that never created anything goes away without touching Rauthy.",
			cr:     newClient(deleting),
			server: existing(true),
			want:   want{obs: managed.ExternalObservation{ResourceExists: false}},
		},
		"DeletingOwned": {
			reason: "An owned client being deleted is reported as existing so that Delete runs.",
			cr:     newClient(owned, deleting),
			server: existing(true),
			want: want{
				obs:   managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true},
				calls: []string{"GET /clients/app", "POST /clients/app/secret"},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			switch {
			case tc.server != nil && tc.serverSecret != "":
				f.putWithSecret(tc.server, tc.serverSecret)
			case tc.server != nil:
				f.put(tc.server)
			}
			e := newExternal(t, f, tc.cr)

			got, err := e.Observe(context.Background(), tc.cr)

			checkErr(t, tc.reason, err, tc.want.err, tc.want.errIs)
			if diff := cmp.Diff(tc.want.obs, got); diff != "" {
				t.Errorf("%s\nObserve(...): -want, +got:\n%s", tc.reason, diff)
			}
			if diff := cmp.Diff(tc.want.calls, f.callLog()); diff != "" {
				t.Errorf("%s\nRauthy calls: -want, +got:\n%s", tc.reason, diff)
			}
			if tc.want.ready != "" && readyStatus(tc.cr) != tc.want.ready {
				t.Errorf("%s\nReady = %q, want %q", tc.reason, readyStatus(tc.cr), tc.want.ready)
			}
		})
	}
}

func TestObserveFillsAtProvider(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(existing(true))
	cr := newClient(owned)

	if _, err := newExternal(t, f, cr).Observe(context.Background(), cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	got := cr.Status.AtProvider
	if diff := cmp.Diff(existing(true).RedirectURIs, got.RedirectURIs); diff != "" {
		t.Errorf("observed redirect URIs (-want +got):\n%s", diff)
	}
	if got.ID != testID || got.AccessTokenAlg != "EdDSA" || got.AccessTokenLifetime != 1800 || !got.Confidential {
		t.Errorf("status.atProvider does not carry the observed client: %+v", got)
	}
}

// checkErr checks err against an expected message substring and an expected
// wrapped sentinel; with neither, err must be nil.
func checkErr(t *testing.T, reason string, err, want, wantIs error) {
	t.Helper()
	if wantIs != nil && !errors.Is(err, wantIs) {
		t.Errorf("%s\nerror = %v, want one wrapping %v", reason, err, wantIs)
	}
	if want != nil && (err == nil || !strings.Contains(err.Error(), want.Error())) {
		t.Errorf("%s\nerror = %v, want one containing %q", reason, err, want)
	}
	if want == nil && wantIs == nil && err != nil {
		t.Errorf("%s\nunexpected error: %v", reason, err)
	}
}

// TestStaleCachedOwnershipIsNotTrusted: the cached object says owned, the API
// server says not. Nothing may act as the owner.
func TestStaleCachedOwnershipIsNotTrusted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeRauthy(t)
	foreign := existing(true)
	f.put(foreign)
	truth := newClient()
	k := newKube(t, f, truth)
	stale := truth.DeepCopy()
	owned(stale)

	obs, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Observe(ctx, stale.DeepCopy())
	if !errors.Is(err, client.ErrAdoption) || obs.ResourceExists || obs.ConnectionDetails != nil {
		t.Errorf("Observe = %+v, %v; want an adoption error and nothing published", obs, err)
	}
	if _, err = client.NewExternal(f.api(t), k.Status(), k, testIssuer).Update(ctx, stale.DeepCopy()); err == nil {
		t.Error("Update trusted the cached ownership")
	}
	gone := stale.DeepCopy()
	deleting(gone)
	if _, err = client.NewExternal(f.api(t), k.Status(), k, testIssuer).Delete(ctx, gone); err != nil {
		t.Errorf("Delete: %v", err)
	}

	for _, c := range f.callLog() {
		if c != "GET /clients/app" {
			t.Errorf("acted on a client through a stale record: %s", c)
		}
	}
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("the foreign client changed: -want, +got:\n%s", diff)
	}
}

// TestObserveClearsMarkerOfMissingClient: a marker must not outlive its
// client (S3 -> S0). Observe clears it, confirmed, before reporting the
// client as missing, so a later client with the id can never look owned.
func TestObserveClearsMarkerOfMissingClient(t *testing.T) {
	t.Parallel()
	refuse := interceptor.Funcs{
		SubResourcePatch: func(context.Context, kube.Client, string, kube.Object, kube.Patch,
			...kube.SubResourcePatchOption,
		) error {
			return errors.New("status write refused")
		},
	}

	cases := map[string]struct {
		reason     string
		truth      *v1alpha1.Client
		cached     func(*v1alpha1.Client) *v1alpha1.Client
		intercept  *interceptor.Funcs
		wantErr    error
		wantExists bool
		wantMarker string
	}{
		"MarkerCleared": {
			reason: "The marker of a vanished client is cleared before Observe reports it missing.",
			truth:  newClient(owned),
		},
		"StaleCacheWithoutMarker": {
			reason: "A marker the cache does not show yet is still cleared: the decision is uncached.",
			truth:  newClient(owned),
			cached: func(cr *v1alpha1.Client) *v1alpha1.Client {
				cr.Status.Ownership = nil
				return cr
			},
		},
		"ClearFails": {
			reason:     "If the marker cannot be cleared, Observe fails instead of letting a create happen.",
			truth:      newClient(owned),
			intercept:  &refuse,
			wantErr:    errors.New("cannot clear status.ownership"),
			wantMarker: testID,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			base := newKube(t, f, tc.truth)
			k := base
			if tc.intercept != nil {
				k = interceptor.NewClient(base.(kube.WithWatch), *tc.intercept)
			}
			cr := tc.truth.DeepCopy()
			if tc.cached != nil {
				cr = tc.cached(cr)
			}

			obs, err := client.NewExternal(f.api(t), k.Status(), base, testIssuer).Observe(context.Background(), cr)

			checkErr(t, tc.reason, err, tc.wantErr, nil)
			if obs.ResourceExists != tc.wantExists {
				t.Errorf("%s\nResourceExists = %v, want %v", tc.reason, obs.ResourceExists, tc.wantExists)
			}
			if got := storedOwnership(t, base, cr); got != tc.wantMarker {
				t.Errorf("%s\nstored status.ownership = %q, want %q", tc.reason, got, tc.wantMarker)
			}
		})
	}
}

func TestDeleteClearsMarker(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.put(existing(true))
	cr := newClient(owned, deleting)
	k := newKube(t, f, cr)

	if _, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Delete(context.Background(), cr); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if f.stored() != nil {
		t.Error("the client was not deleted")
	}
	if got := storedOwnership(t, k, cr); got != "" {
		t.Errorf("stored status.ownership = %q after the delete, want none", got)
	}
}

// TestRecordForAnotherIDIsKept: the API server's record names "app", the
// external name is now "app2", and the cache does not show the record yet.
// The record must be neither cleared nor read as "nothing owned", deleting or
// not, or client "app" would leak.
func TestRecordForAnotherIDIsKept(t *testing.T) {
	t.Parallel()

	for name, del := range map[string]bool{"Live": false, "Deleting": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			f.put(existing(true))
			truth := newClient(owned)
			meta.SetExternalName(truth, "app2")
			if del {
				deleting(truth)
			}
			k := newKube(t, f, truth)
			cached := truth.DeepCopy()
			cached.Status.Ownership = nil

			obs, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Observe(context.Background(), cached)

			checkErr(
				t,
				"a record for another id blocks the resource",
				err,
				errors.New("the client id is immutable"),
				nil,
			)
			if obs.ResourceExists {
				t.Error("reported as existing")
			}
			if got := storedOwnership(t, k, truth); got != testID {
				t.Errorf("stored record = %q, want it kept for %q", got, testID)
			}
			if f.stored() == nil {
				t.Error("client app is gone")
			}
			for _, c := range f.callLog() {
				if !strings.HasPrefix(c, "GET") {
					t.Errorf("unexpected call to Rauthy: %s", c)
				}
			}
		})
	}
}
