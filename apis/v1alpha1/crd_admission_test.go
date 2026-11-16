package v1alpha1_test

import (
	"context"
	"maps"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/model"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/common"
)

// admit validates a Client spec.forProvider against the generated CRD schema
// the way the API server does on create, CEL rules included, and returns the
// errors.
func admit(t *testing.T, forProvider map[string]any) field.ErrorList {
	t.Helper()
	return admitObject(t, forProviderObject(forProvider), nil)
}

func admitObject(t *testing.T, obj, old map[string]any) field.ErrorList {
	t.Helper()
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		clientSchema(t), &internal, nil); err != nil {
		t.Fatalf("convert the Client schema: %v", err)
	}
	v, _, err := validation.NewSchemaValidator(&internal)
	if err != nil {
		t.Fatalf("build the Client schema validator: %v", err)
	}
	structural, err := schema.NewStructural(&internal)
	if err != nil {
		t.Fatalf("build the structural Client schema: %v", err)
	}
	// The API server prunes non-nullable nulls before schema and CEL validation.
	defaulting.PruneNonNullableNullsWithoutDefaults(obj, structural)
	var errs field.ErrorList
	var opts []cel.Option
	if old == nil {
		errs = validation.ValidateCustomResource(nil, obj, v)
	} else {
		correlation := common.NewCorrelatedObject(obj, old, &model.Structural{Structural: structural})
		errs = validation.ValidateCustomResourceUpdate(nil, obj, old, v, validation.WithRatcheting(correlation))
		opts = append(opts, cel.WithRatcheting(correlation))
	}
	if rules := cel.NewValidator(structural, true, celconfig.PerCallLimit); rules != nil {
		celErrs, _ := rules.Validate(
			context.Background(), nil, structural, obj, old, celconfig.RuntimeCELCostBudget, opts...,
		)
		errs = append(errs, celErrs...)
	}
	return errs
}

func forProviderObject(fp map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "rauthy.fluence.dev/v1alpha1",
		"kind":       "Client",
		"metadata":   map[string]any{"name": "app", "namespace": "apps"},
		"spec":       map[string]any{"forProvider": fp},
	}
}

func minimalForProvider(extra map[string]any) map[string]any {
	fp := map[string]any{
		"confidential": true,
		"redirectUris": []any{"https://app.example.com/callback"},
	}
	maps.Copy(fp, extra)
	return fp
}

// Rauthy replaces an empty scope list with its default and refuses an empty
// flow list, so none of these may be submitted empty; a non-empty value and
// leaving the field out are both admitted.
func TestCRDRejectsEmptyRequiredLists(t *testing.T) {
	t.Parallel()
	if errs := admit(t, minimalForProvider(nil)); len(errs) != 0 {
		t.Fatalf("a minimal spec is rejected: %v", errs)
	}
	nonEmpty := map[string]any{
		"scopes":        []any{"openid"},
		"defaultScopes": []any{"openid"},
		"flowsEnabled":  []any{"authorization_code"},
	}
	for field, value := range nonEmpty {
		if errs := admit(t, minimalForProvider(map[string]any{field: value})); len(errs) != 0 {
			t.Errorf("%s: %v is rejected: %v", field, value, errs)
		}
		if errs := admit(t, minimalForProvider(map[string]any{field: []any{}})); len(errs) == 0 {
			t.Errorf("%s: [] is admitted, want it rejected", field)
		}
	}
}

// A public client has no secret, so PKCE with S256 is what protects its
// authorization codes: when its challenge methods are set they must include
// S256. Leaving them unset keeps what Rauthy holds and stays allowed; a
// confidential client may set any list.
func TestCRDPublicClientKeepsS256(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		extra      map[string]any
		wantErrors bool
	}{
		{"public []", map[string]any{"confidential": false, "challenges": []any{}}, true},
		{"public [plain]", map[string]any{"confidential": false, "challenges": []any{"plain"}}, true},
		{"public [S256]", map[string]any{"confidential": false, "challenges": []any{"S256"}}, false},
		{"public [S256, plain]", map[string]any{"confidential": false, "challenges": []any{"S256", "plain"}}, false},
		{"public null", map[string]any{"confidential": false, "challenges": nil}, false},
		{"public unset", map[string]any{"confidential": false}, false},
		{"confidential []", map[string]any{"confidential": true, "challenges": []any{}}, false},
	}
	for _, c := range cases {
		errs := admit(t, minimalForProvider(c.extra))
		if got := len(errs) != 0; got != c.wantErrors {
			t.Errorf("%s: errors %v, want rejected=%t", c.name, errs, c.wantErrors)
		}
	}
}

func TestCRDPublicClientLegacyRatcheting(t *testing.T) {
	t.Parallel()
	for name, methods := range map[string][]any{"empty": {}, "plain": {"plain"}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			old := forProviderObject(minimalForProvider(map[string]any{"confidential": false, "challenges": methods}))
			cases := []struct {
				name       string
				edit       func(map[string]any)
				wantErrors bool
			}{
				{"metadata", func(obj map[string]any) {
					obj["metadata"].(map[string]any)["labels"] = map[string]any{"edited": "true"}
				}, false},
				{"finalizer", func(obj map[string]any) {
					obj["metadata"].(map[string]any)["finalizers"] = []any{"finalizer.managedresource.crossplane.io"}
				}, false},
				{
					"status",
					func(obj map[string]any) { obj["status"] = map[string]any{"atProvider": map[string]any{"id": "app"}} },
					false,
				},
				{"unrelated forProvider", func(obj map[string]any) {
					obj["spec"].(map[string]any)["forProvider"].(map[string]any)["name"] = "edited"
				}, true},
				{"repaired forProvider", func(obj map[string]any) {
					fp := obj["spec"].(map[string]any)["forProvider"].(map[string]any)
					fp["name"] = "edited"
					fp["challenges"] = []any{"S256"}
				}, false},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					obj := runtime.DeepCopyJSON(old)
					tc.edit(obj)
					if errs := admitObject(t, obj, old); (len(errs) != 0) != tc.wantErrors {
						t.Errorf("errors %v, want rejected=%t", errs, tc.wantErrors)
					}
				})
			}
		})
	}
}
