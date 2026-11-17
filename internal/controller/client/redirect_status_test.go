package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fluencelabs/rauthy-go"
	"github.com/google/go-cmp/cmp"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

func TestObserveRedirectDrift(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	server := existing(true)
	server.RedirectURIs = []string{"https://old.example.com/callback"}
	f.put(server)
	cr := newClient(owned)
	e := newExternal(t, f, cr)
	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if obs.ResourceUpToDate || obs.Diff != "fields differ from the spec: redirectUris" {
		t.Fatalf("Observe = %+v, want redirectUris drift", obs)
	}
	if _, err = e.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(cr.Spec.ForProvider.RedirectURIs, f.stored().RedirectURIs); diff != "" {
		t.Fatalf("redirect URIs after update (-want +got):\n%s", diff)
	}
	obs, err = e.Observe(context.Background(), cr)
	if err != nil || !obs.ResourceUpToDate || obs.Diff != "" {
		t.Fatalf("Observe after update = %+v, %v", obs, err)
	}
}

func TestObserveEnabledStatus(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	cr := newClient(owned)
	e := newExternal(t, f, cr)
	for _, enabled := range []bool{true, false, true} {
		server := existing(true)
		server.Enabled = enabled
		f.put(server)
		if _, err := e.Observe(context.Background(), cr); err != nil {
			t.Fatal(err)
		}
		if cr.Status.AtProvider.Enabled != enabled {
			t.Fatalf("status.atProvider.enabled = %v, want %v", cr.Status.AtProvider.Enabled, enabled)
		}
	}
}

func TestCreateRequestFields(t *testing.T) {
	t.Parallel()
	for _, confidential := range []bool{true, false} {
		t.Run(map[bool]string{true: "confidential", false: "public"}[confidential], func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			cr := newClient(func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.Confidential = confidential
				cr.Spec.ForProvider.PostLogoutRedirectURIs = []string{"https://app.example.com/logout"}
			})
			if _, err := newExternal(t, f, cr).Create(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
			got := f.stored()
			if got == nil {
				t.Fatal("client was not created")
			}
			if !validClientName(got.Name) {
				t.Fatal("POST name does not match both Rauthy name patterns")
			}
			if !matchesCreationName(cr, got.Name) {
				t.Fatal("POST name is not bound by the creation record")
			}
			want := rauthy.NewClientRequest{
				ID: testID, Name: got.Name, Confidential: confidential,
				RedirectURIs:           cr.Spec.ForProvider.RedirectURIs,
				PostLogoutRedirectURIs: cr.Spec.ForProvider.PostLogoutRedirectURIs,
			}
			actual := rauthy.NewClientRequest{
				ID: got.ID, Name: got.Name, Confidential: got.Confidential,
				RedirectURIs: got.RedirectURIs, PostLogoutRedirectURIs: got.PostLogoutRedirectURIs,
			}
			if diff := cmp.Diff(want, actual); diff != "" {
				t.Errorf("create fields (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFakeCreateRequiresRedirectArray(t *testing.T) {
	t.Parallel()
	for _, field := range []string{`,"redirect_uris":null`, "", `,"redirect_uris":[]`} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			req := httptest.NewRequest(http.MethodPost, rauthy.APIBasePath+"/clients",
				strings.NewReader(`{"id":"app","confidential":true`+field+`}`))
			rec := httptest.NewRecorder()
			f.serve(rec, req)
			want := http.StatusBadRequest
			if field == `,"redirect_uris":[]` {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body.String())
			}
			if want == http.StatusBadRequest && f.stored() != nil {
				t.Fatal("invalid request created a client")
			}
		})
	}
}

func TestObserveForceMFADrift(t *testing.T) {
	t.Parallel()
	for _, desired := range []bool{true, false} {
		t.Run(map[bool]string{true: "enable", false: "disable"}[desired], func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			server := existing(true)
			server.ForceMFA = !desired
			f.put(server)
			cr := newClient(owned, func(cr *v1alpha1.Client) { cr.Spec.ForProvider.ForceMFA = new(desired) })
			e := newExternal(t, f, cr)
			obs, err := e.Observe(context.Background(), cr)
			if err != nil || obs.ResourceUpToDate || obs.Diff != "fields differ from the spec: forceMfa" ||
				readyStatus(cr) != "False" {
				t.Fatalf("Observe = %+v, %v, Ready=%s", obs, err, readyStatus(cr))
			}
			if _, err = e.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
			if f.stored().ForceMFA != desired {
				t.Fatal("forceMfa was not updated")
			}
			obs, err = e.Observe(context.Background(), cr)
			if err != nil || !obs.ResourceUpToDate || readyStatus(cr) != "True" {
				t.Fatalf("Observe after update = %+v, %v", obs, err)
			}
		})
	}
}

func TestFakeCreateRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"x", strings.Repeat("a", 129), "<script>", "bad/name", "ok-name"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRauthy(t)
			req := httptest.NewRequest(
				http.MethodPost,
				rauthy.APIBasePath+"/clients",
				strings.NewReader(`{"id":"app","name":"`+name+`","confidential":true,"redirect_uris":[]}`),
			)
			rec := httptest.NewRecorder()
			f.serve(rec, req)
			want := http.StatusBadRequest
			if name == "ok-name" {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("POST name %q: status %d, want %d", name, rec.Code, want)
			}
		})
	}
}
