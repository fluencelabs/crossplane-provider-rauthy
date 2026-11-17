package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

const (
	testID     = "app"
	testIssuer = "https://auth.example.com/auth/v1"
)

// newClient returns a confidential Client named testID, as the runtime hands
// it to the external client: external name defaulted to metadata.name and
// the default management policies.
func newClient(mods ...func(*v1alpha1.Client)) *v1alpha1.Client {
	cr := &v1alpha1.Client{
		Name:      testID,
		Namespace: "apps",
		Spec: v1alpha1.ClientSpec{
			ManagedResourceSpec: xpv2.ManagedResourceSpec{
				ManagementPolicies: xpv2.ManagementPolicies{xpv2.ManagementActionAll},
			},
			ForProvider: v1alpha1.ClientParameters{
				Name:         new("My App"),
				Confidential: true,
				RedirectURIs: []string{"https://app.example.com/callback", "https://app.example.com/alt"},
			},
		},
	}
	meta.SetExternalName(cr, testID)
	for _, m := range mods {
		m(cr)
	}
	// owned may run before public: a public client's record has no
	// fingerprint.
	if cr.Status.Ownership != nil && !cr.Spec.ForProvider.Confidential {
		cr.Status.Ownership.SecretFingerprint = ""
	}
	return cr
}

// owned records in status that the resource created its client, as Create
// leaves it.
//
// A confidential client is bound to fakeSecret, the secret the fake gives
// clients stored with put.
func owned(cr *v1alpha1.Client) {
	cr.Status.Ownership = &v1alpha1.ClientOwnership{
		ClientID:  meta.GetExternalName(cr),
		CreatedAt: metav1.NewTime(time.Now().Add(-time.Minute)),
	}
	if cr.Spec.ForProvider.Confidential {
		cr.Status.Ownership.SecretFingerprint = fingerprintOf(meta.GetExternalName(cr), fakeSecret)
	}
}

// fingerprintOf computes the documented binding independently of the code
// under test: hex SHA-256 of "rauthy-client-secret-v1", id and secret joined
// by NUL bytes.
func fingerprintOf(id, secret string) string {
	sum := sha256.Sum256([]byte("rauthy-client-secret-v1\x00" + id + "\x00" + secret))
	return hex.EncodeToString(sum[:])
}

// forged puts on the object every annotation a successful create leaves,
// including the one earlier versions trusted, as a user writing the manifest
// could. None of them may grant ownership.
func forged(cr *v1alpha1.Client) {
	t := time.Now().Add(-time.Minute)
	meta.SetExternalCreatePending(cr, t)
	meta.SetExternalCreateSucceeded(cr, t)
	meta.AddAnnotations(cr, map[string]string{"rauthy.fluence.dev/created-client-id": meta.GetExternalName(cr)})
}

// newKube returns a fake API server holding the given Clients, with the
// status subresource enabled as on a real cluster.
func newKube(t *testing.T, f *fakeRauthy, objs ...*v1alpha1.Client) kube.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.Client{})
	for _, o := range objs {
		bindBackend(f.srv.URL+"/auth/v1", o)
		b = b.WithObjects(o.DeepCopy())
	}
	return b.Build()
}

// newExternal returns the external client for the fake Rauthy, writing
// status to a fake API server that holds cr.
func newExternal(t *testing.T, f *fakeRauthy, cr *v1alpha1.Client) managed.TypedExternalClient[*v1alpha1.Client] {
	t.Helper()
	k := newKube(t, f, cr)
	return client.NewExternal(f.api(t), k.Status(), k, testIssuer)
}

func public(cr *v1alpha1.Client) { cr.Spec.ForProvider.Confidential = false }

func deleting(cr *v1alpha1.Client) {
	now := metav1.Now()
	cr.SetDeletionTimestamp(&now)
	cr.SetFinalizers([]string{managed.FinalizerName})
}

func policies(p ...xpv2.ManagementAction) func(*v1alpha1.Client) {
	return func(cr *v1alpha1.Client) { cr.Spec.ManagementPolicies = p }
}

// runtimeCreate drives Create the way the managed reconciler does: it marks
// the attempt pending, calls Create and records success or failure.
func runtimeCreate(t *testing.T, e managed.TypedExternalClient[*v1alpha1.Client], cr *v1alpha1.Client) error {
	t.Helper()
	meta.SetExternalCreatePending(cr, time.Now())
	_, err := e.Create(context.Background(), cr)
	if err != nil {
		meta.SetExternalCreateFailed(cr, time.Now())
		return err
	}
	meta.SetExternalCreateSucceeded(cr, time.Now())
	return nil
}

func readyStatus(cr *v1alpha1.Client) string {
	return string(cr.GetCondition(xpv2.TypeReady).Status)
}

// bindBackend supplies the instance binding of pre-existing ownership fixtures.
func bindBackend(url string, cr *v1alpha1.Client) {
	if cr.Status.Ownership != nil && cr.Status.Ownership.BackendURL == "" {
		cr.Status.Ownership.BackendURL = url
	}
}
