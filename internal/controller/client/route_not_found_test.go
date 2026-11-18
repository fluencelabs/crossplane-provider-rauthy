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
	const clientRoute = "/auth/v1/clients/app"
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method+"/empty", func(t *testing.T) {
			empty := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }
			testRouteNotFoundRetainsClient(t, method, clientRoute, empty)
		})
		t.Run(method+"/text", func(t *testing.T) {
			testRouteNotFoundRetainsClient(t, method, clientRoute, http.NotFound)
		})
	}
	t.Run("DELETE/gateway-JSON", func(t *testing.T) {
		gateway := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"NotFound","message":"route not found"}`))
		}
		testRouteNotFoundRetainsClient(t, http.MethodDelete, clientRoute, gateway)
	})
	t.Run("POST/secret", func(t *testing.T) {
		testRouteNotFoundRetainsClient(t, http.MethodPost, clientRoute+"/secret", http.NotFound)
	})
}

func testRouteNotFoundRetainsClient(t *testing.T, method, path string, routeResponse http.HandlerFunc) {
	t.Helper()
	f := newFakeRauthy(t)
	f.put(existing(true))
	var routeUnavailable atomic.Bool
	routeUnavailable.Store(true)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == method && r.URL.Path == path {
			if routeUnavailable.Load() {
				routeResponse(w, r)
				return
			}
			if method != http.MethodPost {
				f.deleteStored()
			}
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
	if f.stored() != nil {
		t.Fatal("the Rauthy client remained after the route recovered")
	}
}
