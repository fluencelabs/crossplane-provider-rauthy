package client_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"k8s.io/apimachinery/pkg/types"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// Rauthy client ids are global; Client objects are namespaced. These tests
// put two Clients with the same external name in two namespaces of one
// cluster, reconciled by one provider against one Rauthy.

// join adds cr to the API server h reconciles against, with its status as
// given, and returns a harness for it that shares h's reconciler.
func (h *harness) join(t *testing.T, cr *v1alpha1.Client, uid types.UID) *harness {
	t.Helper()
	pc := &v1alpha1.ClusterProviderConfig{}
	if err := h.kube.Get(context.Background(), kube.ObjectKey{Name: "default"}, pc); err != nil {
		t.Fatal(err)
	}
	bindBackend(pc.Spec.URL+"/auth/v1", cr)
	cr.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "default"}
	cr.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: "app-oidc"}
	cr.SetUID(uid)
	status := cr.Status
	if err := h.kube.Create(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	cr.Status = status
	if err := h.kube.Status().Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	other := *h
	other.key = kube.ObjectKeyFromObject(cr)
	return &other
}

func inNamespace(ns string, redirect string) func(*v1alpha1.Client) {
	return func(cr *v1alpha1.Client) {
		cr.Namespace = ns
		cr.Spec.ForProvider.RedirectURIs = []string{redirect}
	}
}

const (
	tenantA   = "tenant-a"
	tenantB   = "tenant-b"
	redirectA = "https://a.example.com/callback"
	redirectB = "https://b.example.com/callback"
	uidB      = "6d1e2c3b-4a59-4f68-8e7d-1c2b3a4d5e6f"
)

// twoTenants returns harnesses for a public Client in tenant-a and one with
// the same external name in tenant-b. modA and modB adjust each before it is
// stored.
func twoTenants(t *testing.T, f *fakeRauthy, modA, modB func(*v1alpha1.Client)) (*harness, *harness) {
	t.Helper()
	a := newHarness(t, f, newClient(public, inNamespace(tenantA, redirectA), modA))
	b := a.join(t, newClient(public, inNamespace(tenantB, redirectB), modB), uidB)
	return a, b
}

func noop(*v1alpha1.Client) {}

func writes(f *fakeRauthy) []string {
	var out []string
	for _, c := range f.callLog() {
		// POST /clients/{id}/secret only reads the secret.
		if strings.HasPrefix(c, "PUT ") || strings.HasPrefix(c, "DELETE ") || c == "POST /clients" {
			out = append(out, c)
		}
	}
	return out
}

func checkNeutral(t *testing.T, got *v1alpha1.Client, otherNamespace string) {
	t.Helper()
	msg := got.GetCondition(xpv2.TypeSynced).Message
	if !strings.Contains(msg, "holds no ownership record") {
		t.Errorf("Synced message = %q, want the neutral refusal", msg)
	}
	if strings.Contains(msg, otherNamespace) {
		t.Errorf("Synced message = %q names the other namespace", msg)
	}
}

// TestSameIDInTwoNamespaces: whichever Client arrives first owns the id; the
// other never updates or deletes that Rauthy client.
func TestSameIDInTwoNamespaces(t *testing.T) {
	t.Parallel()
	for name, firstIsA := range map[string]bool{"AFirst": true, "BFirst": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			a, b := twoTenants(t, f, noop, noop)
			first, second := a, b
			wantRedirect, otherNS := redirectA, tenantA
			if !firstIsA {
				first, second = b, a
				wantRedirect, otherNS = redirectB, tenantB
			}

			if got := first.reconcile(t, 5); readyStatus(got) != "True" {
				t.Fatalf("first Client Ready = %q, conditions %+v", readyStatus(got), got.Status.Conditions)
			}
			f.resetCalls()
			checkNeutral(t, second.reconcile(t, 5), otherNS)

			second.ageCreate(t)
			if err := second.kube.Delete(context.Background(), second.reconcile(t, 0)); err != nil {
				t.Fatal(err)
			}
			if gone := second.reconcile(t, 3); gone != nil {
				t.Errorf("second Client not finalized: %+v", gone.Status.Conditions)
			}
			if w := writes(f); len(w) != 0 {
				t.Errorf("the second Client wrote to Rauthy: %v", w)
			}
			if c := f.stored(); c == nil || !slices.Equal(c.RedirectURIs, []string{wantRedirect}) {
				t.Errorf("Rauthy client = %+v, want the first Client's redirect URIs", c)
			}
		})
	}
}

