package client_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/go-logr/logr/funcr"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	kube "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
)

// These tests drive the real crossplane-runtime managed reconciler, with the
// provider's own reconciler options, against a fake API server and the fake
// Rauthy. They check what the external client tests cannot: that ownership
// recorded in status survives the runtime's own updates of the object.

// fakeManager provides the two things managed.NewReconciler takes from a
// manager.
type fakeManager struct {
	manager.Manager

	client kube.Client
	scheme *runtime.Scheme
}

func (m fakeManager) GetClient() kube.Client     { return m.client }
func (m fakeManager) GetScheme() *runtime.Scheme { return m.scheme }

type harness struct {
	// kube is the API server's view (what an uncached read returns).
	kube kube.Client
	// cache is what the runtime and the connector read through; it serves
	// the stale object while one is set.
	cache *staleCache
	r     *managed.Reconciler
	key   kube.ObjectKey
	// events and logs capture what the reconciler tells the world.
	events *record.FakeRecorder
	logs   *logSink
}

// logSink collects debug-level log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) add(prefix, args string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, prefix+" "+args)
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// told returns every event, condition message and log line so far.
func (h *harness) told(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for {
		select {
		case e := <-h.events.Events:
			b.WriteString(e + "\n")
			continue
		default:
		}
		break
	}
	if got := h.reconcile(t, 0); got != nil {
		for _, c := range got.Status.Conditions {
			b.WriteString(c.Message + "\n")
		}
	}
	b.WriteString(h.logs.all())
	return b.String()
}

// staleCache stands in for the manager's informer-backed client. While stale
// is set, it serves that copy of the Client instead of the current one, the
// way a lagging informer cache does; writes go to the API server.
type staleCache struct {
	kube.Client

	stale *v1alpha1.Client
}

func (c *staleCache) Get(ctx context.Context, key kube.ObjectKey, obj kube.Object, opts ...kube.GetOption) error {
	if cr, ok := obj.(*v1alpha1.Client); ok && c.stale != nil && key == kube.ObjectKeyFromObject(c.stale) {
		c.stale.DeepCopyInto(cr)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func newHarness(t *testing.T, f *fakeRauthy, cr *v1alpha1.Client, intercept ...interceptor.Funcs) *harness {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cr.Spec.ProviderConfigReference = &xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "default"}
	cr.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: "app-oidc"}
	// The usage tracker names its record after the resource's UID, which a
	// real API server assigns and the fake does not.
	cr.SetUID("0b5c6f7e-3d2a-4c1b-9e8f-7a6b5c4d3e2f")
	b := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&v1alpha1.Client{}, &v1alpha1.ClusterProviderConfig{}).
		WithObjects(providerConfig(f.srv.URL, new(testIssuer)), apiKeySecret(), cr)
	for _, i := range intercept {
		b = b.WithInterceptorFuncs(i)
	}
	k := b.Build()
	cache := &staleCache{Client: k}

	c := &client.Connector{
		Kube:   cache,
		Reader: k,
		Usage:  resource.NewProviderConfigUsageTracker(cache, &v1alpha1.ProviderConfigUsage{}),
	}
	events := record.NewFakeRecorder(1000)
	logs := &logSink{}
	opts := append(client.ReconcilerOptions(c),
		managed.WithManagementPolicies(),
		managed.WithRecorder(event.NewAPIRecorder(events)),
		managed.WithLogger(logging.NewLogrLogger(funcr.New(logs.add, funcr.Options{Verbosity: 1}))),
	)
	r := managed.NewReconciler(fakeManager{client: cache, scheme: s},
		resource.ManagedKind(v1alpha1.ClientGroupVersionKind), opts...)
	return &harness{kube: k, cache: cache, r: r, key: kube.ObjectKeyFromObject(cr), events: events, logs: logs}
}

// reconcile runs n reconciles and returns the Client as stored afterwards, or
// nil once it is gone.
func (h *harness) reconcile(t *testing.T, n int) *v1alpha1.Client {
	t.Helper()
	for range n {
		// Errors surface in the conditions, which the callers check.
		_, _ = h.r.Reconcile(context.Background(), reconcile.Request{NamespacedName: h.key})
	}
	got := &v1alpha1.Client{}
	if err := h.kube.Get(context.Background(), h.key, got); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		t.Fatalf("get the Client: %v", err)
	}
	return got
}

