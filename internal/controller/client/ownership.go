package client

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// AnnotationKeyCreateRefused marks that Rauthy refused this resource's create
// of the given id because the id was taken. It only chooses the wording of
// the error reported afterwards and grants nothing.
const AnnotationKeyCreateRefused = "rauthy.fluence.dev/create-refused"

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
// updates of the object. On any failure the in-memory record is restored, so
// the runtime's final status update cannot persist what the API server
// refused.
func setOwnership(ctx context.Context, status kube.SubResourceWriter, cr *v1alpha1.Client, id string) error {
	prev := cr.Status.Ownership
	orig := cr.DeepCopy()
	if id == "" {
		cr.Status.Ownership = nil
	} else {
		cr.Status.Ownership = &v1alpha1.ClientOwnership{ClientID: id, CreatedAt: metav1.NewTime(time.Now())}
	}
	if err := status.Patch(ctx, cr, kube.MergeFrom(orig)); err != nil {
		cr.Status.Ownership = prev
		return fmt.Errorf("cannot write status.ownership: %w", err)
	}
	// The patch answers with the object as stored. A CRD without
	// status.ownership prunes the field and still reports success.
	if ownedClientID(cr) != id {
		cr.Status.Ownership = prev
		return errCRDLacksOwnership
	}
	return nil
}

// confirmOwnership decides ownership from the Client as the API server holds
// it right now, bypassing the informer cache: a cached object can still carry
// a record that has since been cleared. The object must also be the same one
// (same UID) as the one being reconciled. The answer is remembered for the
// rest of this reconcile.
func (e *external) confirmOwnership(
	ctx context.Context,
	cr *v1alpha1.Client,
	id string,
) (*v1alpha1.Client, bool, error) {
	fresh := &v1alpha1.Client{}
	if err := e.reader.Get(ctx, kube.ObjectKeyFromObject(cr), fresh); err != nil {
		return nil, false, fmt.Errorf("cannot confirm ownership of client %q with an uncached read: %w", id, err)
	}
	if fresh.GetUID() != cr.GetUID() || !ownsClient(fresh, id) {
		e.confirmed = ""
		return fresh, false, nil
	}
	e.confirmed = id
	cr.Status.Ownership = fresh.Status.Ownership
	return fresh, true, nil
}

// ensureOwned is confirmOwnership for the acting paths, reusing a
// confirmation already made in this reconcile.
func (e *external) ensureOwned(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	if e.confirmed != "" && e.confirmed == id {
		return true, nil
	}
	_, ok, err := e.confirmOwnership(ctx, cr, id)
	return ok, err
}

// notOwnedError explains why an existing client is refused. The wording uses
// the create attempts on record as a hint only; none of them grants
// ownership. A create this resource attempted that ended ambiguously (or was
// interrupted) may have made the client, so the outcome is reported as
// unknown; a create Rauthy refused, or no attempt at all, means the client
// belongs to somebody else.
func notOwnedError(cr *v1alpha1.Client, id string) error {
	if cr.GetAnnotations()[AnnotationKeyCreateRefused] == id {
		return adoptionError(id)
	}
	if !meta.GetExternalCreatePending(cr).IsZero() || !meta.GetExternalCreateFailed(cr).IsZero() {
		return outcomeUnknownError(id)
	}
	return adoptionError(id)
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
