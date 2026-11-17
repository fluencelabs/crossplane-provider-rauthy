package client

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// Ownership evidence is status.ownership (the record, M) as the API server
// holds it. The API server ignores status on writes to the main resource, so
// only a writer of the clients/status subresource (the provider) can set it.
// Annotations, including the runtime's external-create-* marks, influence
// neither behaviour nor wording.
//
// For a confidential client the record is bound to the client by a
// fingerprint of the secret Rauthy issued when this resource created it; see
// secretFingerprint. A client with the same id but another secret (recreated
// by somebody else, or its secret rotated outside this provider) is not the
// one the record names. Every record is bound to the Rauthy API root; a public
// client has no secret, so its binding is the backend URL and id.
//
// Invariant: M names a client id only while this resource's client (C) with
// that id exists in Rauthy. Every write of M is confirmed by an uncached read
// when its result is in doubt, and any decision to act as the owner is taken
// from an uncached read plus, for a confidential client, the fingerprint.
//
// States, with M as the API server holds it and C the Rauthy client:
//
//	S0   M absent,  C absent      Observe: not exists -> Create.
//	S1   M absent,  C present     refused: not ours (or a leaked create of ours).
//	S4   M = id on another Client refused, whatever C is: neither copy may act
//	                              as the owner, and none may create the id.
//	S2   M = id,    C ours        owned: observe, update, delete.
//	S2'  M = id,    C different   refused: C is not the client M was recorded
//	                              for (secret differs, or confidentiality
//	                              differs). Never published, updated or
//	                              deleted; deleting the resource finalizes
//	                              without touching Rauthy. M is left as is.
//	S3   M = id,    C absent      transient: Observe clears M (confirmed)
//	                              before reporting "not exists".
//
// Transitions:
//
//	S0 -> S1   POST succeeds, then the secret cannot be read or M cannot be
//	           recorded, and the rollback DELETE fails; or the provider stops
//	           before recording M.
//	S0 -> S2   POST succeeds, the secret is read and M (id and fingerprint) is
//	           recorded, confirmed by the patch answer or an uncached read.
//	S2 -> S3   C deleted, by Delete (which clears M right after the DELETE) or
//	           out of band (Observe clears M on the 404).
//	S3 -> S0   M cleared and confirmed.
//	S2 -> S2'  Out of band: C deleted and recreated by somebody else before
//	           Observe saw the 404, or its secret rotated.
//	S2' -> S3  Somebody deletes C in Rauthy.
//
// Crash points: before the POST leaves S0; between the POST and M leaves S1
// (refused until someone deletes C); after M leaves S2; between a DELETE and
// clearing M leaves S3, which the next Observe repairs. For public clients the
// S2 -> S2' transition cannot be detected: a recreated public client with the
// same id looks like S2.

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

// owner is what this resource is to an existing Rauthy client.
type owner int

const (
	// notOwned: no record names the client.
	notOwned owner = iota
	// otherClient: the record names the id, but the client is not the one it
	// was recorded for (S2').
	otherClient
	// ownedClient: the record names the client and the binding holds (S2).
	ownedClient
)

// verifyOwner decides, from an uncached read of the Client and the binding,
// whether this resource owns the Rauthy client got. For a confidential client
// it returns the secret it read, so the caller need not fetch it again. The
// answer is remembered for the rest of this reconcile.
func (e *external) verifyOwner(
	ctx context.Context,
	cr *v1alpha1.Client,
	id string,
	got *rauthy.ClientResponse,
) (owner, *string, error) {
	e.confirmed = ""
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return notOwned, nil, fmt.Errorf("cannot confirm ownership of client %q: %w", id, err)
	}
	cr.Status.Ownership = fresh.Status.Ownership
	if err = checkRecordID(fresh, id); err != nil {
		return notOwned, nil, err
	}
	rec := fresh.Status.Ownership
	if rec != nil && rec.BackendURL != e.backendURL {
		return notOwned, nil, errBackendChanged
	}
	if rec == nil {
		return notOwned, nil, nil
	}
	taken, err := e.recordedElsewhereCached(ctx, fresh, id)
	if err != nil || taken {
		return notOwned, nil, err
	}
	if !got.Confidential {
		// A public client has no secret: the binding is the backend URL and id. A
		// fingerprint means the client was confidential when recorded.
		if rec.SecretFingerprint != "" {
			return otherClient, nil, nil
		}
		e.confirmed = id
		return ownedClient, nil, nil
	}
	if rec.SecretFingerprint == "" {
		return otherClient, nil, nil
	}
	s, err := e.api.GetClientSecret(ctx, id)
	if err != nil {
		return notOwned, nil, fmt.Errorf("cannot read the secret of Rauthy client %q to confirm ownership: %w", id, err)
	}
	if s.Secret == nil {
		return otherClient, nil, nil
	}
	current := secretFingerprint(id, *s.Secret)
	if subtle.ConstantTimeCompare([]byte(current), []byte(rec.SecretFingerprint)) != 1 {
		return otherClient, nil, nil
	}
	e.confirmed = id
	return ownedClient, s.Secret, nil
}