// ageCreate moves the runtime's create marks an hour into the past, so the
// reconciler's creation grace period no longer delays a re-create.
func (h *harness) ageCreate(t *testing.T) {
	t.Helper()
	got := &v1alpha1.Client{}
	if err := h.kube.Get(context.Background(), h.key, got); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	meta.SetExternalCreatePending(got, old)
	meta.SetExternalCreateSucceeded(got, old)
	if err := h.kube.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) connectionSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	err := h.kube.Get(context.Background(), kube.ObjectKey{Namespace: h.key.Namespace, Name: "app-oidc"}, s)
	if kerrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get the connection Secret: %v", err)
	}
	return s
}

func conditionStatus(cr *v1alpha1.Client, ct xpv2.ConditionType) string {
	return string(cr.GetCondition(ct).Status)
}

func TestReconcilerCreateUpdateReady(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))

	got := h.reconcile(t, 5)

	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("Ready = %q, conditions %+v; want True", conditionStatus(got, xpv2.TypeReady), got.Status.Conditions)
	}
	if got.Status.Ownership == nil || got.Status.Ownership.ClientID != testID {
		t.Errorf("status.ownership = %+v, want client %q", got.Status.Ownership, testID)
	}
	if c := f.stored(); c == nil || c.AccessTokenLifetime != 600 {
		t.Errorf("Rauthy client = %+v, want accessTokenLifetime 600", c)
	}
	s := h.connectionSecret(t)
	if s == nil || string(s.Data[client.ConnectionKeyClientSecret]) != f.secretOf() ||
		string(s.Data[client.ConnectionKeyClientID]) != testID {
		t.Errorf("connection Secret = %+v, want the client id and secret", s)
	}
}

func TestReconcilerFailedPutConverges(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	f.failPut = true
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))

	got := h.reconcile(t, 5)
	if conditionStatus(got, xpv2.TypeReady) == "True" || conditionStatus(got, xpv2.TypeSynced) != "False" {
		t.Fatalf("with the PUT failing: conditions %+v; want not Ready and not Synced", got.Status.Conditions)
	}
	if got.Status.Ownership == nil || f.stored() == nil {
		t.Fatalf("after the POST the resource must own the client: ownership %+v", got.Status.Ownership)
	}

	f.failPut = false
	got = h.reconcile(t, 3)
	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Errorf("after the PUT recovered: conditions %+v; want Ready", got.Status.Conditions)
	}
	if c := f.stored(); c.AccessTokenLifetime != 600 {
		t.Errorf("accessTokenLifetime in Rauthy = %d, want 600", c.AccessTokenLifetime)
	}
}

func TestReconcilerRejectsForgedOwnership(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	foreign := existing(true)
	f.put(foreign)
	// A fresh manifest carrying every annotation a successful create leaves,
	// and asking for different redirect URIs than the foreign client has.
	h := newHarness(t, f, newClient(forged, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.RedirectURIs = []string{"https://attacker.example.com/cb"}
	}))

	got := h.reconcile(t, 5)

	if conditionStatus(got, xpv2.TypeReady) == "True" || conditionStatus(got, xpv2.TypeSynced) != "False" {
		t.Errorf("conditions %+v; want not Ready and not Synced", got.Status.Conditions)
	}
	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "adoption is not supported") {
		t.Errorf("Synced message = %q, want an adoption error", msg)
	}
	if got.Status.Ownership != nil {
		t.Errorf("status.ownership = %+v, want none", got.Status.Ownership)
	}
	if s := h.connectionSecret(t); s != nil && len(s.Data[client.ConnectionKeyClientSecret]) > 0 {
		t.Error("the foreign client's secret was published")
	}

	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatalf("delete the Client: %v", err)
	}
	if after := h.reconcile(t, 3); after != nil {
		t.Errorf("the Client was not finalized: %+v", after.Status.Conditions)
	}

	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("the foreign client changed: -want, +got:\n%s", diff)
	}
	for _, c := range f.callLog() {
		if c != "GET /clients/app" && c != "POST /clients" {
			t.Errorf("unexpected call to Rauthy: %s", c)
		}
	}
}

