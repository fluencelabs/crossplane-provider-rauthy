// Package client reconciles the Client managed resource: an OIDC client in
// Rauthy.
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

var errNoProviderConfig = errors.New("no providerConfigRef set")

// SetupGated adds the Client controller once its CRD is present.
func SetupGated(mgr ctrl.Manager, o controller.Options) error {
	o.Gate.Register(func() {
		if err := Setup(mgr, o); err != nil {
			panic(fmt.Errorf("cannot set up the Client controller: %w", err))
		}
	}, v1alpha1.ClientGroupVersionKind)
	return nil
}

// Setup adds a controller that reconciles Client managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ClientGroupKind)

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Client{}, ClientIDIndex,
		IndexClientID); err != nil {
		return fmt.Errorf("cannot index Clients by recorded client id: %w", err)
	}

	log := o.Logger.WithValues("controller", name)
	opts := append(ReconcilerOptions(&Connector{
		Kube:   mgr.GetClient(),
		Reader: mgr.GetAPIReader(),
		Usage:  resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
		Logger: log,
	}),
		managed.WithLogger(log),
		managed.WithPollInterval(o.PollInterval),
		//nolint:staticcheck // crossplane-runtime has not moved to the new events API yet.
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
	)
	if o.Features.Enabled(feature.EnableBetaManagementPolicies) {
		opts = append(opts, managed.WithManagementPolicies())
	}
	if o.Features.Enabled(feature.EnableAlphaChangeLogs) {
		opts = append(opts, managed.WithChangeLogger(o.ChangeLogOptions.ChangeLogger))
	}
	if o.MetricOptions != nil {
		opts = append(opts, managed.WithMetricRecorder(o.MetricOptions.MRMetrics))
		if o.MetricOptions.MRStateMetrics != nil {
			rec := statemetrics.NewMRStateRecorder(
				mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, &v1alpha1.ClientList{},
				o.MetricOptions.PollStateMetricInterval,
			)
			if err := mgr.Add(rec); err != nil {
				return fmt.Errorf("cannot register the Client state metrics recorder: %w", err)
			}
		}
	}

	r := managed.NewReconciler(mgr, resource.ManagedKind(v1alpha1.ClientGroupVersionKind), opts...)

	if err := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Client{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter)); err != nil {
		return fmt.Errorf("cannot build the Client controller: %w", err)
	}
	return nil
}

// ReconcilerOptions are the options every Client reconciler is built with;
// Setup adds logging, events, metrics and feature flags on top.
func ReconcilerOptions(c *Connector) []managed.ReconcilerOption {
	return []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[*v1alpha1.Client](c),
		// The client id is the external name, known before the create. An
		// interrupted create is therefore safe to retry: status.ownership,
		// not the runtime's create annotations, decides what is ours.
		managed.WithDeterministicExternalName(true),
	}
}

// UsageTracker records that a managed resource uses its provider config.
type UsageTracker interface {
	Track(ctx context.Context, mg resource.ModernManaged) error
}

// Connector builds an external client from a Client's ClusterProviderConfig.
type Connector struct {
	// Kube is the manager's (cached) client.
	Kube kube.Client
	// Reader reads from the API server without a cache; ownership is
	// confirmed and the API key read through it. Kube is used when it is
	// nil, which is only right for a client that has no cache. When Reader
	// is set, Kube must index Clients by ClientIDIndex.
	Reader kube.Reader
	Usage  UsageTracker
	// Logger receives what only the provider's operator may see, such as
	// which Client holds a record that blocks another one. Nil discards it.
	Logger logging.Logger

	// crd is shared by every external client of this connector: once the
	// installed CRD is found to prune status.ownership, no further client is
	// created until the provider restarts with current CRDs.
	crd atomic.Bool
}

// Connect resolves the ClusterProviderConfig, reads the API key and returns a
// client for that Rauthy instance.
func (c *Connector) Connect(
	ctx context.Context,
	cr *v1alpha1.Client,
) (managed.TypedExternalClient[*v1alpha1.Client], error) {
	ref := cr.GetProviderConfigReference()
	if ref == nil {
		return nil, errNoProviderConfig
	}
	if ref.Kind != v1alpha1.ClusterProviderConfigKind {
		return nil, fmt.Errorf(
			"unsupported provider config kind %q: only %s is supported",
			ref.Kind, v1alpha1.ClusterProviderConfigKind,
		)
	}

	if err := c.Usage.Track(ctx, cr); err != nil {
		return nil, fmt.Errorf("cannot track ClusterProviderConfig usage: %w", err)
	}

	pc := &v1alpha1.ClusterProviderConfig{}
	if err := c.Kube.Get(ctx, types.NamespacedName{Name: ref.Name}, pc); err != nil {
		return nil, fmt.Errorf("cannot get ClusterProviderConfig %q: %w", ref.Name, err)
	}

	reader, cache := c.Reader, kube.Reader(c.Kube)
	if reader == nil {
		reader, cache = c.Kube, nil
	}
	key, err := readAPIKey(ctx, reader, pc.Spec.Credentials)
	if err != nil {
		return nil, fmt.Errorf("cannot read the Rauthy API key: %w", err)
	}

	api, err := rauthy.New(pc.Spec.URL, strings.TrimSpace(string(key)),
		// Reconcile retries must confirm ownership before every write.
		rauthy.WithRetryPolicy(1, 0, 0))
	if err != nil {
		return nil, fmt.Errorf("cannot build the Rauthy client: %w", err)
	}
	backend, err := backendIdentity(api.BaseURL().Reveal())
	if err != nil {
		return nil, err
	}
	e := newExternal(
		api,
		c.Kube.Status(),
		reader,
		cache,
		issuerFor(pc.Spec.URL, pc.Spec.IssuerURL),
		backend,
		rauthy.NewSecret(strings.TrimSpace(string(key))),
		&c.crd,
	)
	if c.Logger != nil {
		e.log = c.Logger
	}
	return e, nil
}

// readAPIKey reads the API key from its Secret through r, which must not be
// the cached client: a cached read would start an informer over every Secret
// in the cluster.
func readAPIKey(ctx context.Context, r kube.Reader, creds v1alpha1.ProviderCredentials) ([]byte, error) {
	if creds.Source != xpv2.CredentialsSourceSecret {
		return nil, fmt.Errorf("unsupported credentials source %q: only Secret is supported", creds.Source)
	}
	sel := creds.SecretRef
	s := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sel.Namespace, Name: sel.Name}, s); err != nil {
		return nil, fmt.Errorf("cannot get the credentials Secret: %w", err)
	}
	return s.Data[sel.Key], nil
}
