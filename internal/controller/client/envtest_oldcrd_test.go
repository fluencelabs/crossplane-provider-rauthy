//go:build envtest

package client_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// olderCRDs returns the package's CRDs with status.ownership removed from the
// Client schema, as a Client CRD from before the field existed has it.
func olderCRDs(t *testing.T) []*apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(crdDir(), "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list the CRDs: %v", err)
	}
	var out []*apiextensionsv1.CustomResourceDefinition
	for _, f := range files {
		raw, readErr := os.ReadFile(f)
		if readErr != nil {
			t.Fatal(readErr)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if readErr = yaml.Unmarshal(raw, crd); readErr != nil {
			t.Fatal(readErr)
		}
		if crd.Spec.Names.Kind == v1alpha1.ClientKind {
			status := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"]
			delete(status.Properties, "ownership")
			crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = status
		}
		out = append(out, crd)
	}
	return out
}

// TestEnvOlderCRDRefusesCreation: with a Client CRD that has no
// status.ownership, a real API server accepts the status patch that records
// the pending creation proof and prunes the field. The provider notices from
// the answer, before any POST, and refuses to create: Rauthy receives no
// create call, the API server holds no ownership record, the Client does not
// become Ready, and every later create is refused naming the stale CRD.
func TestEnvOlderCRDRefusesCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := &envtest.Environment{CRDs: olderCRDs(t)}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start a second API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	kc, err := kube.New(cfg, kube.Options{Scheme: envScheme()})
	if err != nil {
		t.Fatal(err)
	}
	s := newEnvSetup(t, kc)
	cr := s.client(testID)
	mustCreate(t, kc, cr)
	cr = storedIn(t, kc, cr)

	c := &client.Connector{Kube: kc, Usage: resource.NewProviderConfigUsageTracker(kc, &v1alpha1.ProviderConfigUsage{})}
	e, err := c.Connect(ctx, cr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err = e.Create(ctx, cr); err == nil || !strings.Contains(err.Error(), "does not keep status.ownership") {
		t.Fatalf("create answered %v, want the CRD named", err)
	}
	if _, err = e.Create(ctx, cr); err == nil || !strings.Contains(err.Error(), "does not keep status.ownership") {
		t.Errorf("a second create answered %v, want it refused", err)
	}
	posts := 0
	for _, call := range s.f.callLog() {
		if call == "POST /clients" {
			posts++
		}
	}
	if posts != 0 {
		t.Errorf("%d creates, want 0", posts)
	}
	if s.stored() != nil {
		t.Error("a client was created in Rauthy")
	}
	got := storedIn(t, kc, cr)
	if got.Status.Ownership != nil {
		t.Errorf("the API server holds status.ownership %+v under a CRD without it", got.Status.Ownership)
	}
	if c := got.GetCondition(xpv2.TypeReady); c.Status == corev1.ConditionTrue {
		t.Errorf("Ready = %+v, want the refused Client not Ready", c)
	}

	// The typed client resets its target before decoding the answer: a
	// marker held only locally does not survive a patch whose answer lacks
	// it. patchOwnership decodes into a fresh object and does not rely on it.
	cr = storedIn(t, kc, cr)
	cr.Status.Ownership = &v1alpha1.ClientOwnership{ClientID: s.id, CreatedAt: metav1.Now()}
	if err = kc.Status().Patch(ctx, cr, kube.RawPatch(types.MergePatchType, []byte(`{"status":{}}`))); err != nil {
		t.Fatalf("patch the status: %v", err)
	}
	if cr.Status.Ownership != nil {
		t.Errorf("after a patch answer without status.ownership the object still holds %+v", cr.Status.Ownership)
	}
}