// TestReconcilerStaleCacheDoesNotAdopt is the lagging-cache race: the owned
// client vanishes, the re-create clears the record, a third party takes the
// id first, and the next reconcile still reads the pre-clear object from the
// cache. Ownership must come from the API server, so the foreign client's
// secret is never read and nothing is written to it.
func TestReconcilerStaleCacheDoesNotAdopt(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient())
	if got := h.reconcile(t, 5); conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("setup: Ready = %q, want True", conditionStatus(got, xpv2.TypeReady))
	}
	h.ageCreate(t)
	preClear := h.reconcile(t, 0)

	f.deleteStored()
	foreign := existing(true)
	f.raceOnPost = foreign
	got := h.reconcile(t, 1)
	if got.Status.Ownership != nil {
		t.Fatalf("after the refused re-create: status.ownership = %+v, want none", got.Status.Ownership)
	}

	h.cache.stale = preClear
	f.resetCalls()
	got = h.reconcile(t, 3)
	h.cache.stale = nil

	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "adoption is not supported") {
		t.Errorf("Synced message = %q, want a refusal", msg)
	}
	for _, c := range f.callLog() {
		if c != "GET /clients/app" {
			t.Errorf("acted on the foreign client through a stale cache: %s", c)
		}
	}
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("the foreign client changed: -want, +got:\n%s", diff)
	}
}

// TestReconcilerUnrecordedCreateLeavesNoOwnership: when the ownership record
// cannot be written, the new client is deleted again, and the runtime's own
// status update must not persist a record for it.
func TestReconcilerUnrecordedCreateLeavesNoOwnership(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(), interceptor.Funcs{
		SubResourcePatch: func(context.Context, kube.Client, string, kube.Object, kube.Patch,
			...kube.SubResourcePatchOption,
		) error {
			return errors.New("status write refused")
		},
	})

	got := h.reconcile(t, 3)

	if got.Status.Ownership != nil {
		t.Errorf("status.ownership = %+v, want none", got.Status.Ownership)
	}
	if f.stored() != nil {
		t.Error("the unrecorded client was left in Rauthy")
	}
	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "was deleted again") {
		t.Errorf("Synced message = %q, want the rollback explained", msg)
	}
}

// TestReconcilerCRDWithoutOwnership: an older CRD prunes status.ownership but
// the patch still succeeds. The create must be rolled back and the cause
// named, instead of leaving a client the resource then refuses forever.
func TestReconcilerCRDWithoutOwnership(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, _ kube.Patch,
			opts ...kube.SubResourcePatchOption,
		) error {
			// Apply nothing and answer with the stored object, as a server
			// pruning the unknown field does.
			return c.SubResource(sub).Patch(ctx, obj, kube.RawPatch(types.MergePatchType, []byte("{}")), opts...)
		},
	})

	got := h.reconcile(t, 2)

	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "does not keep status.ownership") {
		t.Errorf("Synced message = %q, want the CRD named", msg)
	}
	if f.stored() != nil {
		t.Error("the unrecorded client was left in Rauthy")
	}
}

// TestReconcilerErrorWordingIsStable: once the create call itself is over,
// every refusal uses the one neutral message, whatever the create attempt
// ended in.
func TestReconcilerErrorWordingIsStable(t *testing.T) {
	t.Parallel()

	for name, setup := range map[string]func(*fakeRauthy){
		"Ambiguous": func(f *fakeRauthy) { f.commitThenFail = true },
		"Refused":   func(f *fakeRauthy) { f.raceOnPost = existing(true) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			setup(f)
			got := newHarness(t, f, newClient()).reconcile(t, 4)
			if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "holds no ownership record") {
				t.Errorf("Synced message = %q, want the neutral refusal", msg)
			}
			if got.Status.Ownership != nil {
				t.Errorf("status.ownership = %+v, want none", got.Status.Ownership)
			}
		})
	}
}

