package client_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// These tests drive the real crossplane-runtime managed reconciler, with the
// provider's own reconciler options, against a fake API server and the fake
// Rauthy. They check what the external client tests cannot: that ownership
// recorded in status survives the runtime's own updates of the object.

// fakeManager provides the two things managed.NewReconciler takes from a
// manager.
type fakeManager struct {
	manager.Manager

	client kube.Client
	scheme *runtime.Scheme
}

func (m fakeManager) GetClient() kube.Client     { return m.client }
func (m fakeManager) GetScheme() *runtime.Scheme { return m.scheme }

type harness struct {
	kube kube.Client
	r    *managed.Reconciler
	key  kube.ObjectKey
}

func newHarness(t *testing.T, f *fakeRauthy, cr *v1alpha1.Client) *harness {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cr.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "default"}
	cr.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: "app-oidc"}
	// The usage tracker names its record after the resource's UID, which a
	// real API server assigns and the fake does not.
	cr.SetUID("0b5c6f7e-3d2a-4c1b-9e8f-7a6b5c4d3e2f")
	k := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.Client{}, &v1alpha1.ClusterProviderConfig{}).
		WithObjects(providerConfig(f.srv.URL, new(testIssuer)), apiKeySecret(), cr).
		Build()

	c := &client.Connector{
		Kube:  k,
		Usage: resource.NewProviderConfigUsageTracker(k, &v1alpha1.ProviderConfigUsage{}),
	}
	opts := append(client.ReconcilerOptions(c), managed.WithManagementPolicies())
	r := managed.NewReconciler(fakeManager{client: k, scheme: s},
		resource.ManagedKind(v1alpha1.ClientGroupVersionKind), opts...)
	return &harness{kube: k, r: r, key: kube.ObjectKeyFromObject(cr)}
}

// reconcile runs n reconciles and returns the Client as stored afterwards, or
// nil once it is gone.
func (h *harness) reconcile(t *testing.T, n int) *v1alpha1.Client {
	t.Helper()
	for range n {
		// Errors surface in the conditions, which the callers check.
		_, _ = h.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: h.key})
	}
	got := &v1alpha1.Client{}
	if err := h.kube.Get(context.Background(), h.key, got); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("get the Client: %v", err)
	}
	return got
}

func (h *harness) connectionSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	err := h.kube.Get(context.Background(), kube.ObjectKey{Namespace: h.key.Namespace, Name: "app-oidc"}, s)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get the connection Secret: %v", err)
	}
	return s
}

func conditionStatus(cr *v1alpha1.Client, ct xpv2.ConditionType) string {
	return string(cr.GetCondition(ct).Status)
}

func TestReconcilerCreateUpdateReady(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))

	got := h.reconcile(t, 5)

	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("Ready = %q, conditions %+v; want True", conditionStatus(got, xpv2.TypeReady), got.Status.Conditions)
	}
	if got.Status.Ownership == nil || got.Status.Ownership.ClientID != testID {
		t.Errorf("status.ownership = %+v, want client %q", got.Status.Ownership, testID)
	}
	if c := f.stored(); c == nil || c.AccessTokenLifetime != 600 {
		t.Errorf("Rauthy client = %+v, want accessTokenLifetime 600", c)
	}
	s := h.connectionSecret(t)
	if s == nil || string(s.Data[client.ConnectionKeyClientSecret]) != fakeSecret ||
		string(s.Data[client.ConnectionKeyClientID]) != testID {
		t.Errorf("connection Secret = %+v, want the client id and secret", s)
	}
}

func TestReconcilerFailedPutConverges(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.failPut = true
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))

	got := h.reconcile(t, 5)
	if conditionStatus(got, xpv2.TypeReady) == "True" || conditionStatus(got, xpv2.TypeSynced) != "False" {
		t.Fatalf("with the PUT failing: conditions %+v; want not Ready and not Synced", got.Status.Conditions)
	}
	if got.Status.Ownership == nil || f.stored() == nil {
		t.Fatalf("after the POST the resource must own the client: ownership %+v", got.Status.Ownership)
	}

	f.failPut = false
	got = h.reconcile(t, 3)
	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Errorf("after the PUT recovered: conditions %+v; want Ready", got.Status.Conditions)
	}
	if c := f.stored(); c.AccessTokenLifetime != 600 {
		t.Errorf("accessTokenLifetime in Rauthy = %d, want 600", c.AccessTokenLifetime)
	}
}

func TestReconcilerRejectsForgedOwnership(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	foreign := existing(true)
	f.put(foreign)
	// A fresh manifest carrying every annotation a successful create leaves,
	// and asking for different redirect URIs than the foreign client has.
	h := newHarness(t, f, newClient(forged, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.RedirectURIs = []string{"https://attacker.example.com/cb"}
	}))

	got := h.reconcile(t, 5)

	if conditionStatus(got, xpv2.TypeReady) == "True" || conditionStatus(got, xpv2.TypeSynced) != "False" {
		t.Errorf("conditions %+v; want not Ready and not Synced", got.Status.Conditions)
	}
	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "adoption is not supported") {
		t.Errorf("Synced message = %q, want an adoption error", msg)
	}
	if got.Status.Ownership != nil {
		t.Errorf("status.ownership = %+v, want none", got.Status.Ownership)
	}
	if s := h.connectionSecret(t); s != nil && len(s.Data[client.ConnectionKeyClientSecret]) > 0 {
		t.Error("the foreign client's secret was published")
	}

	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete the Client: %v", err)
	}
	if after := h.reconcile(t, 3); after != nil {
		t.Errorf("the Client was not finalized: %+v", after.Status.Conditions)
	}

	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("the foreign client changed: -want, +got:\n%s", diff)
	}
	for _, c := range f.callLog() {
		if c != "GET /clients/app" && c != "POST /clients" {
			t.Errorf("unexpected call to Rauthy: %s", c)
		}
	}
}
