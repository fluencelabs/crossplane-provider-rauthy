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

// Ownership evidence is status.ownership (the marker, M) as the API server
// holds it. The API server ignores status on writes to the main resource, so
// only a writer of the clients/status subresource (the provider) can set it.
// Annotations, including the runtime's external-create-* marks, influence
// neither behaviour nor wording.
//
// Invariant: M names a client id only while this resource's client (C) with
// that id exists in Rauthy. Every write of M is confirmed by an uncached read
// when its result is in doubt, and any decision to act as the owner is taken
// from an uncached read.
//
// States, with M as the API server holds it and C the Rauthy client:
//
//	S0  M absent,  C absent   Observe: not exists -> Create.
//	S1  M absent,  C present  refused: not ours (or a leaked create of ours).
//	S2  M = id,    C present  owned: observe, update, delete.
//	S3  M = id,    C absent   transient: Observe clears M (confirmed) before
//	                          reporting "not exists"; nothing else happens.
//
// Transitions:
//
//	S0 -> S1  POST succeeds, then M cannot be recorded and the rollback DELETE
//	          fails, or the provider stops before recording M.
//	S0 -> S2  POST succeeds and M is recorded (confirmed by the patch answer or
//	          by an uncached read after a failed patch).
//	S2 -> S3  C deleted, by Delete or out of band. Delete clears M right after
//	          the DELETE; Observe clears it on a 404.
//	S3 -> S0  M cleared and confirmed.
//
// Crash points: before the POST leaves S0; between the POST and M leaves S1
// (refused until someone deletes C); after M leaves S2; between a DELETE and
// clearing M leaves S3, which the next Observe repairs. The window that cannot
// be closed: in S3, a third party creating the same id before M is cleared is
// indistinguishable from S2.

// detachedTimeout bounds the repair calls (re-reads, clears and rollbacks)
// that run on a context detached from the reconcile, whose deadline is the
// likeliest reason the first attempt failed.
const detachedTimeout = 30 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachedTimeout)
}

// ownedClientID returns the id the marker of this copy of the resource names,
// or "".
func ownedClientID(cr *v1alpha1.Client) string {
	if cr.Status.Ownership == nil {
		return ""
	}
	return cr.Status.Ownership.ClientID
}

// readFresh reads the Client from the API server, bypassing the cache, on a
// detached bounded context. A different UID means the object was replaced and
// counts as an error.
func (e *external) readFresh(ctx context.Context, cr *v1alpha1.Client) (*v1alpha1.Client, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	fresh := &v1alpha1.Client{}
	if err := e.reader.Get(ctx, kube.ObjectKeyFromObject(cr), fresh); err != nil {
		return nil, fmt.Errorf("cannot read the Client without the cache: %w", err)
	}
	if fresh.GetUID() != cr.GetUID() {
		return nil, fmt.Errorf("the Client %s was replaced while being reconciled", kube.ObjectKeyFromObject(cr))
	}
	return fresh, nil
}

// confirmOwnership decides ownership from an uncached read, and remembers the
// answer for the rest of this reconcile.
func (e *external) confirmOwnership(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return false, fmt.Errorf("cannot confirm ownership of client %q: %w", id, err)
	}
	cr.Status.Ownership = fresh.Status.Ownership
	if ownedClientID(fresh) != id {
		e.confirmed = ""
		return false, nil
	}
	e.confirmed = id
	return true, nil
}

// ensureOwned is confirmOwnership for the acting paths, reusing a
// confirmation already made in this reconcile.
func (e *external) ensureOwned(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	if e.confirmed != "" && e.confirmed == id {
		return true, nil
	}
	return e.confirmOwnership(ctx, cr, id)
}

