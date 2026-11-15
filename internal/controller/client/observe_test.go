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

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// existing returns a client as it sits in Rauthy, matching newClient's spec.
func existing(confidential bool) *rauthy.ClientResponse {
	return &rauthy.ClientResponse{
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
		want   want
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
		"ForeignClientAfterFailedRecreate": {
			reason: "A failed re-create after an earlier success does not keep ownership.",
			cr: newClient(owned, func(cr *v1alpha1.Client) {
				meta.SetExternalCreatePending(cr, time.Now())
				meta.SetExternalCreateFailed(cr, time.Now())
			}),
			server: existing(true),
			want: want{
				errIs: client.ErrAdoption,
				calls: []string{"GET /clients/app"},
			},
		},
		"SucceededWithoutCreatedID": {
			reason: "The runtime's create-succeeded mark alone, without the id Create records, is not ownership.",
			cr: newClient(owned, func(cr *v1alpha1.Client) {
				meta.RemoveAnnotations(cr, client.AnnotationKeyCreatedClientID)
			}),
			server: existing(true),
			want: want{
				errIs: client.ErrAdoption,
				calls: []string{"GET /clients/app"},
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
		"ObserveOnlyPolicyNotCreated": {
			reason: "Management policies without Create are refused for a resource that created nothing.",
			cr:     newClient(policies(xpv2.ManagementActionObserve)),
			server: existing(true),
			want:   want{err: errors.New("adoption is not supported")},
		},
		"ObserveOnlyPolicyOwned": {
			reason: "A resource that created its client may drop Create from its policies later.",
			cr:     newClient(owned, public, policies(xpv2.ManagementActionObserve)),
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
				calls: []string{"GET /clients/app"},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			if tc.server != nil {
				f.put(tc.server)
			}
			e := client.NewExternal(f.api(t), testIssuer)

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

	if _, err := client.NewExternal(f.api(t), testIssuer).Observe(context.Background(), cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	got := cr.Status.AtProvider
	if got.ID != testID || got.AccessTokenAlg != "EdDSA" || got.AccessTokenLifetime != 1800 || !got.Confidential {
		t.Errorf("status.atProvider does not carry the observed client: %+v", got)
	}
}

// checkErr compares err with either an exact message substring or a wrapped
// sentinel.
func checkErr(t *testing.T, reason string, err, want, wantIs error) {
	t.Helper()
	switch {
	case wantIs != nil:
		if !errors.Is(err, wantIs) {
			t.Errorf("%s\nerror = %v, want one wrapping %v", reason, err, wantIs)
		}
	case want != nil:
		if err == nil || !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("%s\nerror = %v, want one containing %q", reason, err, want)
		}
	case err != nil:
		t.Errorf("%s\nunexpected error: %v", reason, err)
	}
}
