package client_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

func TestReconcilerDoesNotRetryWritesWithoutOwnership(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			testWriteOwnership(t, method)
		})
	}
}

func testWriteOwnership(t *testing.T, method string) {
	t.Helper()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient())
	got := h.reconcile(t, 5)
	if readyStatus(got) != "True" {
		t.Fatalf("setup: %+v", got.Status.Conditions)
	}
	if method == http.MethodDelete {
		if err := h.kube.Delete(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	} else {
		got.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
		if err := h.kube.Update(context.Background(), got); err != nil {
			t.Fatal(err)
		}
	}
	foreign := existing(true)
	var attempts atomic.Int32
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == method && attempts.Add(1) == 1 {
			f.putWithSecret(foreign, "foreign-secret")
			writeError(w, http.StatusServiceUnavailable, "retry later")
			return
		}
		f.serve(w, r)
	})
	h.reconcile(t, 4)
	if n := attempts.Load(); n != 1 {
		t.Errorf("write attempts = %d, want 1 followed by fresh ownership refusal", n)
	}
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("replacement changed (-want +got): %s", diff)
	}
}

func TestReconcilerCreationFailuresKeepReplacement(t *testing.T) {
	for _, failBinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending-record", true: "binding"}[failBinding], func(t *testing.T) {
			testCreationFailureReplacement(t, failBinding)
		})
	}
}

func testCreationFailureReplacement(t *testing.T, failBinding bool) {
	t.Helper()
	f := newFakeRauthy(t)
	foreign := existing(true)
	var swapped atomic.Bool
	h := newHarness(t, f, newClient(), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch, opts ...kube.SubResourcePatchOption) error {
			if failBinding {
				return c.SubResource(sub).Patch(ctx, obj, p, opts...)
			}
			f.putWithSecret(foreign, "foreign-secret")
			swapped.Store(true)
			return errors.New("record refused")
		},
	})
	if failBinding {
		f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/v1/clients/app/secret" {
				f.putWithSecret(foreign, "foreign-secret")
				swapped.Store(true)
				writeError(w, http.StatusBadRequest, "secret unavailable")
				return
			}
			f.serve(w, r)
		})
	}
	got := h.reconcile(t, 3)
	if !swapped.Load() {
		t.Fatal("replacement hook did not run")
	}
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("replacement changed (-want +got): %s", diff)
	}
	if got.Status.Ownership != nil {
		t.Errorf("replacement claimed: %+v", got.Status.Ownership)
	}
	for _, call := range f.callLog() {
		if call == "DELETE /clients/app" {
			t.Error("creation failure deleted replacement")
		}
	}
}

func TestReconcilerEmptyRecordDoesNotOwnPublicClient(t *testing.T) {
	f := newFakeRauthy(t)
	foreign := existing(false)
	f.put(foreign)
	h := newHarness(t, f, newClient(public, owned, func(cr *v1alpha1.Client) {
		cr.Status.Ownership.ClientID = ""
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))
	got := h.reconcile(t, 3)
	if readyStatus(got) == "True" {
		t.Error("empty record made foreign client Ready")
	}
	if h.connectionSecret(t) != nil {
		t.Error("foreign connection details published")
	}
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, 3)
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("foreign public client changed (-want +got): %s", diff)
	}
	for _, call := range f.callLog() {
		if call != "GET /clients/app" {
			t.Errorf("acted on foreign public client: %s", call)
		}
	}
}
