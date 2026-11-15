package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"
	kube "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// Connection detail keys published for every Client.
const (
	ConnectionKeyClientID     = "client_id"
	ConnectionKeyClientSecret = "client_secret"
	ConnectionKeyIssuer       = "issuer"
)

// ErrAdoption is returned whenever a Client would have to take over a Rauthy
// client it did not create, or cannot prove it created.
var ErrAdoption = errors.New("adoption is not supported")

var (
	errEmptyID           = errors.New("the client id (the external name) is empty")
	errInvalidID         = errors.New("invalid client id")
	errNotOwned          = errors.New("refusing to modify a Rauthy client this resource did not create")
	errIDChanged         = errors.New("the client id is immutable")
	errCRDLacksOwnership = errors.New(
		"the installed Client CRD does not keep status.ownership; upgrade the provider's CRDs and restart " +
			"the provider",
	)
	errPoliciesNoUpdate = errors.New(
		"lack Update: the client can only be brought to the spec by an update, so Update (or *) is required",
	)
	errPoliciesNoCreate = errors.New(
		"lack Create: without it the resource could only observe a Rauthy client created elsewhere; " +
			"adoption is not supported",
	)
)

// api is the part of the Rauthy client the controller uses.
type api interface {
	CreateClient(ctx context.Context, req rauthy.NewClientRequest) (*rauthy.ClientResponse, error)
	GetClient(ctx context.Context, id string) (*rauthy.ClientResponse, error)
	UpdateClient(ctx context.Context, id string, req rauthy.UpdateClientRequest) (*rauthy.ClientResponse, error)
	DeleteClient(ctx context.Context, id string) error
	GetClientSecret(ctx context.Context, id string) (*rauthy.ClientSecretResponse, error)
}

// external reconciles one Client against Rauthy; see ownership.go for the
// ownership states it moves between.
//
// Create only issues the POST, which carries a handful of fields, and records
// ownership. Everything else reaches Rauthy through Update, which the
// reconciler calls on the next pass because the fresh client differs from the
// spec. A failing PUT is therefore retried as an ordinary update of a client
// the resource is known to own.
type external struct {
	api    api
	status kube.SubResourceWriter
	// reader reads the Client from the API server, bypassing the cache.
	reader kube.Reader
	issuer string
	// crd is set, for the life of the process, once the installed CRD is
	// found to prune status.ownership.
	crd *atomic.Bool
	// confirmed is the client id whose ownership an uncached read confirmed
	// during this reconcile.
	confirmed string
}

// NewExternal returns the external client for a Rauthy instance. status writes
// the Client status subresource, where ownership is recorded; reader must read
// Clients from the API server without a cache; issuer is published to
// consumers in the connection details.
func NewExternal(
	c *rauthy.Client,
	status kube.SubResourceWriter,
	reader kube.Reader,
	issuer string,
) managed.TypedExternalClient[*v1alpha1.Client] {
	return newExternal(c, status, reader, issuer, &atomic.Bool{})
}

func newExternal(c api, status kube.SubResourceWriter, reader kube.Reader, issuer string, crd *atomic.Bool) *external {
	return &external{api: c, status: status, reader: reader, issuer: issuer, crd: crd}
}

