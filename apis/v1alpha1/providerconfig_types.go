package v1alpha1

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderCredentials locates the Rauthy API key.
type ProviderCredentials struct {
	// Source of the credentials. Only a Kubernetes Secret is supported.
	// +kubebuilder:validation:Enum=Secret
	// +kubebuilder:default=Secret
	Source xpv2.CredentialsSource `json:"source"`

	// SecretRef selects the key of a Secret holding the Rauthy API key in the
	// form `name$secret`, exactly as Rauthy displays it on creation. Keep the
	// Secret in the provider's own namespace.
	SecretRef xpv2.SecretKeySelector `json:"secretRef"`
}

// ClusterProviderConfigSpec configures how the provider reaches Rauthy.
type ClusterProviderConfigSpec struct {
	// URL is the Rauthy base URL the provider talks to, for example
	// http://rauthy.rauthy.svc.cluster.local:8080. The API path /auth/v1 is
	// appended automatically.
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// IssuerURL is the OIDC issuer published to consumers in the connection
	// details of every Client. Defaults to URL followed by /auth/v1, which is
	// only right when consumers reach Rauthy at the same address as the
	// provider; set it to the public issuer otherwise.
	// +optional
	// +kubebuilder:validation:Pattern=`^https?://`
	IssuerURL *string `json:"issuerURL,omitempty"`

	// Credentials used to authenticate to the Rauthy admin API.
	Credentials ProviderCredentials `json:"credentials"`
}

// A ProviderConfigStatus reflects the observed state of a provider config.
type ProviderConfigStatus struct {
	xpv2.ProviderConfigStatus `json:",inline"`
}

// +kubebuilder:object:root=true

// A ClusterProviderConfig configures the Rauthy provider. It is cluster scoped
// on purpose: the API key lives in one Secret in the provider's namespace and
// application namespaces only reference the config by name. There is no
// namespaced ProviderConfig.
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.url"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="SECRET-NAME",type="string",JSONPath=".spec.credentials.secretRef.name",priority=1
// +kubebuilder:resource:scope=Cluster,categories={crossplane,provider,rauthy}
type ClusterProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterProviderConfigSpec `json:"spec"`
	Status ProviderConfigStatus      `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterProviderConfigList contains a list of ClusterProviderConfig.
type ClusterProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterProviderConfig `json:"items"`
}

// +kubebuilder:object:root=true

// A ProviderConfigUsage records that a Client uses a ClusterProviderConfig.
// Usages live in the namespace of the managed resource that created them.
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="CONFIG-NAME",type="string",JSONPath=".providerConfigRef.name"
// +kubebuilder:printcolumn:name="RESOURCE-KIND",type="string",JSONPath=".resourceRef.kind"
// +kubebuilder:printcolumn:name="RESOURCE-NAME",type="string",JSONPath=".resourceRef.name"
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,provider,rauthy}
type ProviderConfigUsage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	xpv2.TypedProviderConfigUsage `json:",inline"`
}

// +kubebuilder:object:root=true

// ProviderConfigUsageList contains a list of ProviderConfigUsage.
type ProviderConfigUsageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ProviderConfigUsage `json:"items"`
}
