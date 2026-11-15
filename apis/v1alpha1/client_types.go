package v1alpha1

import (
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The validation markers below transcribe the constraints Rauthy enforces on
// clients. They must stay equal to the accessors in rauthy-go's validation.go;
// a unit test compares the generated CRD with them.

// ClientParameters are the configurable fields of a Rauthy OIDC client.
//
// Optional fields follow one rule: leaving a field unset keeps whatever Rauthy
// currently holds for it (the defaults Rauthy picked at creation, or a value a
// previous spec set). Removing a field from the spec therefore does not revert
// it; set the value explicitly to change it. An explicit empty list ([]) or
// empty string ("") means "none" and clears the field in Rauthy, except for
// flowsEnabled, which Rauthy requires to be non-empty. The optional lists
// carry no omitempty, so that an empty list survives being written back to
// the API server instead of turning into "unset".
type ClientParameters struct {
	// Name is the human-readable name shown on the login page.
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z0-9À-ɏ\-\s\x{3041}-\x{3096}\x{30A0}-\x{30FF}\x{3400}-\x{4DB5}\x{4E00}-\x{9FCB}\x{F900}-\x{FA6A}\x{2E80}-\x{2FD5}\x{FF66}-\x{FF9F}\x{FFA1}-\x{FFDC}\x{31F0}-\x{31FF}]{2,128}$`
	Name *string `json:"name,omitempty"`

	// Confidential clients authenticate with a secret that Rauthy generates.
	// A public client has no secret and must use S256 PKCE. Immutable: the
	// ownership record is bound to the secret, so a client cannot switch
	// between confidential and public after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="confidential is immutable"
	Confidential bool `json:"confidential"`

	// Enabled controls whether the client may be used to log in.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// RedirectURIs are the allowed redirect URIs. Order does not matter.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9,.:/_\-&?=~#!$'()*+%@]+$`
	// +listType=set
	RedirectURIs []string `json:"redirectUris"`

	// PostLogoutRedirectURIs are the allowed redirect URIs after a logout.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9,.:/_\-&?=~#!$'()*+%@]+$`
	// +listType=set
	PostLogoutRedirectURIs []string `json:"postLogoutRedirectUris"`

	// AllowedOrigins are additional CORS origins, as scheme://host[:port]
	// without a path.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^(http|https)://[a-z0-9.:-]+$`
	// +listType=set
	AllowedOrigins []string `json:"allowedOrigins"`

	// FlowsEnabled are the enabled OAuth 2.0 grant types.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=authorization_code;client_credentials;"urn:ietf:params:oauth:grant-type:device_code";password;refresh_token
	// +listType=set
	FlowsEnabled []string `json:"flowsEnabled,omitempty"`

	// AccessTokenAlg is the signing algorithm of access tokens.
	// +optional
	// +kubebuilder:validation:Enum=RS256;RS384;RS512;EdDSA
	AccessTokenAlg *string `json:"accessTokenAlg,omitempty"`

	// IDTokenAlg is the signing algorithm of id tokens.
	// +optional
	// +kubebuilder:validation:Enum=RS256;RS384;RS512;EdDSA
	IDTokenAlg *string `json:"idTokenAlg,omitempty"`

	// AuthCodeLifetime is the lifetime of an authorization code in seconds.
	// +optional
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=300
	AuthCodeLifetime *int64 `json:"authCodeLifetime,omitempty"`

	// AccessTokenLifetime is the lifetime of an access token in seconds.
	// +optional
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=86400
	AccessTokenLifetime *int64 `json:"accessTokenLifetime,omitempty"`

	// Scopes the client may request.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9-_/,:*.]{2,64}$`
	// +listType=set
	Scopes []string `json:"scopes"`

	// DefaultScopes are granted even when the client does not ask for them.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9-_/,:*.]{2,64}$`
	// +listType=set
	DefaultScopes []string `json:"defaultScopes"`

	// Challenges are the accepted PKCE challenge methods. Rauthy requires S256
	// from public clients regardless of this setting.
	// +optional
	// +kubebuilder:validation:items:Enum=plain;S256
	// +listType=set
	Challenges []string `json:"challenges"`

	// ForceMFA requires MFA from users logging in through this client.
	// +optional
	ForceMFA *bool `json:"forceMfa,omitempty"`

	// ClientURI is an informational URI of the application.
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z0-9,.:/_\-&?=~#!$'()*+%@]+$`
	ClientURI *string `json:"clientUri,omitempty"`

	// Contacts of the application's maintainers.
	// +optional
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9+.@/-]{0,48}$`
	// +listType=set
	Contacts []string `json:"contacts"`

	// BackchannelLogoutURI is the OIDC back-channel logout endpoint of the
	// application.
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z0-9,.:/_\-&?=~#!$'()*+%@]+$`
	BackchannelLogoutURI *string `json:"backchannelLogoutUri,omitempty"`

	// RestrictGroupPrefix restricts logins to users in groups starting with
	// this prefix.
	// +optional
	// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z0-9-_/,:*\s]{2,64}$`
	RestrictGroupPrefix *string `json:"restrictGroupPrefix,omitempty"`
}