// TestReconcilerRecordLandedDespiteError: the status patch is applied but
// answers with an error. The record landed, so the client is kept and
// managed; deleting it would leave a marker with no client behind it.
func TestReconcilerRecordLandedDespiteError(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	var once atomic.Bool
	h := newHarness(t, f, newClient(), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, p kube.Patch,
			opts ...kube.SubResourcePatchOption,
		) error {
			if err := c.SubResource(sub).Patch(ctx, obj, p, opts...); err != nil {
				return err
			}
			if once.CompareAndSwap(false, true) {
				return errors.New("timeout after the write")
			}
			return nil
		},
	})

	got := h.reconcile(t, 5)

	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Errorf("conditions %+v; want Ready", got.Status.Conditions)
	}
	if got.Status.Ownership == nil || f.stored() == nil {
		t.Fatalf("ownership %+v, client %+v; want both", got.Status.Ownership, f.stored())
	}
	for _, c := range f.callLog() {
		if c == "DELETE /clients/app" {
			t.Error("the client was rolled back although its record landed")
		}
	}
}

// TestReconcilerCRDWithoutOwnershipStopsCreating: once the CRD is found to
// prune the record, the provider stops creating instead of churning through
// POST and DELETE on every reconcile.
func TestReconcilerCRDWithoutOwnershipStopsCreating(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c kube.Client, sub string, obj kube.Object, _ kube.Patch,
			opts ...kube.SubResourcePatchOption,
		) error {
			return c.SubResource(sub).Patch(ctx, obj, kube.RawPatch(types.MergePatchType, []byte("{}")), opts...)
		},
	})

	got := h.reconcile(t, 5)

	posts := 0
	for _, c := range f.callLog() {
		if c == "POST /clients" {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("%d creates, want 1", posts)
	}
	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "does not keep status.ownership") {
		t.Errorf("Synced message = %q, want the CRD named", msg)
	}
}

// recreate makes the reported window real: the owned client is deleted in
// Rauthy out of band and a third party creates one with the same id and its
// own secret before the provider sees the gap. The record still names the id.
func recreate(f *fakeRauthy) *rauthy.ClientResponse {
	f.deleteStored()
	foreign := existing(true)
	f.putWithSecret(foreign, "third-party-secret")
	return foreign
}

func TestReconcilerRecreatedClientIsNotAdopted(t *testing.T) {
	t.Parallel()

	for name, stale := range map[string]bool{"FreshCache": false, "StaleCache": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRauthy(t)
			h := newHarness(t, f, newClient())
			if got := h.reconcile(t, 5); conditionStatus(got, xpv2.TypeReady) != "True" {
				t.Fatalf("setup: Ready = %q, want True", conditionStatus(got, xpv2.TypeReady))
			}
			ours := f.secretOf()
			before := h.reconcile(t, 0)

			foreign := recreate(f)
			if stale {
				h.cache.stale = before
			}
			f.resetCalls()
			h.reconcile(t, 3)
			h.cache.stale = nil
			got := h.reconcile(t, 1)

			checkRecreatedRefused(t, h, f, got, ours, before.Status.Ownership.SecretFingerprint)
			checkFinalizesUntouched(t, h, f, got, foreign)
		})
	}
}

func checkRecreatedRefused(t *testing.T, h *harness, f *fakeRauthy, got *v1alpha1.Client, ours, fingerprint string) {
	t.Helper()
	if msg := got.GetCondition(
		xpv2.TypeSynced,
	).Message; !strings.Contains(
		msg,
		"is not the one this resource created",
	) {
		t.Errorf("Synced message = %q, want the mismatch explained", msg)
	}
	if s := h.connectionSecret(t); s == nil || string(s.Data[client.ConnectionKeyClientSecret]) != ours {
		t.Error("the connection Secret no longer holds this resource's own secret")
	}
	for _, c := range f.callLog() {
		if c != "GET /clients/app" && c != "POST /clients/app/secret" {
			t.Errorf("acted on the recreated client: %s", c)
		}
	}
	told := h.told(t)
	for _, leak := range []string{fingerprint, ours, "third-party-secret", fingerprintOf(testID, "third-party-secret")} {
		if strings.Contains(told, leak) {
			t.Errorf("events, conditions or logs carry a secret or a fingerprint: %q", leak)
		}
	}
}

