package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

func TestReconcilerTransientCreateRecovery(t *testing.T) {
	for _, stage := range []string{"binding", "rollback-read"} {
		t.Run(stage, func(t *testing.T) {
			f := newFakeRauthy(t)
			var failed, failRead atomic.Bool
			f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (stage == "binding" && r.URL.Path == "/auth/v1/clients/app/secret" && !failed.Swap(true)) ||
					(stage == "rollback-read" && r.Method == http.MethodGet && r.URL.Path == "/auth/v1/clients/app" && failRead.Swap(false)) {
					writeError(w, http.StatusServiceUnavailable, "temporary outage")
					return
				}
				f.serve(w, r)
			})
			var patchFailed atomic.Bool
			h := newHarness(t, f, newClient(), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch, opts ...kube.SubResourcePatchOption) error {
					if stage == "rollback-read" && !patchFailed.Swap(true) {
						failRead.Store(true)
						return errors.New("temporary status failure")
					}
					return c.SubResource(sub).Patch(ctx, obj, p, opts...)
				},
			})
			got := h.reconcile(t, 10)
			if readyStatus(got) != "True" {
				t.Fatalf(
					"after 10 reconciles with only one transient failure: Ready=%s ownershipPresent=%v clientPresent=%v conditions=%+v",
					readyStatus(got),
					got.Status.Ownership != nil,
					f.stored() != nil,
					got.Status.Conditions,
				)
			}
		})
	}
}

// matchesCreationName checks the durable proof independently of the controller.
func matchesCreationName(cr *v1alpha1.Client, name *string) bool {
	if name == nil || cr.Status.Ownership == nil {
		return false
	}
	sum := sha256.Sum256([]byte("rauthy-creation-name-v1\x00" + *name))
	return cr.Status.Ownership.CreationNameHash == hex.EncodeToString(sum[:])
}

func TestReconcilerPendingCreationKeepsReplacement(t *testing.T) {
	for _, failSecret := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "successful-secret-read", true: "failed-secret-read"}[failSecret],
			func(t *testing.T) {
				testPendingReplacement(t, failSecret)
			},
		)
	}
}

func testPendingReplacement(t *testing.T, failSecret bool) {
	t.Helper()
	f := newFakeRauthy(t)
	foreign := existing(true)
	var swapped atomic.Bool
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/v1/clients/app/secret" && !swapped.Swap(true) {
			f.putWithSecret(foreign, "foreign-secret")
			if failSecret {
				writeError(w, http.StatusServiceUnavailable, "temporary outage")
				return
			}
		}
		f.serve(w, r)
	})
	h := newHarness(t, f, newClient())
	got := h.reconcile(t, 10)
	if !swapped.Load() {
		t.Fatal("replacement hook did not run")
	}
	s := h.connectionSecret(t)
	if readyStatus(got) == "True" || (s != nil && len(s.Data) != 0) {
		t.Fatal("foreign client was accepted or its connection details published")
	}
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, 3)
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("foreign client changed: %s", diff)
	}
	for _, call := range f.callLog() {
		if strings.HasPrefix(call, "PUT ") || strings.HasPrefix(call, "DELETE ") {
			t.Errorf("foreign mutation: %s", call)
		}
	}
}

func TestReconcilerPartialPendingPruningCannotAdopt(t *testing.T) {
	f := newFakeRauthy(t)
	h := newHarness(
		t,
		f,
		newClient(public),
		interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch, opts ...kube.SubResourcePatchOption) error {
				if err := c.SubResource(sub).Patch(ctx, obj, p, opts...); err != nil {
					return err
				}
				cr := obj.(*v1alpha1.Client)
				if cr.Status.Ownership != nil {
					cr.Status.Ownership.Pending = false
					cr.Status.Ownership.CreationNameHash = ""
				}
				return c.Status().Update(ctx, cr)
			},
		},
	)
	h.reconcile(t, 1)
	if f.stored() != nil {
		t.Fatal("POST occurred before proof survived admission")
	}
	foreign := existing(false)
	f.put(foreign)
	got := h.reconcile(t, 4)
	s := h.connectionSecret(t)
	if readyStatus(got) == "True" || (s != nil && len(s.Data) != 0) {
		t.Fatal("pruned pending record accepted a foreign public client")
	}
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, 3)
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("foreign client changed: %s", diff)
	}
}

