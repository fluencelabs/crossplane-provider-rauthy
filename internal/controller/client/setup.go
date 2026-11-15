// Package client reconciles the Client managed resource: an OIDC client in
// Rauthy.
package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
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

	opts := append(ReconcilerOptions(&Connector{
		Kube:   mgr.GetClient(),
		Reader: mgr.GetAPIReader(),
		Usage:  resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
	}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
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
	// confirmed through it. Kube is used when it is nil, which is only right
	// for a client that has no cache.
	Reader kube.Reader
	Usage  UsageTracker
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

	sel := pc.Spec.Credentials.SecretRef
	key, err := resource.CommonCredentialExtractor(ctx, pc.Spec.Credentials.Source, c.Kube,
		xpv2.CommonCredentialSelectors{SecretRef: &sel})
	if err != nil {
		return nil, fmt.Errorf("cannot read the Rauthy API key: %w", err)
	}

	api, err := rauthy.New(pc.Spec.URL, strings.TrimSpace(string(key)))
	if err != nil {
		return nil, fmt.Errorf("cannot build the Rauthy client: %w", err)
	}
	reader := c.Reader
	if reader == nil {
		reader = c.Kube
	}
	return NewExternal(api, c.Kube.Status(), reader, issuerFor(pc.Spec.URL, pc.Spec.IssuerURL)), nil
}
