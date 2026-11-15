package client_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

const otherFinalizer = "example.com/other-controller"

// TestStatusPatchKeepsConflictProtection: the stored Client changes (a spec
// edit and a finalizer another controller adds) after the reconciler read it
// and before an ownership patch. The runtime's next Update of its copy must
// then either carry the server's current spec and finalizers or fail with a
// conflict; it must never write the stale ones back.
func TestStatusPatchKeepsConflictProtection(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		cr    *v1alpha1.Client
		setup func(*fakeRauthy)
		act   func(context.Context, managed.TypedExternalClient[*v1alpha1.Client], *v1alpha1.Client) error
	}{
		"RecordOnCreate": {
			cr: newClient(),
			act: func(ctx context.Context, e managed.TypedExternalClient[*v1alpha1.Client], cr *v1alpha1.Client) error {
				_, err := e.Create(ctx, cr)
				return err
			},
		},
		"ClearStaleMarkerInObserve": {
			cr: newClient(owned),
			act: func(ctx context.Context, e managed.TypedExternalClient[*v1alpha1.Client], cr *v1alpha1.Client) error {
				_, err := e.Observe(ctx, cr)
				return err
			},
		},
		"ClearOnDelete": {
			cr:    newClient(owned, deleting),
			setup: func(f *fakeRauthy) { f.put(existing(true)) },
			act: func(ctx context.Context, e managed.TypedExternalClient[*v1alpha1.Client], cr *v1alpha1.Client) error {
				_, err := e.Delete(ctx, cr)
				return err
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkNoStaleWrite(t, tc.cr, tc.setup, tc.act)
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// checkNoStaleWrite runs act on a copy of want read before somebody else
// edits the stored object, then has the runtime update that copy.
func checkNoStaleWrite(
	t *testing.T,
	want *v1alpha1.Client,
	setup func(*fakeRauthy),
	act func(context.Context, managed.TypedExternalClient[*v1alpha1.Client], *v1alpha1.Client) error,
) {
	t.Helper()
	ctx := context.Background()
	f := newFakeRauthy(t)
	if setup != nil {
		setup(f)
	}
	k := newKube(t, want)
	e := client.NewExternal(f.api(t), k.Status(), k, testIssuer)

	// The reconciler's read.
	cr := &v1alpha1.Client{}
	if err := k.Get(ctx, kube.ObjectKeyFromObject(want), cr); err != nil {
		t.Fatal(err)
	}
	// Somebody else writes the object in between.
	edited := cr.DeepCopy()
	edited.Spec.ForProvider.Name = new("Edited Name")
	edited.SetFinalizers(append(edited.GetFinalizers(), otherFinalizer))
	if err := k.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}

	if err := act(ctx, e, cr); err != nil {
		t.Fatalf("external call: %v", err)
	}

	err := k.Update(ctx, cr)
	if kerrors.IsConflict(err) {
		return
	}
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := &v1alpha1.Client{}
	if err = k.Get(ctx, kube.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatal(err)
	}
	if n := got.Spec.ForProvider.Name; n == nil || *n != "Edited Name" {
		t.Errorf("the stale spec was written back: name = %q", deref(n))
	}
	if !slices.Contains(got.GetFinalizers(), otherFinalizer) {
		t.Errorf("the other controller's finalizer was dropped: %v", got.GetFinalizers())
	}
}
