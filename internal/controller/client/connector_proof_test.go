package client_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fluencelabs/rauthy-go"
	"github.com/google/go-cmp/cmp"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// A status reader with Rauthy create access races the provider's POST, without
// reading either the API-key Secret or the outgoing creation request body.
func TestConnectorStatusReaderCannotForgeCreationName(t *testing.T) {
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient()) // Uses the production Connector.
	foreign := existing(true)
	var raced atomic.Bool
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/auth/v1/clients" && !raced.Swap(true) {
			cr := &v1alpha1.Client{}
			if err := h.kube.Get(r.Context(), h.key, cr); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			rec := cr.Status.Ownership
			if rec == nil || !rec.Pending {
				t.Error("pending proof not persisted before POST")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mac := hmac.New(sha256.New, []byte(rec.Backend))
			fmt.Fprintf(mac, "rauthy-creation-name-v1\x00%s\x00%s\x00%s\x00%s",
				cr.UID, rec.Backend, rec.ClientID, rec.SecretFingerprint)
			name := "crossplane-" + hex.EncodeToString(mac.Sum(nil))
			if matchesCreationName(cr, &name) {
				t.Error("public backend and status reproduced the creation proof")
			}
			foreign.Name = &name
			f.putWithSecret(foreign, "attacker-known-secret")
		}
		f.serve(w, r)
	})
	got := h.reconcile(t, 10)
	if !raced.Load() {
		t.Fatal("creation race was not exercised")
	}
	if readyStatus(got) == "True" {
		t.Error("client with a publicly predicted name was adopted")
	}
	if s := h.connectionSecret(t); s != nil && len(s.Data) != 0 {
		t.Error("foreign client connection details were published")
	}
	if got.Status.Ownership != nil && !got.Status.Ownership.Pending {
		t.Error("foreign client ownership was finalized")
	}
	assertForgedClientSurvivesDeletion(t, h, f, got, foreign)
}

func assertForgedClientSurvivesDeletion(
	t *testing.T,
	h *harness,
	f *fakeRauthy,
	got *v1alpha1.Client,
	foreign *rauthy.ClientResponse,
) {
	t.Helper()
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t, 3)
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("foreign client changed: %s", diff)
	}
	for _, call := range f.callLog() {
		if strings.HasPrefix(call, "PUT ") || strings.HasPrefix(call, "DELETE ") {
			t.Errorf("foreign client mutation: %s", call)
		}
	}
}
