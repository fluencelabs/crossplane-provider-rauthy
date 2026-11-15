package client_test

import (
	"context"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	return cr
}

// owned marks the resource as having created its client, the way the runtime
// and Create leave it after a successful create.
func owned(cr *v1alpha1.Client) {
	t := time.Now().Add(-time.Minute)
	meta.SetExternalCreatePending(cr, t)
	meta.SetExternalCreateSucceeded(cr, t)
	meta.AddAnnotations(cr, map[string]string{client.AnnotationKeyCreatedClientID: meta.GetExternalName(cr)})
}

func public(cr *v1alpha1.Client) { cr.Spec.ForProvider.Confidential = false }

func deleting(cr *v1alpha1.Client) {
	now := metav1.Now()
	cr.SetDeletionTimestamp(&now)
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