// ClientIDIndex is the cache field index of Clients by the client id their
// status.ownership records.
const ClientIDIndex = "status.ownership.clientID"

// IndexClientID extracts the ClientIDIndex value of a Client: the client id
// its record names, if any.
func IndexClientID(o kube.Object) []string {
	cr, ok := o.(*v1alpha1.Client)
	if !ok {
		return nil
	}
	if id := ownedClientID(cr); id != "" {
		return []string{id}
	}
	return nil
}

// recordedElsewhere reports whether another Client, in any namespace, records
// id and backend URL in its status.ownership. Client ids are global within a Rauthy instance while Clients
// are namespaced, and a public client's record is bound by the URL and id: a
// record held by two objects cannot tell which of them created the client, so
// neither may act as its owner, and no Client may create an id another one
// records.
//
// This is the authoritative check: every Client is listed from the API
// server, bypassing the cache. Create runs it right before the POST and
// Delete right before the DELETE.
func (e *external) recordedElsewhere(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	all := &v1alpha1.ClientList{}
	if err := e.reader.List(ctx, all); err != nil {
		return false, fmt.Errorf("cannot confirm ownership of client %q: cannot list Clients without the cache: %w",
			id, err)
	}
	return e.refusedFor(cr, otherRecord(all.Items, cr, id, e.backendURL), id), nil
}

// recordedElsewhereCached is recordedElsewhere answered from the cache,
// through the ClientIDIndex, for the paths that run on every poll. It can miss
// a record written within the cache's lag, and everything Observe decides as
// the owner rests on it: publishing the connection details (a confidential
// client's secret included), reporting the client as existing and Ready, and
// the Update that follows. Create and Delete do not: they repeat the check
// uncached right before the POST and the DELETE.
func (e *external) recordedElsewhereCached(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	if e.cache == nil {
		return e.recordedElsewhere(ctx, cr, id)
	}
	same := &v1alpha1.ClientList{}
	if err := e.cache.List(ctx, same, kube.MatchingFields{ClientIDIndex: id}); err != nil {
		return false, fmt.Errorf("cannot confirm ownership of client %q: cannot look up other records: %w", id, err)
	}
	return e.refusedFor(cr, otherRecord(same.Items, cr, id, e.backendURL), id), nil
}

// refusedFor reports whether other blocks cr and, if so, names other in the
// provider's log so that an operator can find the holder. The resource's
// conditions and events never name it: a tenant must not learn another
// namespace's objects from a refusal.
func (e *external) refusedFor(cr, other *v1alpha1.Client, id string) bool {
	if other == nil {
		return false
	}
	e.log.Info("Refusing a client id another Client records",
		"client-id", id,
		"refused", kube.ObjectKeyFromObject(cr).String(),
		"recorded-by", kube.ObjectKeyFromObject(other).String(),
	)
	return true
}

// otherRecord returns a Client among items, other than cr, that records id.
func otherRecord(items []v1alpha1.Client, cr *v1alpha1.Client, id, backendURL string) *v1alpha1.Client {
	for i := range items {
		if other := &items[i]; other.GetUID() != cr.GetUID() && ownedClientID(other) == id &&
			other.Status.Ownership.BackendURL == backendURL {
			return other
		}
	}
	return nil
}

// ensureOwned is verifyOwner for the acting paths, reusing a confirmation
// already made in this reconcile. A client that no longer exists is not
// owned: there is nothing to act on.
func (e *external) ensureOwned(ctx context.Context, cr *v1alpha1.Client, id string) (owner, error) {
	if err := e.checkBackend(ctx, cr); err != nil {
		return notOwned, err
	}
	if id != "" && e.confirmed == id {
		return ownedClient, nil
	}
	got, err := e.lookup(ctx, id)
	if err != nil || got == nil {
		return notOwned, err
	}
	o, _, err := e.verifyOwner(ctx, cr, id, got)
	return o, err
}

// patchOwnership writes the marker (nil clears it) and reports what the API
// server holds afterwards: the patch answer when it succeeded, an uncached
// read when it failed. known is false when neither could be learned.
//
// The answer is decoded into an empty object, not into cr: the client decodes
// it without zeroing the target first, so a field the server pruned (absent
// from the answer) would otherwise keep the marker cr was given. The answer
// then replaces cr whole, as decoding into cr used to: cr carries the
// server's current spec, metadata and resourceVersion together, so a later
// Update of cr cannot write back a spec or finalizers that changed meanwhile.
func (e *external) patchOwnership(
	ctx context.Context,
	cr *v1alpha1.Client,
	want *v1alpha1.ClientOwnership,
) (*v1alpha1.ClientOwnership, bool, error) {
	desired := cr.DeepCopy()
	desired.Status.Ownership = want
	patch, err := kube.MergeFrom(cr).Data(desired)
	if err != nil {
		return nil, false, fmt.Errorf("cannot build the status.ownership patch: %w", err)
	}
	answer := &v1alpha1.Client{}
	answer.SetNamespace(cr.GetNamespace())
	answer.SetName(cr.GetName())
	patchErr := e.status.Patch(ctx, answer, kube.RawPatch(types.MergePatchType, patch))
	if patchErr == nil {
		// The answer is the object as stored.
		answer.DeepCopyInto(cr)
		return cr.Status.Ownership, true, nil
	}
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return nil, false, fmt.Errorf(
			"writing status.ownership failed (%w) and its outcome is unknown (%w)",
			patchErr,
			err,
		)
	}
	cr.Status.Ownership = fresh.Status.Ownership
	return fresh.Status.Ownership, true, patchErr
}

