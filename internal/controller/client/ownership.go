package client

import (
	"fmt"
	"slices"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// AnnotationKeyCreatedClientID records the id of the Rauthy client this
// resource created. Create writes it together with the runtime's
// external-create-succeeded annotation, so it pins ownership to one client id:
// a changed external name can never redirect an owned resource at a client it
// did not create.
const AnnotationKeyCreatedClientID = "rauthy.fluence.dev/created-client-id"

// createdByThis reports whether this resource created the Rauthy client it
// points at.
//
// The runtime marks every create attempt: external-create-pending before
// Create runs, then external-create-succeeded or external-create-failed after
// it returns. Ownership needs the latest attempt to have succeeded (a failed
// re-create after the client vanished does not inherit the old success) and
// the created-client-id annotation written by that successful Create.
func createdByThis(cr *v1alpha1.Client) bool {
	succeeded := meta.GetExternalCreateSucceeded(cr)
	if succeeded.IsZero() {
		return false
	}
	if meta.GetExternalCreateFailed(cr).After(succeeded) || meta.GetExternalCreatePending(cr).After(succeeded) {
		return false
	}
	return createdClientID(cr) != ""
}

func createdClientID(cr *v1alpha1.Client) string {
	return cr.GetAnnotations()[AnnotationKeyCreatedClientID]
}

// policiesAllowCreate reports whether the management policies let this
// resource create its client. Without Create a resource could only ever
// observe a client somebody else made, which is the adoption this provider
// refuses.
func policiesAllowCreate(cr *v1alpha1.Client) bool {
	p := cr.GetManagementPolicies()
	return slices.Contains(p, xpv2.ManagementActionAll) || slices.Contains(p, xpv2.ManagementActionCreate)
}

func validateClientID(id string) error {
	if id == "" {
		return errEmptyID
	}
	if !rauthy.ClientIDPattern().MatchString(id) {
		return fmt.Errorf("%w: %q (the external name) does not match %s", errInvalidID, id, rauthy.ClientIDPattern())
	}
	return nil
}

func adoptionError(id string) error {
	return fmt.Errorf(
		"client %q already exists in Rauthy and was not created by this resource; %w",
		id, ErrAdoption,
	)
}
