package client_test

import (
	"context"
	"slices"
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
	for name, mod := range map[string]func(*v1alpha1.Client){
		"empty": challenges(),
		"plain": challenges("plain"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			h := newHarness(t, f, newClient(public, mod))

			got := h.reconcile(t, 3)

			checkRefusedPKCE(t, got)
			if f.stored() != nil {
				t.Error("a Rauthy client was created")
			}
			if got.Status.Ownership != nil {
				t.Errorf("status.ownership = %+v, want none", got.Status.Ownership)
			}
		})
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

// Unset challenges preserve Rauthy's methods, so observation must refuse an
// unsafe public client until an explicit spec repairs it.
func TestReconcilerPublicUnsetChallengesChecksObserved(t *testing.T) {
	t.Parallel()
	for name, methods := range map[string][]string{"empty": {}, "plain": {"plain"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			stored := existing(false)
			stored.Challenges = methods
			f.put(stored)
			h := newHarness(t, f, newClient(owned, public, finalized))
			got := h.reconcile(t, 3)
			checkRefusedPKCE(t, got)
			if reason := got.GetCondition(xpv2.TypeSynced).Reason; reason != xpv2.ReasonReconcileError {
				t.Errorf("Synced reason = %q, want ReconcileError", reason)
			}
			for _, c := range f.callLog() {
				if !strings.HasPrefix(c, "GET ") {
					t.Errorf("unexpected call to Rauthy: %s", c)
				}
			}
			// Deletion still finalizes an unsafe owned client.
			if err := h.kube.Delete(context.Background(), got); err != nil {
				t.Fatal(err)
			}
			if after := h.reconcile(t, 3); after != nil {
				t.Error("the Client was not finalized")
			}
			if f.stored() != nil {
				t.Error("the owned Rauthy client was not deleted")
			}
		})
	}
}

func TestReconcilerPKCEAllowedAndRepair(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		confidential bool
		observed     []string
		specified    []string
	}{
		{"public observed S256", false, []string{"S256"}, nil},
		{"public explicit repair", false, []string{}, []string{"S256", "plain"}},
		{"confidential empty", true, []string{}, []string{}},
		{"confidential plain", true, []string{"plain"}, []string{"plain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			stored := existing(tc.confidential)
			stored.Challenges = tc.observed
			f.put(stored)
			h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
				cr.Spec.ForProvider.Confidential = tc.confidential
				cr.Spec.ForProvider.Challenges = tc.specified
				owned(cr)
			}))
			got := h.reconcile(t, 5)
			if conditionStatus(got, xpv2.TypeReady) != "True" || conditionStatus(got, xpv2.TypeSynced) != "True" {
				t.Fatalf("conditions %+v; want Ready and Synced", got.Status.Conditions)
			}
			if tc.specified != nil && !slices.Equal(f.stored().Challenges, tc.specified) {
				t.Errorf("stored challenges = %v, want %v", f.stored().Challenges, tc.specified)
			}
		})
	}
}

func TestReconcilerPublicUnsetChallengesDriftClearsReady(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	stored := existing(false)
	stored.Challenges = []string{"S256"}
	f.put(stored)
	h := newHarness(t, f, newClient(owned, public, finalized))
	if got := h.reconcile(t, 3); conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("setup: conditions %+v, want Ready", got.Status.Conditions)
	}
	stored.Challenges = []string{}
	f.put(stored)
	checkRefusedPKCE(t, h.reconcile(t, 2))
	// Only an explicit S256 list repairs the observed methods.
	got := h.reconcile(t, 0)
	got.Spec.ForProvider.Challenges = []string{"S256"}
	if err := h.kube.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if got = h.reconcile(t, 3); conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("after repair: conditions %+v, want Ready", got.Status.Conditions)
	}
	if !slices.Equal(f.stored().Challenges, []string{"S256"}) {
		t.Errorf("stored challenges = %v, want [S256]", f.stored().Challenges)
	}
}

func TestUpdatePublicUnsetChallengesChecksObserved(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	stored := existing(false)
	stored.Challenges = []string{}
	f.put(stored)
	cr := newClient(owned, public)
	if _, err := newExternal(
		t,
		f,
		cr,
	).Update(context.Background(), cr); err == nil ||
		!strings.Contains(err.Error(), "must keep S256") {
		t.Fatalf("Update error = %v, want the PKCE refusal", err)
	}
	for _, c := range f.callLog() {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("unexpected call to Rauthy: %s", c)
		}
	}
}

// A refusal that comes before the PKCE check still clears an earlier Ready.
func TestReconcilerEarlyRefusalClearsReady(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]func(*testing.T, *harness, *v1alpha1.Client){
		"ownership lost": func(t *testing.T, h *harness, cr *v1alpha1.Client) {
			t.Helper()
			cr.Status.Ownership = nil
			if err := h.kube.Status().Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
		},
		"observe only": func(t *testing.T, h *harness, cr *v1alpha1.Client) {
			t.Helper()
			cr.Spec.ManagementPolicies = xpv2.ManagementPolicies{xpv2.ManagementActionObserve}
			if err := h.kube.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			stored := existing(false)
			f.put(stored)
			h := newHarness(t, f, newClient(owned, public, finalized))
			got := h.reconcile(t, 3)
			if conditionStatus(got, xpv2.TypeReady) != "True" {
				t.Fatalf("setup: conditions %+v, want Ready", got.Status.Conditions)
			}
			refuse(t, h, got)
			stored.Challenges = []string{}
			f.put(stored)
			got = h.reconcile(t, 3)
			if conditionStatus(got, xpv2.TypeReady) == "True" || conditionStatus(got, xpv2.TypeSynced) != "False" {
				t.Fatalf("conditions %+v; want not Ready and not Synced", got.Status.Conditions)
			}
		})
	}
}
