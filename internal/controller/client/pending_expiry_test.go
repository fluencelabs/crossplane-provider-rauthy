package client_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/types"
	clocktest "k8s.io/utils/clock/testing"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

func TestReconcilerFailedPostDeletionExpires(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-attempt", true: "recent-retry"}[retry], func(t *testing.T) {
			testFailedPostDeletionExpires(t, retry)
		})
	}
}

func testFailedPostDeletionExpires(t *testing.T, retry bool) {
	t.Helper()
	f := newFakeRauthy(t)
	f.failPost = true
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	h := newHarnessWithClock(t, f, newClient(), clk)
	first := h.reconcile(t, 1)
	if first.Status.Ownership == nil || !first.Status.Ownership.Pending {
		t.Fatal("failed POST did not retain pending proof")
	}
	if retry {
		clk.Step(2 * time.Minute)
		h.ageCreate(t)
		got := h.reconcile(t, 1)
		want := first.Status.Ownership.DeepCopy()
		want.CreatedAt = got.Status.Ownership.CreatedAt
		if diff := cmp.Diff(want, got.Status.Ownership); diff != "" {
			t.Fatalf("retry changed creation proof: %s", diff)
		}
	}
	h.ageCreate(t)
	got := h.reconcile(t, 0)
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	// 30 second HTTP timeout plus 30 second queue allowance, strictly exceeded.
	clk.Step(time.Minute)
	if got = h.reconcile(t, 3); got == nil || len(got.Finalizers) == 0 {
		t.Fatal("finalized before the last POST's settlement boundary")
	}
	if !got.Status.Ownership.CreatedAt.Time.Equal(clk.Now().Add(-time.Minute)) {
		t.Fatal("pending timestamp does not name the latest POST")
	}
	clk.Step(time.Second)
	if got = h.reconcile(t, 4); got != nil {
		t.Fatalf("failed POST left Client terminating after boundary: %+v", got.Status)
	}
	if f.stored() != nil || slices.Contains(f.callLog(), "DELETE /clients/app") {
		t.Fatal("absent client deletion mutated Rauthy")
	}
}

func TestDirectDeletePendingCreationExpires(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.failPost = true
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	var delay404 atomic.Bool
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/auth/v1/clients/app" && delay404.Swap(false) {
			// The lookup starts at +59s, but the 404 reaches Delete at +61s.
			clk.Step(2 * time.Second)
			writeClientNotFound(w, hiqliteNotFound)
			return
		}
		f.serve(w, r)
	})
	h := newHarnessWithClock(t, f, newClient(), clk)
	cr := h.reconcile(t, 1)
	c := &client.Connector{Kube: h.kube, Usage: &noopTracker{}, Clock: clk}
	e, err := c.Connect(t.Context(), cr)
	if err != nil {
		t.Fatal(err)
	}
	proof := cr.Status.Ownership.DeepCopy()
	cr.Status.Ownership = nil
	if _, err = e.Delete(t.Context(), cr); err == nil {
		t.Fatal("Delete completed before the POST settlement boundary")
	}
	if diff := cmp.Diff(proof, cr.Status.Ownership); diff != "" {
		t.Fatalf("Delete lost fresh pending proof: %s", diff)
	}
	clk.Step(59 * time.Second)
	delay404.Store(true)
	if _, err = e.Delete(t.Context(), cr); err == nil || delay404.Load() {
		t.Fatalf("Delete must reject the delayed 404: %v", err)
	}
	if diff := cmp.Diff(proof, h.reconcile(t, 0).Status.Ownership); diff != "" {
		t.Fatalf("delayed 404 changed pending proof: %s", diff)
	}
	if _, err = e.Delete(t.Context(), cr); err != nil || cr.Status.Ownership != nil {
		t.Fatalf("Delete failed to settle absent pending client: %v, %+v", err, cr.Status.Ownership)
	}
	if got := h.reconcile(t, 0); got.Status.Ownership != nil {
		t.Fatal("pending ownership was not cleared durably")
	}
	if f.stored() != nil || slices.Contains(f.callLog(), "DELETE /clients/app") {
		t.Fatal("Delete mutated an absent Rauthy client")
	}
}

func TestReconcilerDeletePendingCreationOnLookupError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			testDeletePendingCreationOnLookupError(t, status)
		})
	}
}