// sameRecord reports whether the stored record is the one that was written.
func sameRecord(stored, want *v1alpha1.ClientOwnership) bool {
	return stored != nil && stored.ClientID == want.ClientID && stored.BackendURL == want.BackendURL &&
		stored.SecretFingerprint == want.SecretFingerprint
}

// clearOwnership removes the marker and returns nil only once the API server
// is known not to hold one.
func (e *external) clearOwnership(ctx context.Context, cr *v1alpha1.Client) error {
	e.confirmed = ""
	stored, known, err := e.patchOwnership(ctx, cr, nil)
	switch {
	case !known:
		return fmt.Errorf("cannot clear status.ownership: %w", err)
	case stored != nil:
		return fmt.Errorf("cannot clear status.ownership (still %q): %w", stored.ClientID, err)
	}
	return nil
}

// recordOwnership records the marker, bound to fingerprint, for a client
// Rauthy just created.
//
//   - Recorded (by the patch answer or an uncached read): success.
//   - Known not to be recorded: the client is deleted again, so the next
//     reconcile starts from S0 instead of refusing it forever.
//   - Unknown: neither delete nor claim. The next Observe decides from an
//     uncached read: owned if the marker landed, refused otherwise.
func (e *external) recordOwnership(ctx context.Context, cr *v1alpha1.Client, id, fingerprint string) error {
	if e.crd.Load() {
		return errCRDLacksOwnership
	}
	marker := &v1alpha1.ClientOwnership{
		ClientID:          id,
		BackendURL:        e.backendURL,
		CreatedAt:         metav1.NewTime(time.Now()),
		SecretFingerprint: fingerprint,
	}
	stored, known, err := e.patchOwnership(ctx, cr, marker)
	switch {
	case !known:
		return fmt.Errorf(
			"created Rauthy client %q but the outcome of recording its ownership is unknown: %w", id, err)
	case sameRecord(stored, marker):
		return nil
	case stored != nil:
		// Something else is recorded: never replace it, never claim.
		return fmt.Errorf("created Rauthy client %q but another ownership record is stored: %w", id, err)
	case err == nil:
		// The patch succeeded and the field is gone: the installed CRD prunes
		// it. Every further create would end the same way, so stop creating.
		e.crd.Store(true)
		err = errCRDLacksOwnership
	}
	cr.Status.Ownership = nil
	return e.rollback(ctx, id, err)
}

// secretFingerprint is the binding between a record and a confidential
// client: a domain-separated SHA-256 of the client id and the secret. It
// cannot be reversed into the secret.
func secretFingerprint(id, secret string) string {
	sum := sha256.Sum256([]byte(fingerprintDomain + "\x00" + id + "\x00" + secret))
	return hex.EncodeToString(sum[:])
}

const fingerprintDomain = "rauthy-client-secret-v1"

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
	if errors.Is(cause, errCRDLacksOwnership) {
		return fmt.Errorf(
			"created Rauthy client %q but could not record its ownership, so it was deleted again; no further "+
				"clients are created until the provider restarts with current CRDs: %w",
			id, cause,
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

// notOwnedError is the one message for a client id this resource holds no
// record of that it may act on, whatever the reason: a client with the id
// exists in Rauthy without a record of this resource, or another Client
// records the id. It never names the other Client.
func notOwnedError(id string) error {
	return fmt.Errorf(
		"a client with id %q exists in Rauthy or is recorded by another Client, and this resource holds no "+
			"ownership record for it, so it is not managed; if an interrupted create of this resource made it, "+
			"delete it in Rauthy to retry, otherwise choose another id: %w",
		id, ErrAdoption,
	)
}

// otherClientError: the record names this id, but the client in Rauthy is not
// the one it was recorded for (S2').
func otherClientError(id string) error {
	return fmt.Errorf(
		"the client with id %q in Rauthy is not the one this resource created (its secret differs: it was "+
			"recreated or its secret was rotated outside this provider), so it is not managed; delete that "+
			"client in Rauthy to let this resource create its own again, or delete this Client, which leaves "+
			"the Rauthy client alone: %w",
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

// checkBackend refuses a switch even when the destination has no client.
// Status is read uncached so a stale object cannot discard the original binding.
func (e *external) checkBackend(ctx context.Context, cr *v1alpha1.Client) error {
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return err
	}
	cr.Status.Ownership = fresh.Status.Ownership
	if rec := cr.Status.Ownership; rec != nil && rec.BackendURL != e.backendURL {
		return errBackendChanged
	}
	return checkRecordID(cr, meta.GetExternalName(cr))
}
