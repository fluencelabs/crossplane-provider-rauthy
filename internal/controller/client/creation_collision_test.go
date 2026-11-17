package client_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

func TestReconcilerCreationRestoresEmptyName(t *testing.T) {
	for _, confidential := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "confidential"}[confidential], func(t *testing.T) {
			f := newFakeRauthy(t)
			h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.Name = new("")
				cr.Spec.ForProvider.Confidential = confidential
			}))
			got := h.reconcile(t, 10)
			if readyStatus(got) != "True" || got.Status.Ownership.Pending {
				t.Fatalf("not finalized: %+v", got.Status)
			}
			if f.stored().Name != nil {
				t.Fatal("empty desired name was not normalized to nil")
			}
			if got.Status.Ownership.CreationNameHash != "" {
				t.Fatal("creation metadata cleanup did not finish")
			}
		})
	}
}

func TestReconcilerConcurrentCreationClearFailure(t *testing.T) {
	for _, confidential := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "confidential"}[confidential], func(t *testing.T) {
			testConcurrentCreationClearFailure(t, confidential)
		})
	}
}

func testConcurrentCreationClearFailure(t *testing.T, confidential bool) {
	t.Helper()
	f := newFakeRauthy(t)
	faults, failedClear := concurrentCreationFaults()
	a := newHarness(t, f, newClient(inNamespace(tenantA, redirectA), func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Confidential = confidential
	}), faults)
	b := a.join(t, newClient(inNamespace(tenantB, redirectB), func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Confidential = confidential
	}), uidB)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, h := range []*harness{a, b} {
		wg.Go(func() { _, _ = h.r.Reconcile(ctx, reconcile.Request{NamespacedName: h.key}) })
	}
	wg.Wait()
	var loserNamespace string
	select {
	case loserNamespace = <-failedClear:
	default:
		t.Fatal("concurrent creates did not exercise the failed clear")
	}
	winner, loser := b, a
	if loserNamespace == tenantB {
		winner, loser = a, b
	}
	if got := loser.reconcile(t, 0); got.Status.Ownership == nil || !got.Status.Ownership.Pending {
		t.Fatal("loser's failed clear did not leave a pending proof")
	}
	if got := winner.reconcile(t, 5); readyStatus(got) != "True" {
		t.Fatalf("loser's proof blocked winner: %+v", got.Status.Conditions)
	}
	want := f.stored()
	f.resetCalls()
	got := loser.reconcile(t, 5)
	if got.Status.Ownership != nil {
		t.Fatal("loser's stale pending proof was not cleared")
	}
	if readyStatus(got) == "True" {
		t.Fatal("loser became Ready")
	}
	if s := loser.connectionSecret(t); s != nil && len(s.Data) != 0 {
		t.Fatal("loser published winner's connection details")
	}
	if err := loser.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	loser.reconcile(t, 3)
	if w := writes(f); len(w) != 0 {
		t.Errorf("loser wrote to winner's client: %v", w)
	}
	if diff := cmp.Diff(want, f.stored()); diff != "" {
		t.Errorf("winner's client changed: %s", diff)
	}
}

func concurrentCreationFaults() (interceptor.Funcs, <-chan string) {
	var lists atomic.Int32
	bothListed := make(chan struct{})
	failedClear := make(chan string, 1)
	var clearFailed atomic.Bool

	return interceptor.Funcs{
		List: func(ctx context.Context, c kube.WithWatch, list kube.ObjectList, opts ...kube.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			_, isClients := list.(*v1alpha1.ClientList)
			if !isClients || len(opts) != 0 {
				return nil
			}
			n := lists.Add(1)
			if n > 2 {
				return nil
			}
			if n == 2 {
				close(bothListed)
			}
			select {
			case <-bothListed:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		},
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch, opts ...kube.SubResourcePatchOption) error {
			raw, err := p.Data(obj)
			if err != nil {
				return err
			}
			if bytes.Contains(raw, []byte(`"ownership":null`)) && !clearFailed.Swap(true) {
				failedClear <- obj.GetNamespace()
				return errors.New("temporary clear failure")
			}
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
	}, failedClear
}
