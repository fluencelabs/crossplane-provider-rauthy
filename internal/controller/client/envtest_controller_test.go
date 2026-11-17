//go:build envtest

package client_test

import (
	"context"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// TestEnvControllerLifecycle runs the controller as Setup builds it against
// the API server: a new Client is created in Rauthy, brought to its spec with
// one update and published; polling keeps it in sync; deleting the
// Client deletes the Rauthy client.
func TestEnvControllerLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const pollInterval = time.Second
	flags := &feature.Flags{}
	flags.Enable(feature.EnableBetaManagementPolicies)
	s := newEnvSetup(t, envKube)
	// Limited to this test's namespace, so it leaves other tests' Clients
	// alone.
	startManager(t, s.ns, func(mgr ctrl.Manager) {
		if err := client.Setup(mgr, controller.Options{
			Logger:                  logging.NewNopLogger(),
			MaxConcurrentReconciles: 1,
			PollInterval:            pollInterval,
			GlobalRateLimiter:       ratelimiter.NewGlobal(10),
			Features:                flags,
		}); err != nil {
			t.Fatalf("set up the controller: %v", err)
		}
	})
	cr := s.client(testID, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
		cr.Spec.ForProvider.AllowedOrigins = []string{}
	})
	mustCreate(t, envKube, cr)

	eventually(t, "the Client to become Ready", func() bool {
		return conditionStatus(stored(t, cr), xpv2.TypeReady) == "True"
	})
	got := stored(t, cr)
	if got.Status.Ownership == nil || got.Status.Ownership.ClientID != s.id {
		t.Errorf("status.ownership = %+v, want client %q", got.Status.Ownership, s.id)
	}
	if c := s.stored(); c == nil || c.AccessTokenLifetime != 600 {
		t.Errorf("Rauthy client = %+v, want accessTokenLifetime 600", c)
	}
	secret := &corev1.Secret{}
	if err := envKube.Get(ctx, kube.ObjectKey{Namespace: s.ns, Name: testID + "-oidc"}, secret); err != nil {
		t.Fatalf("get the connection Secret: %v", err)
	}
	if string(secret.Data[client.ConnectionKeyClientSecret]) != s.secret() {
		t.Error("the connection Secret does not carry the client's secret")
	}

	puts := countCalls(s.f, "PUT /clients/"+s.id)
	time.Sleep(4 * time.Second)
	if n := countCalls(s.f, "PUT /clients/"+s.id); n != puts || n != 1 {
		t.Errorf("%d updates, %d of them while polling an up-to-date client; want 1 and none", n, n-puts)
	}
	if n := countCalls(s.f, "POST /clients"); n != 1 {
		t.Errorf("%d creates, want 1", n)
	}

	remote := s.stored()
	if remote == nil {
		t.Fatal("Rauthy client disappeared before the drift check")
	}
	drifted := *remote
	drifted.AccessTokenLifetime = 900
	s.f.putWithSecret(&drifted, s.secret())
	const repairWindow = 4 * pollInterval
	driftErr := wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, repairWindow, true,
		func(context.Context) (bool, error) {
			c := s.stored()
			return c != nil && c.AccessTokenLifetime == 600, nil
		})
	if driftErr != nil {
		t.Fatalf("polling did not repair Rauthy drift within %s: %v", repairWindow, driftErr)
	}

	// Within the runtime's creation grace period a client that is gone
	// counts as not yet visible, which would hold up the finalizer.
	ageCreateOf(t, cr)
	if err := envKube.Delete(ctx, got); err != nil {
		t.Fatalf("delete the Client: %v", err)
	}
	eventually(t, "the Client to be finalized", func() bool { return stored(t, cr) == nil })
	if s.stored() != nil {
		t.Error("the Rauthy client was not deleted with its resource")
	}
}

func countCalls(f *fakeRauthy, call string) int {
	n := 0
	for _, c := range f.callLog() {
		if c == call {
			n++
		}
	}
	return n
}

// ageCreateOf moves the runtime's create marks an hour into the past.
func ageCreateOf(t *testing.T, cr *v1alpha1.Client) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		got := stored(t, cr)
		old := time.Now().Add(-time.Hour)
		meta.SetExternalCreatePending(got, old)
		meta.SetExternalCreateSucceeded(got, old)
		return envKube.Update(context.Background(), got)
	})
	if err != nil {
		t.Fatalf("age the create marks: %v", err)
	}
}
