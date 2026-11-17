package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

func TestUpdateReturns500WithoutRetryAndRecovers(t *testing.T) {
	t.Parallel()
	for _, truncated := range []bool{false, true} {
		name := "complete"
		if truncated {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkUpdate500Recovery(t, truncated)
		})
	}
}

func checkUpdate500Recovery(t *testing.T, truncated bool) {
	t.Helper()
	ctx := context.Background()
	f := newFakeRauthy(t)
	f.put(existing(true))
	var fail atomic.Bool
	fail.Store(true)
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			f.serve(w, r)
			return
		}
		puts.Add(1)
		if !fail.Load() {
			f.serve(w, r)
			return
		}
		if truncated {
			w.Header().Set("Content-Length", "100")
		}
		writeError(w, http.StatusInternalServerError, "database unavailable")
	}))
	t.Cleanup(srv.Close)
	api, err := rauthy.New(srv.URL, "test$key", rauthy.WithRetryPolicy(3, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	cr := newClient(owned, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	})
	bindBackend(srv.URL+rauthy.APIBasePath, cr)
	k := newKube(t, f, cr)
	e := client.NewExternal(api, k.Status(), k, testIssuer, rauthy.NewSecret("test$key"))

	_, err = e.Update(ctx, cr)
	var apiErr *rauthy.APIError
	if truncated {
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("Update error = %v, want unexpected EOF", err)
		}
	} else if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("Update error = %v, want APIError with status 500", err)
	}
	if got := puts.Load(); got != 1 {
		t.Errorf("PUT attempts = %d, want 1", got)
	}
	obs, err := e.Observe(ctx, cr)
	if err != nil || !obs.ResourceExists || obs.ResourceUpToDate {
		t.Fatalf("Observe after failure = %+v, %v, want owned client with drift", obs, err)
	}

	fail.Store(false)
	if _, err = e.Update(ctx, cr); err != nil {
		t.Fatalf("Update after recovery: %v", err)
	}
	if got := puts.Load(); got != 2 {
		t.Errorf("PUT attempts after recovery = %d, want 2", got)
	}
	obs, err = e.Observe(ctx, cr)
	if err != nil || !obs.ResourceUpToDate || readyStatus(cr) != "True" {
		t.Fatalf("Observe after recovery = %+v, %v, Ready = %s", obs, err, readyStatus(cr))
	}
}
