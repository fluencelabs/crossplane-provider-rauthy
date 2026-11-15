package v1alpha1_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/fluencelabs/rauthy-go"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The Client CRD transcribes Rauthy's validation rules into markers. rauthy-go
// is the source of truth for them; this test fails when the two drift apart.
// Run `make generate` after changing a marker so the CRD is current.

func clientForProvider(t *testing.T) map[string]apiextensionsv1.JSONSchemaProps {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "package", "crds", "rauthy.fluence.dev_clients.yaml"))
	if err != nil {
		t.Fatalf("read the Client CRD: %v", err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err = yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parse the Client CRD: %v", err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("the Client CRD has %d versions, want 1", len(crd.Spec.Versions))
	}
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	return schema.Properties["spec"].Properties["forProvider"].Properties
}

func TestCRDPatternsMatchRauthy(t *testing.T) {
	t.Parallel()
	props := clientForProvider(t)

	scalar := map[string]string{
		"name":                 rauthy.ClientNamePattern().String(),
		"clientUri":            rauthy.URIPattern().String(),
		"backchannelLogoutUri": rauthy.URIPattern().String(),
		"restrictGroupPrefix":  rauthy.GroupPattern().String(),
	}
	for field, want := range scalar {
		if got := props[field].Pattern; got != want {
			t.Errorf("%s: CRD pattern %q, rauthy-go %q", field, got, want)
		}
	}

	items := map[string]string{
		"redirectUris":           rauthy.URIPattern().String(),
		"postLogoutRedirectUris": rauthy.URIPattern().String(),
		"allowedOrigins":         rauthy.OriginPattern().String(),
		"scopes":                 rauthy.ScopePattern().String(),
		"defaultScopes":          rauthy.ScopePattern().String(),
		"contacts":               rauthy.ContactPattern().String(),
	}
	for field, want := range items {
		p := props[field]
		if p.Items == nil || p.Items.Schema == nil {
			t.Errorf("%s: CRD has no item schema", field)
			continue
		}
		if got := p.Items.Schema.Pattern; got != want {
			t.Errorf("%s: CRD item pattern %q, rauthy-go %q", field, got, want)
		}
	}
}

func TestCRDEnumsMatchRauthy(t *testing.T) {
	t.Parallel()
	props := clientForProvider(t)

	scalar := map[string][]string{
		"accessTokenAlg": rauthy.SigningAlgs(),
		"idTokenAlg":     rauthy.SigningAlgs(),
	}
	for field, want := range scalar {
		if got := enumValues(t, props[field].Enum); !sameSet(got, want) {
			t.Errorf("%s: CRD enum %v, rauthy-go %v", field, got, want)
		}
	}

	items := map[string][]string{
		"flowsEnabled": rauthy.GrantTypes(),
		"challenges":   rauthy.ChallengeMethods(),
	}
	for field, want := range items {
		p := props[field]
		if p.Items == nil || p.Items.Schema == nil {
			t.Errorf("%s: CRD has no item schema", field)
			continue
		}
		if got := enumValues(t, p.Items.Schema.Enum); !sameSet(got, want) {
			t.Errorf("%s: CRD item enum %v, rauthy-go %v", field, got, want)
		}
	}
}

func TestCRDBoundsMatchRauthy(t *testing.T) {
	t.Parallel()
	props := clientForProvider(t)

	bounds := map[string][2]float64{
		"authCodeLifetime":    {rauthy.AuthCodeLifetimeMin, rauthy.AuthCodeLifetimeMax},
		"accessTokenLifetime": {rauthy.AccessTokenLifetimeMin, rauthy.AccessTokenLifetimeMax},
	}
	for field, want := range bounds {
		p := props[field]
		if p.Minimum == nil || *p.Minimum != want[0] {
			t.Errorf("%s: CRD minimum %s, rauthy-go %v", field, bound(p.Minimum), want[0])
		}
		if p.Maximum == nil || *p.Maximum != want[1] {
			t.Errorf("%s: CRD maximum %s, rauthy-go %v", field, bound(p.Maximum), want[1])
		}
	}
}

func bound(v *float64) string {
	if v == nil {
		return "unset"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func enumValues(t *testing.T, raw []apiextensionsv1.JSON) []string {
	t.Helper()
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := yaml.Unmarshal(r.Raw, &s); err != nil {
			t.Fatalf("enum value %s is not a string: %v", r.Raw, err)
		}
		out = append(out, s)
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// The ownership record is bound to the client's secret, so confidentiality
// must not change after creation.
func TestCRDConfidentialIsImmutable(t *testing.T) {
	t.Parallel()
	rules := clientForProvider(t)["confidential"].XValidations
	for _, r := range rules {
		if r.Rule == "self == oldSelf" {
			return
		}
	}
	t.Errorf("confidential has validations %+v, want the rule self == oldSelf", rules)
}
