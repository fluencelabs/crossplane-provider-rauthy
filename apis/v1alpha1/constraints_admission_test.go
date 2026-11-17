package v1alpha1_test

import (
	"encoding/json"
	"os"
	"testing"
)

// Client rows from rauthy-go v0.2.0 testdata/constraints-cases.json, whose
// expected values follow live Rauthy 0.36.2 and 0.37.2 results, except
// comma-containing redirect URIs, which cannot survive CSV storage intact.
func TestCRDAdmitsSDKConstraints(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/constraints-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID       string `json:"id"`
		Field    string `json:"field"`
		Value    string `json:"value"`
		Expected bool   `json:"expected"`
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{
		"name":             "name",
		"contacts":         "contacts",
		"flows_enabled":    "flowsEnabled",
		"access_token_alg": "accessTokenAlg",
		"allowed_origins":  "allowedOrigins",
		"redirect_uris":    "redirectUris",
	}
	for _, tc := range cases {
		t.Run(tc.ID, func(t *testing.T) {
			t.Parallel()
			field, ok := fields[tc.Field]
			if !ok {
				t.Fatalf("unmapped SDK field %q", tc.Field)
			}
			var value any = tc.Value
			if field != "name" && field != "accessTokenAlg" {
				value = []any{tc.Value}
			}
			errs := admit(t, minimalForProvider(map[string]any{field: value}))
			if (len(errs) == 0) != tc.Expected {
				t.Fatalf("admit %s=%q: %v, want accepted=%v", field, tc.Value, errs, tc.Expected)
			}
		})
	}
}

func TestCRDRejectsCommaRedirectURIs(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"redirectUris", "postLogoutRedirectUris"} {
		for _, uri := range []string{"https://app.example/a,b", "https://app.example/a,"} {
			if errs := admit(t, minimalForProvider(map[string]any{field: []any{uri}})); len(errs) == 0 {
				t.Errorf("%s admitted comma-containing URI %q", field, uri)
			}
		}
		if errs := admit(
			t,
			minimalForProvider(map[string]any{field: []any{"https://app.example/a%2Cb"}}),
		); len(
			errs,
		) != 0 {
			t.Errorf("%s rejected encoded comma: %v", field, errs)
		}
	}
}
