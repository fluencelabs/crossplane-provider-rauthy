package client_test

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// Two Clients from an earlier release record the same public client. Until
// both are bound to a backend, neither may delete it.
func TestLegacyDuplicateRecordBlocksDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeRauthy(t)
	f.put(existing(false))
	first := newClient(owned, public, deleting)
	first.SetUID(types.UID("first"))
	second := newClient(owned, public)
	second.Namespace = "other"
	second.SetUID(types.UID("second"))
	k := newKube(t, f, first, second)
	for _, cr := range []*v1alpha1.Client{first, second} {
		stored := &v1alpha1.Client{}
		if err := k.Get(ctx, kube.ObjectKeyFromObject(cr), stored); err != nil {
			t.Fatal(err)
		}
		stored.Status.Ownership.BackendURL = ""
		if err := k.Status().Update(ctx, stored); err != nil {
			t.Fatal(err)
		}
	}
	// A refused delete leaves Rauthy untouched; the error, if any, is not the point.
	_, _ = client.NewExternal(f.api(t), k.Status(), k, testIssuer).Delete(ctx, first)
	if f.stored() == nil {
		t.Fatal("the client was deleted while another legacy record names it")
	}
}
