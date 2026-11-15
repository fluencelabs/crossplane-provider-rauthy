// Package config reconciles ClusterProviderConfigs by accounting for their
// usage, so a config cannot be deleted while a Client still uses it.
package config

import (
	"fmt"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/providerconfig"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// Setup adds the ClusterProviderConfig controller.
//
// Every managed resource of this provider is namespaced, so every usage record
// is a namespaced ProviderConfigUsage living beside the resource that created
// it, even though the config it points at is cluster scoped.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := providerconfig.ControllerName(v1alpha1.ClusterProviderConfigGroupKind)

	of := resource.ProviderConfigKinds{
		Config:    v1alpha1.ClusterProviderConfigGroupVersionKind,
		Usage:     v1alpha1.ProviderConfigUsageGroupVersionKind,
		UsageList: v1alpha1.ProviderConfigUsageListGroupVersionKind,
	}

	r := providerconfig.NewReconciler(mgr, of,
		providerconfig.WithLogger(o.Logger.WithValues("controller", name)),
		//nolint:staticcheck // crossplane-runtime has not moved to the new events API yet.
		providerconfig.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))))

	err := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		For(&v1alpha1.ClusterProviderConfig{}).
		Watches(&v1alpha1.ProviderConfigUsage{},
			&resource.EnqueueRequestForProviderConfig{Kind: v1alpha1.ClusterProviderConfigKind}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
	if err != nil {
		return fmt.Errorf("cannot build the ClusterProviderConfig controller: %w", err)
	}
	return nil
}