// patchOwnership writes the marker (nil clears it) and reports what the API
// server holds afterwards: the patch answer when it succeeded, an uncached
// read when it failed. known is false when neither could be learned.
func (e *external) patchOwnership(
	ctx context.Context,
	cr *v1alpha1.Client,
	want *v1alpha1.ClientOwnership,
) (string, bool, error) {
	orig := cr.DeepCopy()
	cr.Status.Ownership = want
	patchErr := e.status.Patch(ctx, cr, kube.MergeFrom(orig))
	if patchErr == nil {
		// The answer is the object as stored.
		return ownedClientID(cr), true, nil
	}
	cr.Status.Ownership = orig.Status.Ownership
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return "", false, fmt.Errorf(
			"writing status.ownership failed (%w) and its outcome is unknown (%w)",
			patchErr,
			err,
		)
	}
	cr.Status.Ownership = fresh.Status.Ownership
	return ownedClientID(fresh), true, patchErr
}

// clearOwnership removes the marker and returns nil only once the API server
// is known not to hold one.
func (e *external) clearOwnership(ctx context.Context, cr *v1alpha1.Client) error {
	e.confirmed = ""
	stored, known, err := e.patchOwnership(ctx, cr, nil)
	switch {
	case !known:
		return fmt.Errorf("cannot clear status.ownership: %w", err)
	case stored != "":
		return fmt.Errorf("cannot clear status.ownership (still %q): %w", stored, err)
	}
	return nil
}

// recordOwnership records the marker for a client Rauthy just created.
//
//   - Recorded (by the patch answer or an uncached read): success.
//   - Known not to be recorded: the client is deleted again, so the next
//     reconcile starts from S0 instead of refusing it forever.
//   - Unknown: neither delete nor claim. The next Observe decides from an
//     uncached read: owned if the marker landed, refused otherwise.
func (e *external) recordOwnership(ctx context.Context, cr *v1alpha1.Client, id string) error {
	if e.crd.Load() {
		return errCRDLacksOwnership
	}
	marker := &v1alpha1.ClientOwnership{ClientID: id, CreatedAt: metav1.NewTime(time.Now())}
	stored, known, err := e.patchOwnership(ctx, cr, marker)
	switch {
	case !known:
		return fmt.Errorf(
			"created Rauthy client %q but the outcome of recording its ownership is unknown: %w", id, err)
	case stored == id:
		return nil
	case err == nil:
		// The patch succeeded and the field is gone: the installed CRD prunes
		// it. Every further create would end the same way, so stop creating.
		e.crd.Store(true)
		err = errCRDLacksOwnership
	}
	cr.Status.Ownership = nil
	return e.rollback(ctx, id, err)
}

// rollback deletes a client whose ownership is known not to be recorded.
func (e *external) rollback(ctx context.Context, id string, cause error) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := e.api.DeleteClient(ctx, id); err != nil && !rauthy.IsNotFound(err) {
		return fmt.Errorf(
			"created Rauthy client %q but could not record its ownership (%w), and could not delete it again "+
				"(%w): the resource will refuse it; delete client %q in Rauthy to retry",
			id, cause, err, id,
		)
	}
	return fmt.Errorf(
		"created Rauthy client %q but could not record its ownership, so it was deleted again and "+
			"will be recreated: %w",
		id, cause,
	)
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

// notOwnedError is the one message for an existing client this resource holds
// no record of, whatever the reason.
func notOwnedError(id string) error {
	return fmt.Errorf(
		"a client with id %q exists in Rauthy and this resource holds no ownership record for it, so it is "+
			"not managed; if an interrupted create of this resource made it, delete it in Rauthy to retry, "+
			"otherwise choose another id: %w",
		id, ErrAdoption,
	)
}

// refusedError: Rauthy refused this resource's create because the id is taken.
func refusedError(id string, postErr error) error {
	return fmt.Errorf(
		"client %q already exists in Rauthy and was not created by this resource; choose another id; %w (%w)",
		id, ErrAdoption, postErr,
	)
}

// outcomeUnknownError: this resource's create failed ambiguously and a client
// with the id exists.
func outcomeUnknownError(id string, postErr error) error {
	return fmt.Errorf(
		"create outcome unknown: the request failed but a client with id %q now exists; "+
			"verify and delete it in Rauthy to retry: %w (%w)",
		id, ErrAdoption, postErr,
	)
}
