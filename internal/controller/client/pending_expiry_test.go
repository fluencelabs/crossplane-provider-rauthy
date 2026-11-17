package client_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	clk.Step(time.Minute + time.Second)
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
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
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
	f := newFakeRauthy(t)
	f.failPost = true
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
	// Block the configured client's transport so synctest can measure its
	// timeout exactly. The settlement window reserves 30s of its 60s for the queue.
	synctest.Test(t, func(t *testing.T) {
		transport := http.DefaultTransport
		http.DefaultTransport = pendingLookupStall{} //nolint:reassign // This serial test controls the default transport.
		defer func() {
			http.DefaultTransport = transport //nolint:reassign // Restore the transport before other tests run.
		}()
		const requestBudget = 30 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), requestBudget+5*time.Second)
		defer cancel()
		started := time.Now()
		_, err = e.Delete(ctx, cr)
		if elapsed := time.Since(started); !errors.Is(err, context.DeadlineExceeded) ||
			ctx.Err() != nil || elapsed != requestBudget {
			t.Fatalf(
				"Delete after %s: %v (parent context: %v), want the %s HTTP timeout",
				elapsed,
				err,
				ctx.Err(),
				requestBudget,
			)
		}
	})
}

type pendingLookupStall struct{}

func (pendingLookupStall) RoundTrip(r *http.Request) (*http.Response, error) {
	<-r.Context().Done()
	return nil, r.Context().Err()
}
