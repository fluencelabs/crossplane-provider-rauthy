package client_test

import (
	"context"
	"sync/atomic"
	"testing"

	kube "sigs.k8s.io/controller-runtime/pkg/client"
)

// countingReader counts the Lists made through the uncached reader.
type countingReader struct {
	kube.Reader

	lists atomic.Int32
}

func (r *countingReader) List(ctx context.Context, list kube.ObjectList, opts ...kube.ListOption) error {
	r.lists.Add(1)
	return r.Reader.List(ctx, list, opts...)
}

// TestOwnedObserveListsFromCache: observing an owned client looks other
// records up in the cache only; the uncached cluster-wide List is kept for
// the create and the delete, where a lagging cache matters most.
func TestOwnedObserveListsFromCache(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient())

	if got := h.reconcile(t, 1); f.stored() == nil {
		t.Fatalf("not created: %+v", got.Status.Conditions)
	}
	if n := h.uncached.lists.Load(); n == 0 {
		t.Error("the create made no uncached List")
	}
	if got := h.reconcile(t, 4); readyStatus(got) != "True" {
		t.Fatalf("Ready = %q, conditions %+v", readyStatus(got), got.Status.Conditions)
	}

	h.uncached.lists.Store(0)
	h.reconcile(t, 3)
	if n := h.uncached.lists.Load(); n != 0 {
		t.Errorf("observing an owned client made %d uncached Lists, want none", n)
	}

	got := h.reconcile(t, 0)
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, 1)
	if n := h.uncached.lists.Load(); n == 0 {
		t.Error("the delete made no uncached List")
	}
	if f.stored() != nil {
		t.Error("the client was not deleted")
	}
}
