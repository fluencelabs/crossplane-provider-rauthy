package client

import (
	"context"
	"fmt"
	"slices"
	"time"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// Ownership evidence lives in status.ownership. The API server ignores status
// on create and update of the main resource, so a manifest cannot carry it:
// only a writer of the clients/status subresource (the provider) can set it.
// Annotations, including the runtime's external-create-* marks, grant nothing.

// ownedClientID returns the id of the Rauthy client this resource created, or
// "" when it created none.
func ownedClientID(cr *v1alpha1.Client) string {
	if cr.Status.Ownership == nil {
		return ""
	}
	return cr.Status.Ownership.ClientID
}

// ownsClient reports whether this resource created the Rauthy client id.
func ownsClient(cr *v1alpha1.Client, id string) bool {
	owned := ownedClientID(cr)
	return owned != "" && owned == id
}

// setOwnership writes (or, with an empty id, clears) status.ownership through
// the status subresource, so the record is durable before the runtime's own
// updates of the object.
func setOwnership(ctx context.Context, status kube.SubResourceWriter, cr *v1alpha1.Client, id string) error {
	orig := cr.DeepCopy()
	if id == "" {
		cr.Status.Ownership = nil
	} else {
		cr.Status.Ownership = &v1alpha1.ClientOwnership{ClientID: id, CreatedAt: metav1.NewTime(time.Now())}
	}
	if err := status.Patch(ctx, cr, kube.MergeFrom(orig)); err != nil {
		return fmt.Errorf("cannot write status.ownership: %w", err)
	}
	return nil
}

// checkPolicies refuses management policies this resource cannot honour. A
// client can only be made to match the spec by the PUT, so Update is always
// required; Create is required until the resource has created its client,
// since without it the resource could only ever observe a client made
// elsewhere.
func checkPolicies(cr *v1alpha1.Client, owned bool) error {
	p := cr.GetManagementPolicies()
	has := func(a xpv2.ManagementAction) bool {
		return slices.Contains(p, xpv2.ManagementActionAll) || slices.Contains(p, a)
	}
	if !has(xpv2.ManagementActionUpdate) {
		return fmt.Errorf("management policies %v: %w", p, errPoliciesNoUpdate)
	}
	if !owned && !has(xpv2.ManagementActionCreate) {
		return fmt.Errorf("management policies %v: %w", p, errPoliciesNoCreate)
	}
	return nil
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

func outcomeUnknownError(id string) error {
	return fmt.Errorf(
		"create outcome unknown: the request failed but a client with id %q now exists; "+
			"verify and delete it in Rauthy to retry: %w",
		id, ErrAdoption,
	)
}