// Observe reads the client and decides whether this resource owns it. The
// cached object only decides cheap refusals; anything that acts as the owner
// (reporting the client as existing, reading its secret, and the Update and
// Delete that follow) needs ownership confirmed by an uncached read.
func (e *external) Observe(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalObservation, error) {
	id := meta.GetExternalName(cr)
	if owned := ownedClientID(cr); owned != "" && owned != id {
		return managed.ExternalObservation{}, fmt.Errorf(
			"%w: this resource created client %q but its external name is now %q; restore the external name",
			errIDChanged, owned, id,
		)
	}
	if release, err := e.releasable(ctx, cr); err != nil || release {
		return managed.ExternalObservation{ResourceExists: false}, err
	}
	if err := validateClientID(id); err != nil && !meta.WasDeleted(cr) {
		return managed.ExternalObservation{}, err
	}
	if !meta.WasDeleted(cr) {
		if err := checkPolicies(cr, ownedClientID(cr) != ""); err != nil {
			return managed.ExternalObservation{}, err
		}
	}

	got, err := e.lookup(ctx, id)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if got == nil {
		// S3 -> S0: a marker must not outlive its client. Clear it, and only
		// then let the runtime create again or finalize.
		if err = e.clearStaleMarker(ctx, cr); err != nil {
			return managed.ExternalObservation{}, err
		}
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	owned, err := e.confirmOwnership(ctx, cr, id)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if !owned {
		if meta.WasDeleted(cr) {
			// Nothing of ours exists: let the resource go without touching
			// Rauthy.
			return managed.ExternalObservation{ResourceExists: false}, nil
		}
		return managed.ExternalObservation{}, notOwnedError(id)
	}
	return e.observeOwned(ctx, cr, got)
}

// releasable reports whether a resource being deleted owns nothing, so it can
// go without touching Rauthy. The cache only gets a cheap no: a lagging cache
// must not leak an owned client, so its "nothing" is confirmed uncached.
func (e *external) releasable(ctx context.Context, cr *v1alpha1.Client) (bool, error) {
	if !meta.WasDeleted(cr) || ownedClientID(cr) != "" {
		return false, nil
	}
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return false, err
	}
	cr.Status.Ownership = fresh.Status.Ownership
	return fresh.Status.Ownership == nil, nil
}

// lookup returns the Rauthy client, or nil when there is none (or the id is
// one Rauthy could never hold).
func (e *external) lookup(ctx context.Context, id string) (*rauthy.ClientResponse, error) {
	if !rauthy.ClientIDPattern().MatchString(id) {
		return nil, nil //nolint:nilnil // An invalid id names no client: "none" is the answer.
	}
	got, err := e.api.GetClient(ctx, id)
	if rauthy.IsNotFound(err) {
		return nil, nil //nolint:nilnil // Not found is the "none" answer.
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get Rauthy client %q: %w", id, err)
	}
	return got, nil
}

// clearStaleMarker clears a marker the API server holds while no client
// exists. The cache may lag either way, so the decision comes from an
// uncached read.
func (e *external) clearStaleMarker(ctx context.Context, cr *v1alpha1.Client) error {
	fresh, err := e.readFresh(ctx, cr)
	if err != nil {
		return err
	}
	cr.Status.Ownership = fresh.Status.Ownership
	if fresh.Status.Ownership == nil {
		return nil
	}
	return e.clearOwnership(ctx, cr)
}

// observeOwned reports a client whose ownership is confirmed.
func (e *external) observeOwned(
	ctx context.Context,
	cr *v1alpha1.Client,
	got *rauthy.ClientResponse,
) (managed.ExternalObservation, error) {
	cr.Status.AtProvider = observation(got)
	if meta.WasDeleted(cr) {
		// Deleting an owned client: existence is all the reconciler needs.
		return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
	}

	details, err := e.connectionDetails(ctx, got)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	diff := differences(&cr.Spec.ForProvider, got)
	if len(diff) == 0 {
		cr.SetConditions(xpv2.Available())
	} else {
		cr.SetConditions(xpv2.Unavailable())
	}

	return managed.ExternalObservation{
		ResourceExists:    true,
		ResourceUpToDate:  len(diff) == 0,
		Diff:              diffMessage(diff),
		ConnectionDetails: details,
	}, nil
}

// Create is only called after Observe found no client and confirmed that the
// API server holds no marker (S0).
func (e *external) Create(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalCreation, error) {
	id := meta.GetExternalName(cr)
	if err := validateClientID(id); err != nil {
		return managed.ExternalCreation{}, err
	}
	if e.crd.Load() {
		return managed.ExternalCreation{}, errCRDLacksOwnership
	}
	cr.SetConditions(xpv2.Creating())

	if _, err := e.api.CreateClient(ctx, createRequest(id, &cr.Spec.ForProvider)); err != nil {
		return managed.ExternalCreation{}, e.createFailed(ctx, id, err)
	}
	if err := e.recordOwnership(ctx, cr, id); err != nil {
		return managed.ExternalCreation{}, err
	}
	return managed.ExternalCreation{}, nil
}

