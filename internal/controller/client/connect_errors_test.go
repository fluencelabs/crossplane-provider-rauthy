package client_test

import (
	"context"
	"strings"
	"testing"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// reconcileOnce runs one reconcile and checks that a failure is handed back
// to the controller's rate limiter (a backed-off requeue) rather than
// scheduled again at a fixed interval.
func reconcileOnce(t *testing.T, h *harness) *v1alpha1.Client {
	t.Helper()
	res, _ := h.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: h.key})
	//nolint:staticcheck // The runtime still asks for a rate-limited requeue through Requeue.
	if !res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("result = %+v, want a rate-limited requeue", res)
	}
	return h.reconcile(t, 0)
}

func checkSyncedMessage(t *testing.T, got *v1alpha1.Client, want string) {
	t.Helper()
	c := got.GetCondition(xpv2.TypeSynced)
	if c.Status != "False" || c.Message != want {
		t.Errorf("Synced = %q, message %q; want False, %q", c.Status, c.Message, want)
	}
}

// A ClusterProviderConfig URL with a query cannot build a Rauthy client: the
// resource says so and Rauthy is never called.
func TestReconcilerURLWithQuery(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient())
	pc := &v1alpha1.ClusterProviderConfig{}
	if err := h.kube.Get(context.Background(), kube.ObjectKey{Name: "default"}, pc); err != nil {
		t.Fatal(err)
	}
	pc.Spec.URL = f.srv.URL + "/?tenant=a"
	if err := h.kube.Update(context.Background(), pc); err != nil {
		t.Fatal(err)
	}

	got := reconcileOnce(t, h)

	checkSyncedMessage(
		t,
		got,
		`connect failed: cannot build the Rauthy client: rauthy url must not carry a query or fragment`,
	)
	if calls := f.callLog(); len(calls) != 0 {
		t.Errorf("Rauthy was called: %v", calls)
	}
}

// A Rauthy URL that answers 3xx is not followed: the resource names the host it
// pointed to, and the request is not retried.
func TestReconcilerRedirectRefused(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.redirectTo = "https://login.example.com"
	h := newHarness(t, f, newClient())

	got := reconcileOnce(t, h)

	checkSyncedMessage(t, got, `observe failed: cannot get Rauthy client "app": GET /clients/app: HTTP 308: `+
		`redirect refused: the server pointed to https://login.example.com`)
	if calls := f.callLog(); len(calls) != 1 {
		t.Errorf("Rauthy calls = %v, want the one GET", calls)
	}
	if events := h.told(t); !strings.Contains(events, "CannotObserveExternalResource") {
		t.Errorf("no CannotObserveExternalResource event in:\n%s", events)
	}
}

func TestReconcilerURLSecrets(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"query", "fragment", "transport"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			h := newHarness(t, f, newClient())
			pc := &v1alpha1.ClusterProviderConfig{}
			if err := h.kube.Get(context.Background(), kube.ObjectKey{Name: "default"}, pc); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "query":
				pc.Spec.URL = f.srv.URL + "/?token=query-secret"
			case "fragment":
				pc.Spec.URL = f.srv.URL + "/#fragment-secret"
			case "transport":
				pc.Spec.URL = strings.Replace(
					f.srv.URL,
					"://",
					"://private-user:private-password@",
					1,
				) + "/private-path"
				f.srv.Close()
			}
			if err := h.kube.Update(context.Background(), pc); err != nil {
				t.Fatal(err)
			}
			stage := "connect failed:"
			if scenario == "transport" {
				stage = "observe failed:"
			}
			checkURLSecretFailure(t, h, stage)
			if calls := f.callLog(); len(calls) != 0 {
				t.Errorf("Rauthy was called: %v", calls)
			}
		})
	}
}

func checkURLSecretFailure(t *testing.T, h *harness, stage string) {
	t.Helper()
	got := reconcileOnce(t, h)
	condition := got.GetCondition(xpv2.TypeSynced)
	if condition.Status != "False" || !strings.HasPrefix(condition.Message, stage) {
		t.Fatalf("Synced = %+v, want False with %q", condition, stage)
	}
	told := h.told(t) + h.provider.all()
	for _, secret := range []string{"query-secret", "fragment-secret", "private-user", "private-password", "private-path"} {
		if strings.Contains(told, secret) {
			t.Errorf("reconciler exposed %q in conditions, events or logs", secret)
		}
	}
}
