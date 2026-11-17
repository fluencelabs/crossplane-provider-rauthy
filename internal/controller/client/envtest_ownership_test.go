//go:build envtest

package client_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// connectEnv connects to the setup's Rauthy the way Setup wires the
// connector: the manager's cached, indexed client and its uncached reader.
func connectEnv(
	t *testing.T, mgr ctrl.Manager, reader kube.Reader, cr *v1alpha1.Client,
) managed.TypedExternalClient[*v1alpha1.Client] {
	t.Helper()
	ctx := context.Background()
	pc := &v1alpha1.ClusterProviderConfig{}
	eventually(t, "the provider config in the cache", func() bool {
		return mgr.GetClient().Get(ctx, kube.ObjectKey{Name: cr.Spec.ProviderConfigReference.Name}, pc) == nil
	})
	c := &client.Connector{
		Kube:   mgr.GetClient(),
		Reader: reader,
		Usage:  resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
	}
	e, err := c.Connect(ctx, cr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return e
}

// createLikeRuntime runs one create the way the managed reconciler does,
// against the API server: mark the create pending with an Update, Create,
// then mark it succeeded with an Update of the very object Create was given.
func createLikeRuntime(t *testing.T, e managed.TypedExternalClient[*v1alpha1.Client], cr *v1alpha1.Client) {
	t.Helper()
	ctx := context.Background()
	if obs, err := e.Observe(ctx, cr); err != nil || obs.ResourceExists {
		t.Fatalf("observe before the create: %+v, %v; want not existing", obs, err)
	}
	meta.SetExternalCreatePending(cr, time.Now())
	if err := envKube.Update(ctx, cr); err != nil {
		t.Fatalf("mark the create pending: %v", err)
	}
	if _, err := e.Create(ctx, cr); err != nil {
		t.Fatalf("create: %v", err)
	}
	meta.SetExternalCreateSucceeded(cr, time.Now())
	if err := envKube.Update(ctx, cr); err != nil {
		t.Fatalf("the Update after the create answered %v; the status patch must leave the object current", err)
	}
}

// TestEnvCreateRecordsOwnership: the status patch records ownership on a real
// API server, its answer leaves the object at the stored resourceVersion (the
// runtime's Update right after the create does not conflict), and the update
// does not drop the record.
func TestEnvCreateRecordsOwnership(t *testing.T) {
	t.Parallel()
	mgr := startManager(t, "", nil)
	s := newEnvSetup(t, envKube)
	cr := s.client(testID)
	mustCreate(t, envKube, cr)
	cr = stored(t, cr)

	e := connectEnv(t, mgr, mgr.GetAPIReader(), cr)
	createLikeRuntime(t, e, cr)

	got := stored(t, cr)
	want := fingerprintOf(s.id, s.secret())
	if o := got.Status.Ownership; o == nil || o.ClientID != s.id || o.SecretFingerprint != want {
		t.Errorf("status.ownership = %+v, want client %q bound to its secret", o, s.id)
	}
	if meta.GetExternalCreateSucceeded(got).IsZero() {
		t.Error("the create-succeeded annotation was not stored")
	}
}

// TestEnvOtherRecordFromCache: the field index on status.ownership.clientID
// works in a real cache, observation looks other records up there only, and a
// record another Client holds for the id refuses this one once the cache has
// it.
func TestEnvOtherRecordFromCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := startManager(t, "", nil)
	s := newEnvSetup(t, envKube)
	cr := s.client(testID)
	mustCreate(t, envKube, cr)
	cr = stored(t, cr)
	uncached := &countingReader{Reader: mgr.GetAPIReader()}
	e := connectEnv(t, mgr, uncached, cr)
	createLikeRuntime(t, e, cr)

	cr = stored(t, cr)
	uncached.lists.Store(0)
	if obs, err := e.Observe(ctx, cr); err != nil || !obs.ResourceExists {
		t.Fatalf("observe the owned client: %+v, %v; want it existing", obs, err)
	}
	if n := uncached.lists.Load(); n != 0 {
		t.Errorf("observing listed Clients without the cache %d times, want 0", n)
	}

	// A Client in another namespace that records the same id.
	other := &corev1.Namespace{Name: s.ns + "-b"}
	mustCreate(t, envKube, other)
	b := s.client(testID)
	b.Namespace = other.Name
	mustCreate(t, envKube, b)
	b = stored(t, b)
	b.Status.Ownership = &v1alpha1.ClientOwnership{ClientID: s.id, CreatedAt: metav1.Now()}
	if err := envKube.Status().Update(ctx, b); err != nil {
		t.Fatalf("record the id on the other Client: %v", err)
	}

	eventually(t, "the other record to refuse the owner", func() bool {
		_, err := e.Observe(ctx, stored(t, cr))
		return errors.Is(err, client.ErrAdoption)
	})
	if n := uncached.lists.Load(); n != 0 {
		t.Errorf("observing listed Clients without the cache %d times, want 0", n)
	}
	if s.stored() == nil {
		t.Error("the Rauthy client is gone")
	}
}