// createFailed explains a failed POST; the outcome is known only here. A
// client that exists after a refused (4xx) POST belongs to somebody else; one
// that exists after a transport error or a 5xx may be the product of this
// very request. Neither is ever claimed.
func (e *external) createFailed(ctx context.Context, id string, postErr error) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	if _, getErr := e.api.GetClient(ctx, id); getErr != nil {
		return fmt.Errorf("cannot create Rauthy client %q: %w", id, postErr)
	}
	if isClientError(postErr) {
		return refusedError(id, postErr)
	}
	return outcomeUnknownError(id, postErr)
}

func (e *external) Update(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalUpdate, error) {
	id := meta.GetExternalName(cr)
	owned, err := e.ensureOwned(ctx, cr, id)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}
	if !owned {
		return managed.ExternalUpdate{}, fmt.Errorf("client %q: %w", id, errNotOwned)
	}

	got, err := e.api.GetClient(ctx, id)
	if err != nil {
		return managed.ExternalUpdate{}, fmt.Errorf("cannot get Rauthy client %q: %w", id, err)
	}

	updated, err := e.api.UpdateClient(ctx, id, updateRequest(id, &cr.Spec.ForProvider, got))
	if err != nil {
		return managed.ExternalUpdate{}, fmt.Errorf("cannot update Rauthy client %q: %w", id, err)
	}

	cr.Status.AtProvider = observation(updated)
	if len(differences(&cr.Spec.ForProvider, updated)) == 0 {
		cr.SetConditions(xpv2.Available())
	}
	return managed.ExternalUpdate{}, nil
}

func (e *external) Delete(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalDelete, error) {
	id := meta.GetExternalName(cr)
	cr.SetConditions(xpv2.Deleting())

	owned, err := e.ensureOwned(ctx, cr, id)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	if !owned {
		// Never delete a client this resource did not create.
		return managed.ExternalDelete{}, nil
	}

	if err = e.api.DeleteClient(ctx, id); err != nil && !rauthy.IsNotFound(err) {
		return managed.ExternalDelete{}, fmt.Errorf("cannot delete Rauthy client %q: %w", id, err)
	}
	// S2 -> S3 -> S0 at once: the marker goes with its client.
	if err = e.clearOwnership(ctx, cr); err != nil {
		return managed.ExternalDelete{}, fmt.Errorf("deleted Rauthy client %q: %w", id, err)
	}
	return managed.ExternalDelete{}, nil
}

func (e *external) Disconnect(context.Context) error { return nil }

// connectionDetails returns what a consumer needs to use the client. Only a
// confidential client has a secret.
func (e *external) connectionDetails(
	ctx context.Context,
	got *rauthy.ClientResponse,
) (managed.ConnectionDetails, error) {
	details := managed.ConnectionDetails{
		ConnectionKeyClientID: []byte(got.ID),
		ConnectionKeyIssuer:   []byte(e.issuer),
	}
	if !got.Confidential {
		return details, nil
	}

	s, err := e.api.GetClientSecret(ctx, got.ID)
	if err != nil {
		return nil, fmt.Errorf("cannot get the secret of Rauthy client %q: %w", got.ID, err)
	}
	if s.Secret != nil {
		details[ConnectionKeyClientSecret] = []byte(*s.Secret)
	}
	return details, nil
}

func diffMessage(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return "fields differ from the spec: " + strings.Join(fields, ", ")
}

// isClientError reports whether err is a 4xx answer from Rauthy: the request
// was understood and refused, so nothing was created.
func isClientError(err error) bool {
	apiErr, ok := errors.AsType[*rauthy.APIError](err)
	return ok && apiErr.StatusCode >= http.StatusBadRequest && apiErr.StatusCode < http.StatusInternalServerError
}