func TestReconcilerCreationRestoresUnsetName(t *testing.T) {
	for _, confidential := range []bool{false, true} {
		t.Run(map[bool]string{false: "public", true: "confidential"}[confidential], func(t *testing.T) {
			f := newFakeRauthy(t)
			h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.Name = nil
				cr.Spec.ForProvider.Confidential = confidential
			}))
			got := h.reconcile(t, 10)
			if readyStatus(got) != "True" || got.Status.Ownership.Pending {
				t.Fatalf("not finalized: %+v", got.Status)
			}
			if f.stored().Name != nil {
				t.Fatal("temporary creation name was not cleared")
			}
			if got.Status.Ownership.CreationNameHash != "" {
				t.Fatal("creation cleanup did not finish")
			}
		})
	}
}

func TestReconcilerPostCreateFailuresRecover(t *testing.T) {
	for _, stage := range []string{"lookup", "final-record", "cleanup", "cleanup-status"} {
		t.Run(stage, func(t *testing.T) { testPostCreateFailure(t, stage) })
	}
}

func testPostCreateFailure(t *testing.T, stage string) {
	t.Helper()
	f := newFakeRauthy(t)
	var created, failed atomic.Bool
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stage == "lookup" && created.Load() && r.Method == http.MethodGet && !failed.Swap(true) {
			writeError(w, http.StatusServiceUnavailable, "temporary lookup failure")
			return
		}
		if stage == "cleanup" && r.Method == http.MethodPut && !failed.Swap(true) {
			writeError(w, http.StatusServiceUnavailable, "temporary update failure")
			return
		}
		f.serve(w, r)
		if r.URL.Path == "/auth/v1/clients" && r.Method == http.MethodPost {
			created.Store(true)
		}
	})
	var patches atomic.Int32
	h := newHarness(
		t,
		f,
		newClient(),
		interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch, opts ...kube.SubResourcePatchOption) error {
				patchNumber := patches.Add(1)
				if (stage == "final-record" && patchNumber == 2) || (stage == "cleanup-status" && patchNumber == 3) {
					failed.Store(true)
					return errors.New("temporary final record failure")
				}
				return c.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
		},
	)
	got := h.reconcile(t, 10)
	if !failed.Load() {
		t.Fatal("failure hook did not run")
	}
	if readyStatus(got) != "True" || got.Status.Ownership == nil || got.Status.Ownership.Pending {
		t.Fatalf("create did not recover: %+v", got.Status)
	}
	if got.Status.Ownership.CreationNameHash != "" {
		t.Fatal("creation metadata cleanup did not finish")
	}
	if f.stored() == nil || f.stored().Name == nil || *f.stored().Name != "My App" {
		t.Fatal("desired client name not restored")
	}
	assertNoCreationDeletes(t, f)
}

func assertNoCreationDeletes(t *testing.T, f *fakeRauthy) {
	t.Helper()
	for _, call := range f.callLog() {
		if strings.HasPrefix(call, "DELETE ") {
			t.Errorf("unexpected deletion: %s", call)
		}
	}
}

func TestReconcilerPendingWithoutClientRetriesCreate(t *testing.T) {
	f := newFakeRauthy(t)
	f.failPost = true
	h := newHarness(t, f, newClient())
	got := h.reconcile(t, 1)
	if got.Status.Ownership == nil || !got.Status.Ownership.Pending || f.stored() != nil {
		t.Fatal("failed POST did not retain pending reservation")
	}
	f.failPost = false
	got = h.reconcile(t, 10)
	if readyStatus(got) != "True" || got.Status.Ownership.Pending || f.stored() == nil {
		t.Fatalf("absent pending creation did not recover: %+v", got.Status)
	}
}