// checkFinalizesUntouched deletes the resource and checks that it lets go of
// the foreign client without touching it.
func checkFinalizesUntouched(
	t *testing.T,
	h *harness,
	f *fakeRauthy,
	got *v1alpha1.Client,
	foreign *rauthy.ClientResponse,
) {
	t.Helper()
	// The runtime waits out its creation grace period before it trusts that
	// nothing of ours exists, so age the create first.
	h.ageCreate(t)
	if err := h.kube.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if after := h.reconcile(t, 3); after != nil {
		t.Errorf("the Client was not finalized: %+v", after.Status.Conditions)
	}
	if diff := cmp.Diff(foreign, f.stored()); diff != "" {
		t.Errorf("the foreign client changed: -want, +got:\n%s", diff)
	}
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "DELETE") || strings.HasPrefix(c, "PUT") {
			t.Errorf("acted on the recreated client: %s", c)
		}
	}
}

func TestReconcilerRotatedSecretIsNotAdopted(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.AccessTokenLifetime = new(int64(600))
	}))
	if got := h.reconcile(t, 5); conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("setup: Ready = %q, want True", conditionStatus(got, xpv2.TypeReady))
	}

	f.rotate()
	f.resetCalls()
	got := h.reconcile(t, 2)

	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "its secret was rotated outside") {
		t.Errorf("Synced message = %q, want the mismatch explained", msg)
	}
	if s := h.connectionSecret(t); s != nil && string(s.Data[client.ConnectionKeyClientSecret]) == "rotated-secret" {
		t.Error("the rotated secret was published")
	}
	for _, c := range f.callLog() {
		if c != "GET /clients/app" && c != "POST /clients/app/secret" {
			t.Errorf("acted on the rotated client: %s", c)
		}
	}
}

func TestReconcilerPublicClient(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient(public, func(cr *v1alpha1.Client) {
		cr.Spec.ForProvider.Challenges = []string{"S256"}
	}))

	got := h.reconcile(t, 5)

	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("conditions %+v; want Ready", got.Status.Conditions)
	}
	if got.Status.Ownership == nil || got.Status.Ownership.SecretFingerprint != "" {
		t.Errorf("record = %+v, want the id and no fingerprint", got.Status.Ownership)
	}
	s := h.connectionSecret(t)
	if s == nil || len(s.Data[client.ConnectionKeyClientSecret]) != 0 ||
		string(s.Data[client.ConnectionKeyClientID]) != testID {
		t.Errorf("connection Secret = %+v, want the id without a secret", s)
	}
	for _, c := range f.callLog() {
		if strings.HasSuffix(c, "/secret") {
			t.Errorf("a public client's secret was asked for: %s", c)
		}
	}
}

func TestReconcilerConfidentialFlipRefused(t *testing.T) {
	t.Parallel()
	f := newFakeRauthy(t)
	h := newHarness(t, f, newClient())
	got := h.reconcile(t, 5)
	if conditionStatus(got, xpv2.TypeReady) != "True" {
		t.Fatalf("setup: Ready = %q, want True", conditionStatus(got, xpv2.TypeReady))
	}

	// The CRD refuses this change; the fake API server does not enforce CEL,
	// which is the case the controller check covers.
	got.Spec.ForProvider.Confidential = false
	if err := h.kube.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	f.resetCalls()
	got = h.reconcile(t, 2)

	if msg := got.GetCondition(xpv2.TypeSynced).Message; !strings.Contains(msg, "confidential cannot change") {
		t.Errorf("Synced message = %q, want the change refused", msg)
	}
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "PUT") {
			t.Errorf("the change reached Rauthy: %s", c)
		}
	}
	if c := f.stored(); c == nil || !c.Confidential {
		t.Errorf("Rauthy client = %+v, want it still confidential", c)
	}
}