// TestStaleRecordDoesNotTakeOverAnotherNamespace is the window the review
// found: A's client is deleted out of band while A's record remains, and B
// in another namespace asks for the same id. B must not create while A's
// record names the id; once A has cleared it, B creates its own, and A then
// neither overwrites nor deletes B's client.
func TestStaleRecordDoesNotTakeOverAnotherNamespace(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	a, b := twoTenants(t, f, noop, noop)
	if got := a.reconcile(t, 5); readyStatus(got) != "True" {
		t.Fatalf("A Ready = %q, conditions %+v", readyStatus(got), got.Status.Conditions)
	}
	f.deleteStored()

	gotB := b.reconcile(t, 2)
	if f.stored() != nil {
		t.Fatal("B created the id while A's record still names it")
	}
	checkNeutral(t, gotB, tenantA)

	// A sees its client gone and clears its record; B can now create.
	if got := a.reconcile(t, 1); got.Status.Ownership != nil {
		t.Fatalf("A kept its record of a missing client: %+v", got.Status.Ownership)
	}
	b.ageCreate(t)
	if got := b.reconcile(t, 5); readyStatus(got) != "True" {
		t.Fatalf("B Ready = %q, conditions %+v", readyStatus(got), got.Status.Conditions)
	}

	f.resetCalls()
	gotA := a.reconcile(t, 5)
	checkNeutral(t, gotA, tenantB)
	a.ageCreate(t)
	if err := a.kube.Delete(context.Background(), a.reconcile(t, 0)); err != nil {
		t.Fatal(err)
	}
	if gone := a.reconcile(t, 3); gone != nil {
		t.Errorf("A not finalized: %+v", gone.Status.Conditions)
	}
	if w := writes(f); len(w) != 0 {
		t.Errorf("A wrote to B's Rauthy client: %v", w)
	}
	if c := f.stored(); c == nil || !slices.Equal(c.RedirectURIs, []string{redirectB}) {
		t.Errorf("Rauthy client = %+v, want B's redirect URIs", c)
	}
}

// TestTwoRecordsForOneID: two Clients already record the same id (a record
// left from before this guard, or one forged through clients/status). Neither
// may act as the owner while the other's record exists: no PUT, no DELETE,
// nothing published, and deleting one finalizes it without touching Rauthy.
func TestTwoRecordsForOneID(t *testing.T) {
	t.Parallel()
	for name, confidential := range map[string]bool{"Public": false, "Confidential": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkTwoRecords(t, confidential)
		})
	}
}

func checkTwoRecords(t *testing.T, confidential bool) {
	t.Helper()
	f := newFakeRauthy(t)
	server := existing(confidential)
	server.RedirectURIs = []string{redirectB}
	f.put(server)
	conf := func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Confidential = confidential
		owned(cr)
	}
	a, b := twoTenants(t, f, conf, conf)

	for range 2 {
		checkNeutral(t, a.reconcile(t, 2), tenantB)
		checkNeutral(t, b.reconcile(t, 2), tenantA)
	}
	for _, h := range []*harness{a, b} {
		if s := h.connectionSecret(t); s != nil {
			t.Errorf("%s: connection Secret published: %v", h.key, s.Data)
		}
	}
	a.ageCreate(t)
	if err := a.kube.Delete(context.Background(), a.reconcile(t, 0)); err != nil {
		t.Fatal(err)
	}
	if gone := a.reconcile(t, 3); gone != nil {
		t.Errorf("A not finalized: %+v", gone.Status.Conditions)
	}
	if w := writes(f); len(w) != 0 {
		t.Errorf("a Client wrote to Rauthy: %v", w)
	}
	if c := f.stored(); c == nil || !slices.Equal(c.RedirectURIs, []string{redirectB}) {
		t.Errorf("Rauthy client = %+v, want it untouched", c)
	}
}

// TestRefusalNamesHolderOnlyInProviderLog: the operator can find the Client
// that holds a blocking record from the provider's log, while the refused
// resource's conditions and events never name it.
func TestRefusalNamesHolderOnlyInProviderLog(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	a, b := twoTenants(t, f, noop, noop)
	if got := a.reconcile(t, 5); readyStatus(got) != "True" {
		t.Fatalf("A Ready = %q, conditions %+v", readyStatus(got), got.Status.Conditions)
	}
	f.deleteStored()

	gotB := b.reconcile(t, 2)
	checkNeutral(t, gotB, tenantA)
	for {
		select {
		case e := <-b.events.Events:
			if strings.Contains(e, tenantA) {
				t.Errorf("event %q names the other Client", e)
			}
			continue
		default:
		}
		break
	}
	if log := b.provider.all(); !strings.Contains(log, tenantA+"/"+testID) {
		t.Errorf("provider log does not name the Client holding the record:\n%s", log)
	}
}
