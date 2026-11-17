package client_test

import (
	"context"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

func TestOwnershipCannotSwitchBackend(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign public client", true: "empty backend"}[missing], func(t *testing.T) {
			t.Parallel()
			checkBackendSwitch(t, missing)
		})
	}
}

func TestIndependentBackendsAllowSameID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := newFakeRauthy(t), newFakeRauthy(t)
	first := newClient(public)
	first.SetUID(types.UID("first"))
	second := newClient(public, func(cr *v1alpha1.Client) { cr.Namespace = "other" })
	second.SetUID(types.UID("second"))
	k := newKube(t, a, first, second)
	if _, err := client.NewExternal(a.api(t), k.Status(), k, testIssuer).Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewExternal(b.api(t), k.Status(), k, testIssuer).Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	for _, cr := range []*v1alpha1.Client{first, second} {
		f := a
		if cr == second {
			f = b
		}
		obs, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Observe(ctx, cr)
		if err != nil || !obs.ResourceExists {
			t.Fatalf("Observe: %+v, %v", obs, err)
		}
	}
}

func TestReconcilerRefusesBackendChange(t *testing.T) {
	t.Parallel()
	for _, changeRef := range []bool{false, true} {
		t.Run(map[bool]string{false: "config URL", true: "config reference"}[changeRef], func(t *testing.T) {
			t.Parallel()
			checkReconcilerBackendSwitch(t, changeRef)
		})
	}
}

func checkBackendSwitch(t *testing.T, missing bool) {
	t.Helper()
	ctx := context.Background()
	a, b := newFakeRauthy(t), newFakeRauthy(t)
	cr := newClient(public)
	k := newKube(t, a, cr)
	if _, err := client.NewExternal(a.api(t), k.Status(), k, testIssuer).Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if !missing {
		b.put(existing(false))
	}
	before := b.stored()
	e := client.NewExternal(b.api(t), k.Status(), k, testIssuer)
	if _, err := e.Observe(ctx, cr); err == nil {
		t.Error("Observe accepted another backend")
	}
	if _, err := e.Update(ctx, cr); err == nil {
		t.Error("Update accepted another backend")
	}
	if _, err := e.Delete(ctx, cr); err == nil {
		t.Error("Delete accepted another backend")
	}
	if _, err := e.Create(ctx, cr); err == nil {
		t.Error("Create accepted another backend")
	}
	if len(writes(b)) != 0 {
		t.Fatalf("foreign backend writes: %v", writes(b))
	}
	if diff := cmp.Diff(before, b.stored()); diff != "" {
		t.Fatal(diff)
	}
	if a.stored() == nil {
		t.Fatal("original client disappeared")
	}
	if storedOwnership(t, k, cr) == "" {
		t.Fatal("original ownership lost")
	}
}

func checkReconcilerBackendSwitch(t *testing.T, changeRef bool) {
	t.Helper()
	ctx := context.Background()
	a, b := newFakeRauthy(t), newFakeRauthy(t)
	h := newHarness(t, a, newClient(public))
	got := h.reconcile(t, 5)
	if readyStatus(got) != "True" {
		t.Fatalf("original client not ready: %+v", got.Status.Conditions)
	}
	b.put(existing(false))
	before := b.stored()
	changeBackend(t, h, got, b.srv.URL, changeRef)
	got = h.reconcile(t, 3)
	if readyStatus(got) != "False" ||
		!strings.Contains(got.GetCondition(xpv2.TypeSynced).Message, "backend cannot change") {
		t.Fatalf("backend change accepted: %+v", got.Status.Conditions)
	}
	if err := h.kube.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = h.reconcile(t, 3)
	if got == nil || got.Status.Ownership == nil {
		t.Fatal("deletion lost original binding")
	}
	if diff := cmp.Diff(before, b.stored()); diff != "" {
		t.Fatal(diff)
	}
	if len(writes(b)) != 0 || a.stored() == nil {
		t.Fatalf("backend switch wrote to a remote client: %v", writes(b))
	}
}

func changeBackend(t *testing.T, h *harness, got *v1alpha1.Client, url string, changeRef bool) {
	t.Helper()
	ctx := context.Background()
	pc := providerConfig(url, new(testIssuer))
	if !changeRef {
		current := &v1alpha1.ClusterProviderConfig{}
		if err := h.kube.Get(ctx, kube.ObjectKey{Name: pc.Name}, current); err != nil {
			t.Fatal(err)
		}
		current.Spec.URL = url
		if err := h.kube.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
		return
	}
	pc.Name = "other"
	if err := h.kube.Create(ctx, pc); err != nil {
		t.Fatal(err)
	}
	got.Spec.ProviderConfigReference.Name = pc.Name
	if err := h.kube.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilerIndependentBackends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b := newFakeRauthy(t), newFakeRauthy(t)
	first := newHarness(t, a, newClient(public, inNamespace(tenantA, redirectA)))
	if got := first.reconcile(t, 5); readyStatus(got) != "True" {
		t.Fatalf("first not ready: %+v", got.Status.Conditions)
	}
	pc := providerConfig(b.srv.URL, new(testIssuer))
	pc.Name = "other"
	if err := first.kube.Create(ctx, pc); err != nil {
		t.Fatal(err)
	}
	second := first.join(t, newClient(public, inNamespace(tenantB, redirectB)), uidB)
	got := second.reconcile(t, 0)
	got.Spec.ProviderConfigReference.Name = pc.Name
	if err := second.kube.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = second.reconcile(t, 5)
	if readyStatus(got) != "True" {
		t.Fatalf("independent backend not ready: %+v", got.Status.Conditions)
	}
	if readyStatus(first.reconcile(t, 2)) != "True" {
		t.Fatal("second backend blocked first")
	}
	second.ageCreate(t)
	got = second.reconcile(t, 0)
	if err := second.kube.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if second.reconcile(t, 3) != nil {
		t.Fatalf("second client not finalized: %+v", second.reconcile(t, 0).Status.Conditions)
	}
	if b.stored() != nil || a.stored() == nil {
		t.Fatal("deleting second backend affected the wrong client")
	}
}

// A record written before records named their Rauthy is bound to the current
// backend on the first Observe, and the client stays managed.
func TestLegacyRecordBindsToCurrentBackend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeRauthy(t)
	f.put(existing(false))
	cr := newClient(owned, public)
	k := newKube(t, f, cr)
	stored := &v1alpha1.Client{}
	if err := k.Get(ctx, kube.ObjectKeyFromObject(cr), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Ownership.BackendURL = ""
	if err := k.Status().Update(ctx, stored); err != nil {
		t.Fatal(err)
	}
	obs, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Observe(ctx, stored)
	if err != nil || !obs.ResourceExists {
		t.Fatalf("Observe of a legacy record = %+v, %v; want the client managed", obs, err)
	}
	after := &v1alpha1.Client{}
	if err = k.Get(ctx, kube.ObjectKeyFromObject(cr), after); err != nil {
		t.Fatal(err)
	}
	if want := f.srv.URL + "/auth/v1"; after.Status.Ownership == nil || after.Status.Ownership.BackendURL != want {
		t.Fatalf("stored ownership = %+v, want backendURL %q", after.Status.Ownership, want)
	}
}