func testDeletePendingCreationOnLookupError(t *testing.T, status int) {
	t.Helper()
	f := newFakeRauthy(t)
	f.commitThenFail = true
	var failGet atomic.Bool
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failGet.Load() && r.Method == http.MethodGet && r.URL.Path == "/auth/v1/clients/app" {
			writeError(w, status, http.StatusText(status))
			return
		}
		f.serve(w, r)
	})
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	h := newHarnessWithClock(t, f, newClient(), clk)
	cr := h.reconcile(t, 1)
	proof := cr.Status.Ownership.DeepCopy()
	if proof == nil || !proof.Pending || f.stored() == nil {
		t.Fatal("POST did not leave both a client and pending proof")
	}
	h.ageCreate(t)
	if err := h.kube.Delete(t.Context(), h.reconcile(t, 0)); err != nil {
		t.Fatal(err)
	}
	clk.Step(time.Minute + time.Second)
	f.resetCalls()
	failGet.Store(true)
	got := h.reconcile(t, 1)
	if got == nil || len(got.Finalizers) == 0 {
		t.Fatalf("Client finalized on GET %d", status)
	}
	if diff := cmp.Diff(proof, got.Status.Ownership); diff != "" {
		t.Fatalf("GET %d changed pending proof: %s", status, diff)
	}
	if f.stored() == nil || slices.Contains(f.callLog(), "DELETE /clients/app") {
		t.Fatalf("GET %d caused a Rauthy deletion", status)
	}
	failGet.Store(false)
	if got = h.reconcile(t, 4); got != nil || f.stored() != nil {
		t.Fatal("the next reconcile did not delete the client and finalize")
	}
}

func TestPendingRetryRequiresDurableTimestamp(t *testing.T) {
	f := newFakeRauthy(t)
	f.failPost = true
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	prune := false
	h := newHarnessWithClock(t, f, newClient(), clk, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch,
			opts ...kube.SubResourcePatchOption,
		) error {
			if prune {
				p = kube.RawPatch(types.MergePatchType, []byte("{}"))
			}
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	})
	first := h.reconcile(t, 1)
	clk.Step(2 * time.Minute)
	h.ageCreate(t)
	f.resetCalls()
	prune = true
	got := h.reconcile(t, 1)
	if diff := cmp.Diff(first.Status.Ownership, got.Status.Ownership); diff != "" {
		t.Fatalf("failed timestamp refresh changed proof: %s", diff)
	}
	if slices.Contains(f.callLog(), "POST /clients") {
		t.Fatal("POST sent without a durably refreshed timestamp")
	}
}

func TestDeletePendingCreationUsesHTTPTimeout(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.failPost = true
	var stall atomic.Bool
	canceled := make(chan struct{})
	lookupArrival := make(chan time.Time, 1)
	testCtx := t.Context()
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/auth/v1/clients/app" && stall.Swap(false) {
			lookupArrival <- time.Now()
			select {
			case <-r.Context().Done():
				close(canceled)
			case <-testCtx.Done():
			}
			return
		}
		f.serve(w, r)
	})
	clk := clocktest.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	h := newHarnessWithClock(t, f, newClient(), clk)
	cr := h.reconcile(t, 1)
	if cr.Status.Ownership == nil || !cr.Status.Ownership.Pending {
		t.Fatal("failed POST did not leave pending proof")
	}
	c := &client.Connector{Kube: h.kube, Usage: &noopTracker{}, Clock: clk}
	e, err := c.Connect(t.Context(), cr)
	if err != nil {
		t.Fatal(err)
	}
	// The settlement window allows 30s for the request and 30s for the queue.
	// Delete also does Kubernetes lookups. Allow five seconds for those,
	// scheduling, and HTTP cleanup, with a separate parent deadline.
	const requestBudget, maxElapsed = 30 * time.Second, 35 * time.Second
	ctx, cancel := context.WithTimeout(testCtx, maxElapsed+5*time.Second)
	defer cancel()
	stall.Store(true)
	started := time.Now()
	_, err = e.Delete(ctx, cr)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil || elapsed < requestBudget || elapsed > maxElapsed {
		t.Fatalf("Delete after %s: %v (parent context: %v), want HTTP timeout in [%s, %s]",
			elapsed, err, ctx.Err(), requestBudget, maxElapsed)
	}
	// Check the time before the HTTP timeout starts separately. The fake
	// Kubernetes client and loopback connection have no intentional wait.
	const maxLookupStart = 1500 * time.Millisecond
	select {
	case arrived := <-lookupArrival:
		if wait := arrived.Sub(started); wait > maxLookupStart {
			t.Fatalf("Delete reached its ownership lookup after %s, want at most %s", wait, maxLookupStart)
		}
	default:
		t.Fatal("Delete did not reach its ownership lookup")
	}
	select {
	case <-canceled:
		t.Logf("The server observed cancellation after the client timed out in %s.", elapsed)
	case <-time.After(time.Second):
		t.Fatal("server did not observe cancellation of the stalled GET")
	}
}
