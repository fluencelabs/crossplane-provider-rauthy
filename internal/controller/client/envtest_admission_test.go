//go:build envtest

package client_test

import (
	"context"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// TestEnvAdmission: the CRD's CEL rules, minItems and enums are enforced by
// the API server, and status cannot be written through the main resource.
func TestEnvAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newEnvSetup(t, envKube)

	// Written as manifests are, not through the Go types: scopes,
	// defaultScopes and flowsEnabled carry omitempty, so a typed [] is never
	// sent.
	refused := map[string]struct {
		path  []string
		value any
	}{
		"namespaced provider config": {[]string{"spec", "providerConfigRef", "kind"}, "ProviderConfig"},
		"empty redirectUris":         {[]string{"spec", "forProvider", "redirectUris"}, []any{}},
		"empty scopes":               {[]string{"spec", "forProvider", "scopes"}, []any{}},
		"empty defaultScopes":        {[]string{"spec", "forProvider", "defaultScopes"}, []any{}},
		"empty flowsEnabled":         {[]string{"spec", "forProvider", "flowsEnabled"}, []any{}},
		"unknown challenge":          {[]string{"spec", "forProvider", "challenges"}, []any{"S512"}},
	}
	for name, c := range refused {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s.client("refused"))
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: raw}
		u.SetGroupVersionKind(v1alpha1.ClientGroupVersionKind)
		if err = unstructured.SetNestedField(u.Object, c.value, c.path...); err != nil {
			t.Fatal(err)
		}
		if err = envKube.Create(ctx, u); !kerrors.IsInvalid(err) {
			t.Errorf("%s: create answered %v, want Invalid", name, err)
		}
	}

	cr := s.client("app", owned)
	mustCreate(t, envKube, cr)
	got := stored(t, cr)
	if got.Status.Ownership != nil {
		t.Errorf("a create stored status.ownership %+v, want it ignored", got.Status.Ownership)
	}

	got.Spec.ForProvider.Confidential = false
	if err := envKube.Update(ctx, got); !kerrors.IsInvalid(err) {
		t.Errorf("switching confidential answered %v, want Invalid", err)
	}

	got = stored(t, cr)
	owned(got)
	if err := envKube.Update(ctx, got); err != nil {
		t.Fatalf("update the Client: %v", err)
	}
	if after := stored(t, cr); after.Status.Ownership != nil {
		t.Errorf("an update of the main resource stored status.ownership %+v, want it ignored", after.Status.Ownership)
	}
}

// TestEnvNullPrunedEmptyKept: the optional lists carry no omitempty, so an
// unset one is sent as null and an empty one as []. The API server drops the
// null, so the field reads back unset, and keeps the [], so an explicit
// "none" survives the runtime writing the object back.
func TestEnvNullPrunedEmptyKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newEnvSetup(t, envKube)
	cr := s.client("app", func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AllowedOrigins = []string{}
		cr.Spec.ForProvider.PostLogoutRedirectURIs = []string{}
		cr.Spec.ForProvider.Challenges = nil
		cr.Spec.ForProvider.Contacts = nil
	})
	mustCreate(t, envKube, cr)

	check := func(when string) {
		t.Helper()
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(v1alpha1.ClientGroupVersionKind)
		if err := envKube.Get(ctx, kube.ObjectKeyFromObject(cr), u); err != nil {
			t.Fatalf("get the Client: %v", err)
		}
		fp, _, _ := unstructured.NestedMap(u.Object, "spec", "forProvider")
		for _, f := range []string{"allowedOrigins", "postLogoutRedirectUris"} {
			if v, ok := fp[f].([]any); !ok || len(v) != 0 {
				t.Errorf("%s: %s = %#v, want []", when, f, fp[f])
			}
		}
		for _, f := range []string{"challenges", "contacts"} {
			if v, ok := fp[f]; ok {
				t.Errorf("%s: %s = %#v, want it pruned", when, f, v)
			}
		}
		got := stored(t, cr)
		if got.Spec.ForProvider.AllowedOrigins == nil || got.Spec.ForProvider.Challenges != nil {
			t.Errorf("%s: typed allowedOrigins %#v, challenges %#v; want [] and nil", when,
				got.Spec.ForProvider.AllowedOrigins, got.Spec.ForProvider.Challenges)
		}
	}
	check("after create")

	got := stored(t, cr)
	got.SetLabels(map[string]string{"touched": "yes"})
	if err := envKube.Update(ctx, got); err != nil {
		t.Fatalf("update the Client: %v", err)
	}
	check("after a write-back")
}
