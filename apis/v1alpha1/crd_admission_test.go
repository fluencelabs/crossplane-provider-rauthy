package v1alpha1_test

import (
	"maps"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// admit validates a Client spec.forProvider against the generated CRD schema
// the way the API server does on create, and returns the errors.
func admit(t *testing.T, forProvider map[string]any) field.ErrorList {
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
	obj := map[string]any{
		"apiVersion": "rauthy.fluence.dev/v1alpha1",
		"kind":       "Client",
		"metadata":   map[string]any{"name": "app", "namespace": "apps"},
		"spec":       map[string]any{"forProvider": forProvider},
	}
	return validation.ValidateCustomResource(nil, obj, v)
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
