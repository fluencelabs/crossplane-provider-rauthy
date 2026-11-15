package client_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"k8s.io/apimachinery/pkg/types"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// prunedRealDecode answers a status patch the way a real API server with a
// CRD that prunes status.ownership does, decoded the way the real typed
// client decodes it: the answer, which has no "ownership" key, is unmarshalled
// into the object passed in without zeroing it first. Fields absent from the
// answer keep whatever the object held. controller-runtime's fake client
// zeroes the object instead, which hides that difference.
func prunedRealDecode() interceptor.Funcs {
	return interceptor.Funcs{SubResourcePatch: func(
		ctx context.Context, c kube.Client, sub string, obj kube.Object, _ kube.Patch,
		opts ...kube.SubResourcePatchOption,
	) error {
		stored, ok := obj.DeepCopyObject().(kube.Object)
		if !ok {
			panic("not a kube.Object")
		}
		if err := c.SubResource(sub).Patch(ctx, stored,
			kube.RawPatch(types.MergePatchType, []byte("{}")), opts...); err != nil {
			return err
		}
		answer, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		return json.Unmarshal(answer, obj)
	}}
}

// TestReconcilerCRDWithoutOwnershipRealDecode: the pruning CRD is detected
// from what the API server stored, not from the marker the patched object
// already held before the answer was decoded into it.
func TestReconcilerCRDWithoutOwnershipRealDecode(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(), prunedRealDecode())

	got := h.reconcile(t, 3)

	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "does not keep status.ownership") {
		t.Errorf("Synced message = %q, want the CRD named", msg)
	}
	if f.stored() != nil {
		t.Error("the unrecorded client was left in Rauthy")
	}
	posts := 0
	for _, c := range f.callLog() {
		if c == "POST /clients" {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("%d creates, want 1", posts)
	}
}
