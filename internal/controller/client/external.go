package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

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
	errRefused           = errors.New("the create was refused")
	errNotOwned          = errors.New("refusing to modify a Rauthy client this resource did not create")
	errIDChanged         = errors.New("the client id is immutable")
	errCRDLacksOwnership = errors.New(
		"the installed Client CRD does not keep status.ownership; upgrade the provider's CRDs",
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

// external reconciles one Client against Rauthy.
//
// Create only issues the POST, which carries a handful of fields, and records
// ownership in status.ownership. Everything else reaches Rauthy through
// Update, which the reconciler calls on the next pass because the fresh client
// differs from the spec. A failing PUT is therefore retried as an ordinary
// update of a client the resource is known to own.
type external struct {
	api    api
	status kube.SubResourceWriter
	// reader reads the Client from the API server, bypassing the cache.
	reader kube.Reader
	issuer string
	// confirmed is the client id whose ownership an uncached read confirmed
	// during this reconcile.
	confirmed string
}

// NewExternal returns the external client for a Rauthy instance. status writes
// the Client status subresource, where ownership is recorded; reader must read
// Clients from the API server without a cache, to confirm ownership before
// acting on it; issuer is published to consumers in the connection details.
func NewExternal(
	c *rauthy.Client,
	status kube.SubResourceWriter,
	reader kube.Reader,
	issuer string,
) managed.TypedExternalClient[*v1alpha1.Client] {
	return &external{api: c, status: status, reader: reader, issuer: issuer}
}

// Observe reads the client and decides whether this resource owns it. The
// cached object only decides cheap refusals; anything that would act as the
// owner (reporting the client as existing, reading its secret, and the Update
// and Delete that follow) needs ownership confirmed by an uncached read.
func (e *external) Observe(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalObservation, error) {
	id := meta.GetExternalName(cr)
	if owned := ownedClientID(cr); owned != "" && owned != id {
		return managed.ExternalObservation{}, fmt.Errorf(
			"%w: this resource created client %q but its external name is now %q; restore the external name",
			errIDChanged, owned, id,
		)
	}
	if meta.WasDeleted(cr) && ownedClientID(cr) == "" {
		// The cache says nothing of ours exists. Confirm before letting the
		// resource go: a lagging cache must not leak an owned client.
		if _, ok, err := e.confirmOwnership(ctx, cr, id); err != nil || !ok {
			return managed.ExternalObservation{ResourceExists: false}, err
		}
	}
	if err := validateClientID(id); err != nil {
		return managed.ExternalObservation{}, err
	}
	if !meta.WasDeleted(cr) {
		if err := checkPolicies(cr, ownedClientID(cr) != ""); err != nil {
			return managed.ExternalObservation{}, err
		}
	}

	got, err := e.api.GetClient(ctx, id)
	if rauthy.IsNotFound(err) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if err != nil {
		return managed.ExternalObservation{}, fmt.Errorf("cannot get Rauthy client %q: %w", id, err)
	}

	fresh, ok, err := e.confirmOwnership(ctx, cr, id)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if !ok {
		if meta.WasDeleted(cr) {
			return managed.ExternalObservation{ResourceExists: false}, nil
		}
		return managed.ExternalObservation{}, notOwnedError(fresh, id)
	}
	return e.observeOwned(ctx, cr, got)
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

func (e *external) Create(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalCreation, error) {
	id := meta.GetExternalName(cr)
	if err := validateClientID(id); err != nil {
		return managed.ExternalCreation{}, err
	}
	cr.SetConditions(xpv2.Creating())

	// A record of an earlier client (one that has since vanished from Rauthy)
	// must be gone before the POST: should the id now belong to somebody
	// else, a stale record would hand their client to this resource.
	if cr.Status.Ownership != nil {
		if err := setOwnership(ctx, e.status, cr, ""); err != nil {
			return managed.ExternalCreation{}, fmt.Errorf("not creating client %q: %w", id, err)
		}
	}

	if _, err := e.api.CreateClient(ctx, createRequest(id, &cr.Spec.ForProvider)); err != nil {
		err = e.createFailed(ctx, id, err)
		// Persisted by the runtime with its create-failed mark; it only
		// chooses the wording of later errors.
		if errors.Is(err, errRefused) {
			meta.AddAnnotations(cr, map[string]string{AnnotationKeyCreateRefused: id})
		} else {
			meta.RemoveAnnotations(cr, AnnotationKeyCreateRefused)
		}
		return managed.ExternalCreation{}, err
	}

	if err := setOwnership(ctx, e.status, cr, id); err != nil {
		cr.Status.Ownership = nil
		meta.RemoveAnnotations(cr, AnnotationKeyCreateRefused)
		return managed.ExternalCreation{}, e.unrecorded(ctx, id, err)
	}
	meta.RemoveAnnotations(cr, AnnotationKeyCreateRefused)
	return managed.ExternalCreation{}, nil
}

// createFailed explains a failed POST. Rauthy's answer to a duplicate id is
// not a dedicated status code, so the client is looked up: a client that
// exists after a refused (4xx) POST belongs to somebody else; one that exists
// after a transport error or a 5xx may be the product of this very request.
// Neither is ever claimed.
func (e *external) createFailed(ctx context.Context, id string, postErr error) error {
	if _, getErr := e.api.GetClient(ctx, id); getErr != nil {
		return fmt.Errorf("cannot create Rauthy client %q: %w", id, postErr)
	}
	if isClientError(postErr) {
		return fmt.Errorf("%w (%w: %w)", adoptionError(id), errRefused, postErr)
	}
	return fmt.Errorf("%w (%w)", outcomeUnknownError(id), postErr)
}

// unrecorded handles a client that was created but whose ownership could not
// be recorded. Without the record the resource would refuse the client as
// foreign forever, so the client is deleted again and the next reconcile
// creates it afresh. If even that fails, a human has to delete it.
func (e *external) unrecorded(ctx context.Context, id string, recordErr error) error {
	if err := e.api.DeleteClient(ctx, id); err != nil && !rauthy.IsNotFound(err) {
		return fmt.Errorf(
			"created Rauthy client %q but could not record its ownership (%w), and could not delete it again "+
				"(%w): the resource will refuse it; delete client %q in Rauthy to retry",
			id, recordErr, err, id,
		)
	}
	return fmt.Errorf(
		"created Rauthy client %q but could not record its ownership, so it was deleted again and "+
			"will be recreated: %w",
		id, recordErr,
	)
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
