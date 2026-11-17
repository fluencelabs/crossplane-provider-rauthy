package client_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// A client deleted and recreated with a new secret after Observe confirmed the
// old one must not be updated: Update confirms ownership again on the client it
// is about to write.
func TestReplacementDuringUpdateIsRefused(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient())
	got := h.reconcile(t, 5)
	if readyStatus(got) != "True" {
		t.Fatalf("setup: %+v", got.Status.Conditions)
	}
	got.Spec.ForProvider.RedirectURIs = []string{"https://desired.example.com/callback"}
	if err := h.kube.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	gets := 0
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/auth/v1/clients/app" {
			gets++
			if gets == 2 { // Update's read, after Observe confirmed the old secret
				f.deleteStored()
				foreign := existing(true)
				foreign.RedirectURIs = []string{"https://foreign.example.com/callback"}
				f.putWithSecret(foreign, "foreign-new-secret")
			}
		}
		f.serve(w, r)
	})
	got = h.reconcile(t, 1)
	if !slices.Equal(f.stored().RedirectURIs, []string{"https://foreign.example.com/callback"}) {
		t.Errorf("the replacement's redirect URIs were overwritten: %v", f.stored().RedirectURIs)
	}
	if readyStatus(got) == "True" {
		t.Error("Ready after acting on a client with another secret")
	}
}

// The same replacement while Delete checks the other records must not be
// deleted: the client is confirmed last, right before DELETE.
func TestReplacementDuringDeleteIsKept(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	armed, swapped := false, false
	h := newHarness(t, f, newClient(), interceptor.Funcs{List: func(
		ctx context.Context, c kube.WithWatch, list kube.ObjectList, opts ...kube.ListOption,
	) error {
		if _, ok := list.(*v1alpha1.ClientList); ok && armed && !swapped && len(opts) == 0 {
			swapped = true
			f.deleteStored()
			f.putWithSecret(existing(true), "foreign-new-secret")
		}
		return c.List(ctx, list, opts...)
	}})
	got := h.reconcile(t, 5)
	if readyStatus(got) != "True" {
		t.Fatalf("setup: %+v", got.Status.Conditions)
	}
	h.ageCreate(t)
	got = h.reconcile(t, 0)
	if !slices.Contains(got.Finalizers, managed.FinalizerName) {
		t.Fatal("missing finalizer")
	}
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	armed = true
	h.reconcile(t, 1)
	if !swapped {
		t.Fatal("the uncached List before DELETE did not run")
	}
	if f.stored() == nil {
		t.Error("the replacement confidential client was deleted")
	}
}
