package client_test

import (
	"context"
	"strings"
	"testing"

	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

func TestBackendBindingPrunedCRD(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"backend field pruned", "ownership pruned", "concurrent record"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newFakeRauthy(t)
			cr := newClient(owned, public)
			k := newKube(t, f, cr)
			if err := k.Get(ctx, kube.ObjectKeyFromObject(cr), cr); err != nil {
				t.Fatal(err)
			}
			cr.Status.Ownership.Backend = ""
			if err := k.Status().Update(ctx, cr); err != nil {
				t.Fatal(err)
			}
			watcher, ok := k.(kube.WithWatch)
			if !ok {
				t.Fatal("fake client does not support watch")
			}
			k = interceptor.NewClient(watcher, prunedBindingReply(t, mode))
			_, err := client.NewExternal(f.api(t), k.Status(), k, testIssuer).Observe(ctx, cr)
			want := "CRD"
			if mode == "concurrent record" {
				want = "ownership record changed"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Observe error = %v, want %q", err, want)
			}
			if len(f.callLog()) != 0 {
				t.Fatalf("Rauthy called despite failed binding: %v", f.callLog())
			}
		})
	}
}

func prunedBindingReply(t *testing.T, mode string) interceptor.Funcs {
	t.Helper()
	return interceptor.Funcs{SubResourcePatch: func(
		ctx context.Context, c kube.Client, _ string, obj kube.Object, _ kube.Patch,
		_ ...kube.SubResourcePatchOption,
	) error {
		// Answer with the stored record, as a pruning CRD does.
		if err := c.Get(ctx, kube.ObjectKeyFromObject(obj), obj); err != nil {
			return err
		}
		answer, ok := obj.(*v1alpha1.Client)
		if !ok {
			t.Fatal("status patch target is not a Client")
		}
		switch mode {
		case "ownership pruned":
			answer.Status.Ownership = nil
		case "concurrent record":
			answer.Status.Ownership.ClientID = "other-client"
		}
		return nil
	}}
}