// ClientObservation is the client as Rauthy reports it.
type ClientObservation struct {
	ID                     string   `json:"id,omitempty"`
	Name                   *string  `json:"name,omitempty"`
	Confidential           bool     `json:"confidential,omitempty"`
	Enabled                bool     `json:"enabled,omitempty"`
	RedirectURIs           []string `json:"redirectUris,omitempty"`
	PostLogoutRedirectURIs []string `json:"postLogoutRedirectUris,omitempty"`
	AllowedOrigins         []string `json:"allowedOrigins,omitempty"`
	FlowsEnabled           []string `json:"flowsEnabled,omitempty"`
	AccessTokenAlg         string   `json:"accessTokenAlg,omitempty"`
	IDTokenAlg             string   `json:"idTokenAlg,omitempty"`
	AuthCodeLifetime       int64    `json:"authCodeLifetime,omitempty"`
	AccessTokenLifetime    int64    `json:"accessTokenLifetime,omitempty"`
	Scopes                 []string `json:"scopes,omitempty"`
	DefaultScopes          []string `json:"defaultScopes,omitempty"`
	Challenges             []string `json:"challenges,omitempty"`
	ForceMFA               bool     `json:"forceMfa,omitempty"`
	ClientURI              *string  `json:"clientUri,omitempty"`
	Contacts               []string `json:"contacts,omitempty"`
	BackchannelLogoutURI   *string  `json:"backchannelLogoutUri,omitempty"`
	RestrictGroupPrefix    *string  `json:"restrictGroupPrefix,omitempty"`
}

// A ClientSpec defines the desired state of a Client.
// +kubebuilder:validation:XValidation:rule="!has(self.providerConfigRef) || self.providerConfigRef.kind == 'ClusterProviderConfig'",message="providerConfigRef.kind must be ClusterProviderConfig: this provider has no namespaced ProviderConfig"
type ClientSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`

	ForProvider ClientParameters `json:"forProvider"`
}

// ClientOwnership records the Rauthy client a Client created. The provider
// writes it through the status subresource right after Rauthy accepted the
// create, and treats it as the only evidence that the client is its own.
type ClientOwnership struct {
	// ClientID is the id of the Rauthy client this resource created.
	ClientID string `json:"clientID"`

	// CreatedAt is when the provider created the client.
	CreatedAt metav1.Time `json:"createdAt"`

	// SecretFingerprint binds the record to the client's secret: the hex
	// SHA-256 of "rauthy-client-secret-v1", the client id and the secret
	// Rauthy issued, separated by NUL bytes. Empty for a public client, which
	// has no secret. The secret itself is never stored.
	// +optional
	SecretFingerprint string `json:"secretFingerprint,omitempty"`
}

// A ClientStatus represents the observed state of a Client.
type ClientStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`

	AtProvider ClientObservation `json:"atProvider,omitempty"`

	// Ownership is set once this resource has created its Rauthy client.
	// +optional
	Ownership *ClientOwnership `json:"ownership,omitempty"`
}

// +kubebuilder:object:root=true

// A Client is an OIDC client in Rauthy. The client id is the external name
// (the crossplane.io/external-name annotation), which defaults to
// metadata.name and cannot change once the client exists.
//
// A Client only ever manages a Rauthy client it created itself, as recorded in
// status.ownership: if a client with the same id already exists in Rauthy, the
// resource reports an error instead of taking it over.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="RECORDED-ID",type="string",JSONPath=".status.ownership.clientID"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,rauthy}
type Client struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClientSpec   `json:"spec"`
	Status ClientStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClientList contains a list of Client.
type ClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Client `json:"items"`
}
