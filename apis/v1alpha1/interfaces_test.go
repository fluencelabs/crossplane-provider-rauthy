package v1alpha1_test

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// The types must satisfy the crossplane-runtime contracts. The checks live in a
// test file so that code generation can load the package before the generated
// method sets exist.
var (
	_ resource.ModernManaged            = &v1alpha1.Client{}
	_ resource.ManagedList              = &v1alpha1.ClientList{}
	_ resource.ProviderConfig           = &v1alpha1.ClusterProviderConfig{}
	_ resource.TypedProviderConfigUsage = &v1alpha1.ProviderConfigUsage{}
	_ resource.ProviderConfigUsageList  = &v1alpha1.ProviderConfigUsageList{}
)
