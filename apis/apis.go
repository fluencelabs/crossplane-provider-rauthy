// Package apis contains the Kubernetes API of the Rauthy provider.
package apis

import (
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// AddToScheme adds every API type of the provider to the scheme.
func AddToScheme(s *runtime.Scheme) error {
	return v1alpha1.SchemeBuilder.AddToScheme(s)
}
