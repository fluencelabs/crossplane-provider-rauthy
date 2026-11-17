package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fluencelabs/rauthy-go"

	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// The first POST times out at the caller while the server is still processing
// it. It commits after the next observation returns 404,
// before the second POST is processed. No status is forged.
func TestReconcilerLateCommitRetainsCreationProof(t *testing.T) {
	for _, confidential := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "public", true: "confidential"}[confidential],
			func(t *testing.T) { testLateCommit(t, confidential) },
		)
	}
}

func testLateCommit(t *testing.T, confidential bool) {
	t.Helper()
	f := newFakeRauthy(t)
	started, release, committed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var posts atomic.Int32
	var firstName string
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/auth/v1/clients" {
			if posts.Add(1) == 1 {
				firstName = readPostName(t, r)
				close(started)
				<-release
				f.serve(w, r)
				close(committed)
				return
			}
			unblock()
			<-committed
			if name := readPostName(t, r); name != firstName {
				t.Errorf("retry name = %q, first = %q", name, firstName)
			}
		}
		f.serve(w, r)
	})
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Confidential = confidential
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))
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
	first := h.reconcile(t, 0)
	if first.Status.Ownership == nil || !first.Status.Ownership.Pending {
		t.Fatal("no durable pending proof")
	}
	got := h.reconcile(t, 10)
	if posts.Load() != 2 || f.stored() == nil {
		t.Fatal("late commit ordering was not exercised")
	}
	assertLateCommitResult(t, h, f, got, confidential)
}

func assertLateCommitResult(t *testing.T, h *harness, f *fakeRauthy, got *v1alpha1.Client, confidential bool) {
	t.Helper()
	if readyStatus(got) != "True" || got.Status.Ownership == nil || got.Status.Ownership.Pending {
		t.Fatalf("late create did not recover: %+v", got.Status)
	}
	if f.stored().AccessTokenLifetime != 600 || f.created != 1 || len(f.clients) != 1 {
		t.Fatal("expected one configured client")
	}
	if s := h.connectionSecret(t); s == nil || (confidential && string(s.Data["client_secret"]) != f.secretOf()) {
		t.Fatal("owned secret not published")
	}
}

func readPostName(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req rauthy.NewClientRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		t.Error(err)
	}
	if req.Name == nil {
		t.Error("POST omitted creation name")
		return ""
	}
	return *req.Name
}
