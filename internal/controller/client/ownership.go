package client

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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
// A finalized M binds this resource to its client (C). A pending M is only
// a reservation until its random name is verified. Every write is confirmed
// by an uncached read
// when its result is in doubt, and any decision to act as the owner is taken
// from an uncached read plus, for a confidential client, the fingerprint.
//
// States, with M as the API server holds it and C the Rauthy client:
//
//	S0   M absent,  C absent      Observe: not exists -> Create.
//	SP   M pending, C any        Verify creation name, then finalize or refuse.
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
// Creation first writes a pending record with a hash of a random temporary
// Rauthy name. POST sends that name. Pending records permit no publication or
// mutation until the name is confirmed, the secret is read, and the final
// binding is durably recorded. Any transient failure resumes on the next
// reconcile. A missing remote client retains the pending record for retry. A mismatched
// name is refused. The first owned Update restores the requested name.
//
// A replacement public client after finalization still follows the documented
// backend-and-id policy. Confidential clients are confirmed by secret before
// every write; Rauthy has no conditional mutation to close the final read/write
// gap.

// detachedTimeout bounds the repair calls (re-reads and clears)
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
	if ownedClientID(fresh) == "" {
		fresh.Status.Ownership = nil
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
// it returns the secret it read, so the caller need not fetch it again.
func (e *external) verifyOwner(
	ctx context.Context,
	cr *v1alpha1.Client,
	id string,
	got *rauthy.ClientResponse,
) (owner, *rauthy.Secret, error) {
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return notOwned, nil, fmt.Errorf("cannot confirm ownership of client %q: %w", id, err)
	}
	cr.Status.Ownership = fresh.Status.Ownership
	if err = checkRecordID(fresh, id); err != nil {
		return notOwned, nil, err
	}
	if err = e.bindBackend(ctx, fresh); err != nil {
		return notOwned, nil, err
	}
	cr.Status.Ownership = fresh.Status.Ownership
	rec := fresh.Status.Ownership
	if rec == nil {
		return notOwned, nil, nil
	}
	if rec.Pending {
		return e.finishCreation(ctx, cr, got)
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
// records. Pending proofs do not claim the id or block its actual creator.
//
// This is the authoritative check: every Client is listed from the API
// server, bypassing the cache. Create runs it right before the POST; Delete
// runs it before confirming the client itself and then sending the DELETE.
func (e *external) recordedElsewhere(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	all := &v1alpha1.ClientList{}
	if err := e.reader.List(ctx, all); err != nil {
		return false, fmt.Errorf("cannot confirm ownership of client %q: cannot list Clients without the cache: %w",
			id, err)
	}
	return e.refusedFor(cr, otherRecord(all.Items, cr, id, e.backend), id), nil
}

// recordedElsewhereCached is recordedElsewhere answered from the cache,
// through the ClientIDIndex, for the paths that run on every poll. It can miss
// a record written within the cache's lag, and everything Observe decides as
// the owner rests on it: publishing the connection details (a confidential
// client's secret included), reporting the client as existing and Ready, and
// the Update that follows. Create and Delete do not: they run the uncached
// check before the POST and the DELETE.
func (e *external) recordedElsewhereCached(ctx context.Context, cr *v1alpha1.Client, id string) (bool, error) {
	if e.cache == nil {
		return e.recordedElsewhere(ctx, cr, id)
	}
	same := &v1alpha1.ClientList{}
	if err := e.cache.List(ctx, same, kube.MatchingFields{ClientIDIndex: id}); err != nil {
		return false, fmt.Errorf("cannot confirm ownership of client %q: cannot look up other records: %w", id, err)
	}
	return e.refusedFor(cr, otherRecord(same.Items, cr, id, e.backend), id), nil
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

// otherRecord returns another finalized record for id. Pending records prove
// only an attempted create; the random Rauthy name decides which attempt won.
func otherRecord(items []v1alpha1.Client, cr *v1alpha1.Client, id, backend string) *v1alpha1.Client {
	for i := range items {
		// A record without a backend URL predates the binding: it was made
		// through the only backend its resource had, which may be this one.
		if other := &items[i]; other.GetUID() != cr.GetUID() && ownedClientID(other) == id &&
			!other.Status.Ownership.Pending &&
			(other.Status.Ownership.Backend == backend || other.Status.Ownership.Backend == "") {
			return other
		}
	}
	return nil
}

// ensureOwned reads the client and confirms, uncached, that this resource
// owns it, right before a write. It returns the client it read, so the write
// is built from the state it confirmed: a confirmation cached from Observe
// would let a client deleted and recreated with a new secret in between be
// updated or deleted by a resource that no longer owns it.
func (e *external) ensureOwned(
	ctx context.Context,
	cr *v1alpha1.Client,
	id string,
) (owner, *rauthy.ClientResponse, error) {
	if err := e.checkBackend(ctx, cr); err != nil {
		return notOwned, nil, err
	}
	got, err := e.lookup(ctx, id)
	if err != nil || got == nil {
		return notOwned, nil, err
	}
	o, _, err := e.verifyOwner(ctx, cr, id, got)
	return o, got, err
}

// patchOwnership writes the marker (nil clears it) and reports what the API
// server holds afterwards: the patch answer when it succeeded, an uncached
// read when it failed. known is false when neither could be learned.
//
// The answer is decoded into an empty object, not into cr, so that a field the
// server pruned (absent from the answer) cannot keep the marker cr was given
// whatever the decoder does with absent fields. (The typed client against a
// real API server resets the target before decoding; the envtest suite shows
// it. Decoding into a fresh object does not depend on that.) The answer then
// replaces cr whole, as decoding into cr used to: cr carries the
// server's current spec, metadata and resourceVersion together, so a later
// Update of cr cannot write back a spec or finalizers that changed meanwhile.
func (e *external) patchOwnership(
	ctx context.Context,
	cr *v1alpha1.Client,
	want *v1alpha1.ClientOwnership,
) (*v1alpha1.ClientOwnership, bool, error) {
	desired := cr.DeepCopy()
	desired.Status.Ownership = want
	base := kube.MergeFrom(cr)
	if (want != nil && want.CreationNameHash != "") ||
		(cr.Status.Ownership != nil && cr.Status.Ownership.CreationNameHash != "") {
		base = kube.MergeFromWithOptions(cr, kube.MergeFromWithOptimisticLock{})
	}
	patch, err := base.Data(desired)
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
	fresh.DeepCopyInto(cr)
	return cr.Status.Ownership, true, patchErr
}

// sameRecord reports whether the stored record is the one that was written.
func sameRecord(stored, want *v1alpha1.ClientOwnership) bool {
	return stored != nil && stored.ClientID == want.ClientID && stored.Backend == want.Backend &&
		stored.SecretFingerprint == want.SecretFingerprint && stored.Pending == want.Pending &&
		stored.CreationNameHash == want.CreationNameHash
}

// clearOwnership removes the marker and returns nil only once the API server
// is known not to hold one.
func (e *external) clearOwnership(ctx context.Context, cr *v1alpha1.Client) error {
	stored, known, err := e.patchOwnership(ctx, cr, nil)
	switch {
	case !known:
		return fmt.Errorf("cannot clear status.ownership: %w", err)
	case stored != nil:
		return fmt.Errorf("cannot clear status.ownership (still %q): %w", stored.ClientID, err)
	}
	return nil
}

// writeOwnership requires confirmation that the complete record survived admission.
func (e *external) writeOwnership(ctx context.Context, cr *v1alpha1.Client, marker *v1alpha1.ClientOwnership) error {
	stored, known, err := e.patchOwnership(ctx, cr, marker)
	if sameRecord(stored, marker) {
		return nil
	}
	if !known {
		return fmt.Errorf("ownership write outcome is unknown: %w", err)
	}
	if err != nil {
		return fmt.Errorf("cannot record ownership: %w", err)
	}
	e.crd.Store(true)
	return errCRDLacksOwnership
}

// beginCreation persists proof before POST and reproduces it for retries.
// The API key keeps status readers from predicting the temporary name.
func (e *external) beginCreation(ctx context.Context, cr *v1alpha1.Client, id string) (string, error) {
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return "", err
	}
	if e.proofKey.Reveal() == "" {
		return "", errors.New("the API key is required to derive creation proof")
	}
	fresh.DeepCopyInto(cr)
	marker := cr.Status.Ownership
	if marker != nil && !marker.Pending {
		return "", notOwnedError(id)
	}
	if marker == nil {
		marker = &v1alpha1.ClientOwnership{
			ClientID:          id,
			Backend:           e.backend,
			CreatedAt:         metav1.NewTime(time.Now()),
			SecretFingerprint: "pending:" + rand.Text(),
			Pending:           true,
		}
	}
	mac := hmac.New(sha256.New, []byte(e.proofKey.Reveal()))
	fmt.Fprintf(
		mac,
		"rauthy-creation-name-v1\x00%s\x00%s\x00%s\x00%s",
		cr.UID,
		e.backend,
		id,
		marker.SecretFingerprint,
	)
	name := "crossplane-" + hex.EncodeToString(mac.Sum(nil))
	hash := creationNameHash(&name)
	if cr.Status.Ownership != nil {
		if marker.CreationNameHash != hash {
			return "", errors.New("cannot reproduce pending creation proof with the current API key")
		}
		return name, nil
	}
	marker.CreationNameHash = hash
	return name, e.writeOwnership(ctx, cr, marker)
}

func creationNameHash(name *string) string {
	if name == nil {
		return ""
	}
	sum := sha256.Sum256([]byte("rauthy-creation-name-v1\x00" + *name))
	return hex.EncodeToString(sum[:])
}

// finishCreation only binds a secret after confirming the random name from
// this create. Read failures retain the pending record for the next reconcile.
func (e *external) finishCreation(
	ctx context.Context,
	cr *v1alpha1.Client,
	got *rauthy.ClientResponse,
) (owner, *rauthy.Secret, error) {
	rec := cr.Status.Ownership
	if rec.CreationNameHash == "" || creationNameHash(got.Name) != rec.CreationNameHash ||
		got.Confidential != cr.Spec.ForProvider.Confidential {
		return notOwned, nil, e.clearOwnership(ctx, cr)
	}
	if taken, err := e.recordedElsewhereCached(ctx, cr, rec.ClientID); err != nil || taken {
		return notOwned, nil, err
	}
	var secret *rauthy.Secret
	want := rec.DeepCopy()
	want.SecretFingerprint = ""
	if got.Confidential {
		s, err := e.api.GetClientSecret(ctx, rec.ClientID)
		if err != nil {
			return notOwned, nil, fmt.Errorf("cannot read the secret of the new client: %w", err)
		}
		if s.Secret == nil {
			return notOwned, nil, errNoSecret
		}
		secret = s.Secret
		want.SecretFingerprint = secretFingerprint(rec.ClientID, *secret)
		// A replacement during the secret read must not be bound as ours.
		current, err := e.lookup(ctx, rec.ClientID)
		if err != nil {
			return notOwned, nil, err
		}
		if current == nil || creationNameHash(current.Name) != rec.CreationNameHash || !current.Confidential {
			return notOwned, nil, e.clearOwnership(ctx, cr)
		}
	}
	want.Pending = false
	if err := e.writeOwnership(ctx, cr, want); err != nil {
		return notOwned, nil, err
	}
	return ownedClient, secret, nil
}

// secretFingerprint is the binding between a record and a confidential
// client: a domain-separated SHA-256 of the client id and the secret. It
// cannot be reversed into the secret.
func secretFingerprint(id string, secret rauthy.Secret) string {
	sum := sha256.Sum256([]byte(fingerprintDomain + "\x00" + id + "\x00" + secret.Reveal()))
	return hex.EncodeToString(sum[:])
}

const fingerprintDomain = "rauthy-client-secret-v1"

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
			"the next reconcile will verify the pending creation proof: %w (%w)",
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
	if err = e.bindBackend(ctx, fresh); err != nil {
		return err
	}
	cr.Status.Ownership = fresh.Status.Ownership
	return checkRecordID(cr, meta.GetExternalName(cr))
}

// bindBackend refuses a record made through another Rauthy. A record written
// before records named their Rauthy has no backend URL: it was made through
// the only backend the resource has had, so it is bound to the current one
// once, by the same confirmed status write as any other record.
func (e *external) bindBackend(ctx context.Context, cr *v1alpha1.Client) error {
	rec := cr.Status.Ownership
	switch {
	case rec == nil || rec.Backend == e.backend:
		return nil
	case rec.Backend != "":
		return errBackendChanged
	}
	want := rec.DeepCopy()
	want.Backend = e.backend
	stored, _, err := e.patchOwnership(ctx, cr, want)
	if err != nil {
		return fmt.Errorf("cannot bind the ownership record to the Rauthy backend: %w", err)
	}
	if stored == nil || (stored.ClientID == rec.ClientID &&
		stored.SecretFingerprint == rec.SecretFingerprint && stored.Backend == "") {
		return errors.New("the installed Client CRD does not keep status.ownership.backend; " +
			"upgrade the provider's CRDs and restart the provider")
	}
	if !sameRecord(stored, want) {
		return errors.New("the ownership record changed while binding it to the Rauthy backend")
	}
	return nil
}
