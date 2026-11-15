// Package controller wires every controller of the provider into a manager.
package controller

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/client"
	"github.com/fluencelabs/crossplane-provider-rauthy/internal/controller/config"
)

// SetupGated adds all controllers to the manager. Managed resource
// controllers start only once their CRDs exist (safe-start).
func SetupGated(mgr ctrl.Manager, o controller.Options) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
		config.Setup,
		client.SetupGated,
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}
