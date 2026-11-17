package client_test

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
)

func TestReconcilerRouteNotFoundRetainsClient(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method+"/empty", func(t *testing.T) {
			testRouteNotFoundRetainsClient(t, method, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			})
		})
		t.Run(method+"/text", func(t *testing.T) {
			testRouteNotFoundRetainsClient(t, method, http.NotFound)
		})
	}
}

func testRouteNotFoundRetainsClient(t *testing.T, method string, routeResponse http.HandlerFunc) {
	t.Helper()
	f := newFakeRauthy(t)
	f.put(existing(true))
	var routeUnavailable atomic.Bool
	routeUnavailable.Store(true)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == method && r.URL.Path == "/auth/v1/clients/app" {
			if routeUnavailable.Load() {
				routeResponse(w, r)
				return
			}
			f.deleteStored()
		}
		f.serve(w, r)
	})

	h := newHarness(t, f, newClient(owned, deleting))
	proof := h.reconcile(t, 0).Status.Ownership
	got := h.reconcile(t, 1)
	if got == nil || len(got.Finalizers) == 0 {
		t.Fatal("route 404 finalized the Client")
	}
	if diff := cmp.Diff(proof, got.Status.Ownership); diff != "" {
		t.Fatalf("route 404 changed ownership: %s", diff)
	}
	synced := got.GetCondition(xpv2.TypeSynced)
	if synced.Reason != xpv2.ReasonReconcileError || !strings.Contains(synced.Message, "HTTP 404") {
		t.Fatalf("route 404 did not report a reconcile error: %+v", synced)
	}
	if f.stored() == nil {
		t.Fatal("route 404 removed the Rauthy client")
	}

	routeUnavailable.Store(false)
	if got = h.reconcile(t, 1); got != nil && got.Status.Ownership != nil {
		t.Fatal("Rauthy NotFound retained ownership")
	}
	if h.reconcile(t, 1) != nil {
		t.Fatal("Rauthy NotFound did not allow finalization")
	}
}
