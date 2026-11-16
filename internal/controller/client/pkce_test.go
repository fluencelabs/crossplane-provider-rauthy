package client_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// The CRD refuses a public client whose challenge methods are set without
// S256. An object stored before that rule existed is refused by the
// controller: nothing of its spec reaches Rauthy, and the reason is on the
// Synced condition.

// challenges sets the challenge methods, never to nil: no argument is [].
func challenges(c ...string) func(*v1alpha1.Client) {
	return func(cr *v1alpha1.Client) { cr.Spec.ForProvider.Challenges = append([]string{}, c...) }
}

// finalized carries the finalizer an object reconciled before the rule got.
func finalized(cr *v1alpha1.Client) { cr.SetFinalizers([]string{managed.FinalizerName}) }

func TestReconcilerPublicWithoutS256IsNotUpdated(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*v1alpha1.Client){
		"empty": challenges(),
		"plain": challenges("plain"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkOwnedPublicWithoutS256(t, mod)
		})
	}
}

// checkOwnedPublicWithoutS256 reconciles a public Client that owns its Rauthy
// client and whose spec mod leaves without S256: nothing is written to
// Rauthy, and deleting the Client still deletes the client.
func checkOwnedPublicWithoutS256(t *testing.T, mod func(*v1alpha1.Client)) {
	t.Helper()
	f := newFakeRauthy(t)
	stored := existing(false)
	stored.Challenges = []string{"S256"}
	f.put(stored)
	h := newHarness(t, f, newClient(owned, public, finalized, mod))

	got := h.reconcile(t, 3)

	checkRefusedPKCE(t, got)
	if c := f.stored(); len(c.Challenges) != 1 || c.Challenges[0] != "S256" {
		t.Errorf("challenges in Rauthy = %v, want [S256] untouched", c.Challenges)
	}
	for _, c := range f.callLog() {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("unexpected call to Rauthy: %s", c)
		}
	}

	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete the Client: %v", err)
	}
	if after := h.reconcile(t, 3); after != nil {
		t.Errorf("the Client was not finalized: %+v", after.Status.Conditions)
	}
	if f.stored() != nil {
		t.Error("the owned Rauthy client was not deleted with its resource")
	}
}

func TestReconcilerPublicWithoutS256IsNotCreated(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(public, challenges()))

	got := h.reconcile(t, 3)

	checkRefusedPKCE(t, got)
	if f.stored() != nil {
		t.Error("a Rauthy client was created")
	}
	if got.Status.Ownership != nil {
		t.Errorf("status.ownership = %+v, want none", got.Status.Ownership)
	}
}

func checkRefusedPKCE(t *testing.T, got *v1alpha1.Client) {
	t.Helper()
	if conditionStatus(got, xpv2.TypeReady) == "True" || conditionStatus(got, xpv2.TypeSynced) != "False" {
		t.Fatalf("conditions %+v; want not Ready and not Synced", got.Status.Conditions)
	}
	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "must keep S256") {
		t.Errorf("Synced message = %q, want the PKCE refusal", msg)
	}
}
