package client_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/clock"
	clocktest "k8s.io/utils/clock/testing"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

func TestReconcilerDeletePendingCreation(t *testing.T) {
	for _, confidential := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "confidential"}[confidential], func(t *testing.T) {
			for _, replacement := range []bool{false, true} {
				t.Run(map[bool]string{false: "owned", true: "foreign"}[replacement], func(t *testing.T) {
					testDeletePendingCreation(t, confidential, replacement)
				})
			}
		})
	}
}

func delayedCreation(t *testing.T, confidential bool, clk clock.PassiveClock,
	intercept ...interceptor.Funcs,
) (*harness, *fakeRauthy, func()) {
	t.Helper()
	f := newFakeRauthy(t)
	started, release, committed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var posts atomic.Int32
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/auth/v1/clients" {
			posts.Add(1)
			close(started)
			<-release
			f.serve(w, r)
			close(committed)
			return
		}
		f.serve(w, r)
	})
	h := newHarnessWithClock(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Confidential = confidential
	}), clk, intercept...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = h.r.Reconcile(ctx, reconcile.Request{NamespacedName: h.key}) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("POST not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled reconcile did not return")
	}
	got := h.reconcile(t, 0)
	if got.Status.Ownership == nil || !got.Status.Ownership.Pending {
		t.Fatal("no durable pending proof")
	}
	return h, f, func() {
		unblock()
		select {
		case <-committed:
		case <-time.After(10 * time.Second):
			t.Fatal("POST did not commit")
		}
		if posts.Load() != 1 {
			t.Fatalf("POST count = %d, want 1", posts.Load())
		}
	}
}

func testDeletePendingCreation(t *testing.T, confidential, replacement bool) {
	t.Helper()
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	h, f, commit := delayedCreation(t, confidential, clk)
	got := h.reconcile(t, 0)
	proof := got.Status.Ownership.DeepCopy()
	h.ageCreate(t)
	got = h.reconcile(t, 0)
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	clk.Step(59 * time.Second)
	got = h.reconcile(t, 4)
	if got == nil || len(got.Finalizers) == 0 {
		t.Fatal("Client finalized while POST could still commit")
	}
	if diff := cmp.Diff(proof, got.Status.Ownership); diff != "" {
		t.Fatalf("pending proof changed before POST committed: %s", diff)
	}
	commit()
	late := f.stored()
	if late == nil || !late.Enabled {
		t.Fatal("delayed POST did not create an enabled client")
	}
	if replacement {
		late.Name = new("foreign client")
		f.putWithSecret(late, "foreign-secret")
	} else {
		f.mu.Lock()
		f.failDelete = true
		f.mu.Unlock()
		if got = h.reconcile(t, 2); got == nil || len(got.Finalizers) == 0 || f.stored() == nil {
			t.Fatal("failed DELETE lost the client or finalizer")
		}
		f.mu.Lock()
		f.failDelete = false
		f.mu.Unlock()
	}
	clk.Step(2 * time.Second)
	f.resetCalls()
	if got = h.reconcile(t, 4); got != nil {
		t.Fatalf("Client not finalized after resolving POST: %+v", got.Status)
	}
	deleted := slices.Contains(f.callLog(), "DELETE /clients/app")
	if replacement {
		if diff := cmp.Diff(late, f.stored()); diff != "" || deleted {
			t.Fatalf("foreign client changed: %s; DELETE = %v", diff, deleted)
		}
	} else if f.stored() != nil || !deleted {
		t.Fatal("late owned client was not deleted")
	}
}

func TestDeletePendingCreationWithoutClient(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.failPost = true
	cr := newClient()
	e := newExternal(t, f, cr)
	if err := runtimeCreate(t, e, cr); err == nil || cr.Status.Ownership == nil || !cr.Status.Ownership.Pending {
		t.Fatal("failed POST did not leave pending proof")
	}
	proof := cr.Status.Ownership.DeepCopy()
	// Delete must confirm pending ownership even if its caller has a stale copy.
	cr.Status.Ownership = nil
	f.resetCalls()
	if _, err := e.Delete(context.Background(), cr); err == nil {
		t.Fatal("Delete reported completion while POST outcome remained unknown")
	}
	if diff := cmp.Diff(proof, cr.Status.Ownership); diff != "" {
		t.Fatalf("Delete discarded pending proof: %s", diff)
	}
	if diff := cmp.Diff([]string{"GET /clients/app"}, f.callLog()); diff != "" {
		t.Fatalf("Delete mutated an unresolved client: %s", diff)
	}
}

func TestReconcilerDeletePendingCreationUsesLookupTime(t *testing.T) {
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	var afterLookup func()
	h, f, commit := delayedCreation(t, true, clk, interceptor.Funcs{
		Get: func(ctx context.Context, c kube.WithWatch, key kube.ObjectKey, obj kube.Object, opts ...kube.GetOption) error {
			if _, ok := obj.(*v1alpha1.Client); ok && afterLookup != nil {
				afterLookup()
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	proof := h.reconcile(t, 0).Status.Ownership.DeepCopy()
	h.ageCreate(t)
	if err := h.kube.Delete(t.Context(), h.reconcile(t, 0)); err != nil {
		t.Fatal(err)
	}
	clk.Step(59 * time.Second)
	f.resetCalls()
	crossed := false
	afterLookup = func() {
		if !crossed && slices.Contains(f.callLog(), "GET /clients/app") {
			crossed = true
			commit()
			clk.Step(2 * time.Second)
		}
	}
	got := h.reconcile(t, 1)
	afterLookup = nil
	if !crossed || f.stored() == nil {
		t.Fatal("late POST did not commit after the deletion lookup")
	}
	if got == nil || len(got.Finalizers) == 0 {
		t.Fatal("Client finalized using a 404 received before expiry")
	}
	if diff := cmp.Diff(proof, got.Status.Ownership); diff != "" {
		t.Fatalf("pending proof changed: %s", diff)
	}
	if got = h.reconcile(t, 4); got != nil || f.stored() != nil {
		t.Fatal("the next reconcile did not delete the late client and finalize")
	}
}
