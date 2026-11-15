package client

import (
	"slices"
	"strings"

	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// createRequest renders the body of POST /clients. Rauthy accepts nothing
// beyond these fields there; the rest of the spec reaches Rauthy in the first
// Update.
func createRequest(id string, p *v1alpha1.ClientParameters) rauthy.NewClientRequest {
	return rauthy.NewClientRequest{
		ID:                     id,
		Name:                   p.Name,
		Confidential:           p.Confidential,
		RedirectURIs:           p.RedirectURIs,
		PostLogoutRedirectURIs: p.PostLogoutRedirectURIs,
	}
}

// updateRequest renders the body of PUT /clients/{id}, a full replacement.
//
// The body starts as the observed client, every setting included, and the
// fields the spec sets are laid over it. A field the spec leaves unset, and
// every setting the spec does not expose (SCIM, allowed resources, default
// audiences, custom claims), keeps what Rauthy holds.
func updateRequest(id string, p *v1alpha1.ClientParameters, obs *rauthy.ClientResponse) rauthy.UpdateClientRequest {
	req := obs.UpdateRequest()
	req.ID = id
	req.Name = orPtr(p.Name, req.Name)
	req.Confidential = p.Confidential
	req.RedirectURIs = p.RedirectURIs
	req.PostLogoutRedirectURIs = orList(p.PostLogoutRedirectURIs, req.PostLogoutRedirectURIs)
	req.AllowedOrigins = orList(p.AllowedOrigins, req.AllowedOrigins)
	req.Enabled = orValue(p.Enabled, req.Enabled)
	req.FlowsEnabled = orList(p.FlowsEnabled, req.FlowsEnabled)
	req.AccessTokenAlg = orValue(p.AccessTokenAlg, req.AccessTokenAlg)
	req.IDTokenAlg = orValue(p.IDTokenAlg, req.IDTokenAlg)
	req.AuthCodeLifetime = orValue(p.AuthCodeLifetime, req.AuthCodeLifetime)
	req.AccessTokenLifetime = orValue(p.AccessTokenLifetime, req.AccessTokenLifetime)
	req.Scopes = orList(p.Scopes, req.Scopes)
	req.DefaultScopes = orList(p.DefaultScopes, req.DefaultScopes)
	req.Challenges = orList(p.Challenges, req.Challenges)
	req.ForceMFA = orValue(p.ForceMFA, req.ForceMFA)
	req.ClientURI = orPtr(p.ClientURI, req.ClientURI)
	req.Contacts = orList(p.Contacts, req.Contacts)
	req.BackchannelLogoutURI = orPtr(p.BackchannelLogoutURI, req.BackchannelLogoutURI)
	req.RestrictGroupPrefix = orPtr(p.RestrictGroupPrefix, req.RestrictGroupPrefix)
	return req
}

// differences lists the spec fields whose value Rauthy does not hold. Only
// fields the spec sets are compared, lists as sets.
func differences(p *v1alpha1.ClientParameters, obs *rauthy.ClientResponse) []string {
	var diff []string
	check := func(field string, equal bool) {
		if !equal {
			diff = append(diff, field)
		}
	}

	check("name", ptrEqual(p.Name, obs.Name))
	check("confidential", p.Confidential == obs.Confidential)
	check("enabled", valueEqual(p.Enabled, obs.Enabled))
	check("redirectUris", setEqual(p.RedirectURIs, obs.RedirectURIs))
	check("postLogoutRedirectUris", listEqual(p.PostLogoutRedirectURIs, obs.PostLogoutRedirectURIs))
	check("allowedOrigins", listEqual(p.AllowedOrigins, obs.AllowedOrigins))
	check("flowsEnabled", listEqual(p.FlowsEnabled, obs.FlowsEnabled))
	check("accessTokenAlg", valueEqual(p.AccessTokenAlg, obs.AccessTokenAlg))
	check("idTokenAlg", valueEqual(p.IDTokenAlg, obs.IDTokenAlg))
	check("authCodeLifetime", valueEqual(p.AuthCodeLifetime, obs.AuthCodeLifetime))
	check("accessTokenLifetime", valueEqual(p.AccessTokenLifetime, obs.AccessTokenLifetime))
	check("scopes", listEqual(p.Scopes, obs.Scopes))
	check("defaultScopes", listEqual(p.DefaultScopes, obs.DefaultScopes))
	check("challenges", listEqual(p.Challenges, obs.Challenges))
	check("forceMfa", valueEqual(p.ForceMFA, obs.ForceMFA))
	check("clientUri", ptrEqual(p.ClientURI, obs.ClientURI))
	check("contacts", listEqual(p.Contacts, obs.Contacts))
	check("backchannelLogoutUri", ptrEqual(p.BackchannelLogoutURI, obs.BackchannelLogoutURI))
	check("restrictGroupPrefix", ptrEqual(p.RestrictGroupPrefix, obs.RestrictGroupPrefix))

	return diff
}

// observation copies the client as Rauthy reports it into status.atProvider.
func observation(obs *rauthy.ClientResponse) v1alpha1.ClientObservation {
	return v1alpha1.ClientObservation{
		ID:                     obs.ID,
		Name:                   obs.Name,
		Confidential:           obs.Confidential,
		Enabled:                obs.Enabled,
		RedirectURIs:           obs.RedirectURIs,
		PostLogoutRedirectURIs: obs.PostLogoutRedirectURIs,
		AllowedOrigins:         obs.AllowedOrigins,
		FlowsEnabled:           obs.FlowsEnabled,
		AccessTokenAlg:         obs.AccessTokenAlg,
		IDTokenAlg:             obs.IDTokenAlg,
		AuthCodeLifetime:       obs.AuthCodeLifetime,
		AccessTokenLifetime:    obs.AccessTokenLifetime,
		Scopes:                 obs.Scopes,
		DefaultScopes:          obs.DefaultScopes,
		Challenges:             obs.Challenges,
		ForceMFA:               obs.ForceMFA,
		ClientURI:              obs.ClientURI,
		Contacts:               obs.Contacts,
		BackchannelLogoutURI:   obs.BackchannelLogoutURI,
		RestrictGroupPrefix:    obs.RestrictGroupPrefix,
	}
}

// issuerFor returns the issuer published to consumers: the configured one, or
// the Rauthy URL with its API base path.
func issuerFor(url string, issuer *string) string {
	if issuer != nil && *issuer != "" {
		return *issuer
	}
	base := strings.TrimRight(url, "/")
	if strings.HasSuffix(base, rauthy.APIBasePath) {
		return base
	}
	return base + rauthy.APIBasePath
}

func orPtr[T any](desired, observed *T) *T {
	if desired != nil {
		return desired
	}
	return observed
}

func orValue[T any](desired *T, observed T) T {
	if desired != nil {
		return *desired
	}
	return observed
}

func orList(desired, observed []string) []string {
	if len(desired) > 0 {
		return desired
	}
	return observed
}

func ptrEqual[T comparable](desired, observed *T) bool {
	if desired == nil {
		return true
	}
	return observed != nil && *desired == *observed
}

func valueEqual[T comparable](desired *T, observed T) bool {
	return desired == nil || *desired == observed
}

// listEqual compares an optional list: an unset (empty) desired list matches
// anything.
func listEqual(desired, observed []string) bool {
	return len(desired) == 0 || setEqual(desired, observed)
}

// setEqual compares two lists as sets: Rauthy stores these fields without a
// meaningful order.
func setEqual(a, b []string) bool {
	return slices.Equal(normalise(a), normalise(b))
}

func normalise(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}
