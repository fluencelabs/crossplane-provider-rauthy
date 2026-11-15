package client

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/fluencelabs/rauthy-go"

	"github.com/fluencelabs/crossplane-provider-rauthy/apis/v1alpha1"
)

// Connection detail keys published for every Client.
const (
	ConnectionKeyClientID     = "client_id"
	ConnectionKeyClientSecret = "client_secret"
	ConnectionKeyIssuer       = "issuer"
)

// ErrAdoption is returned whenever a Client would have to take over a Rauthy
// client it did not create.
var ErrAdoption = errors.New("adoption is not supported")

var (
	errEmptyID   = errors.New("the client id (the external name) is empty")
	errInvalidID = errors.New("invalid client id")
	errNotOwned  = errors.New("refusing to modify a Rauthy client this resource did not create")
	errIDChanged = errors.New("the client id is immutable")
	errPolicies  = errors.New(
		"management policies without Create would only observe a Rauthy client created elsewhere; " +
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
// Create only issues the POST, which carries a handful of fields. Everything
// else reaches Rauthy through Update, which the reconciler calls on the next
// pass because the fresh client differs from the spec. A failure after the
// POST therefore never happens inside Create: Create succeeds as soon as the
// client exists, the runtime records the success, and a failing PUT is retried
// as an ordinary update of a client the resource is known to own.
type external struct {
	api    api
	issuer string
}

// NewExternal returns the external client for a Rauthy instance. issuer is
// published to consumers in the connection details.
func NewExternal(c *rauthy.Client, issuer string) managed.TypedExternalClient[*v1alpha1.Client] {
	return &external{api: c, issuer: issuer}
}

func (e *external) Observe(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalObservation, error) {
	id := meta.GetExternalName(cr)
	owned := createdByThis(cr)

	if owned && createdClientID(cr) != id {
		return managed.ExternalObservation{}, fmt.Errorf(
			"%w: this resource created client %q but its external name is now %q; restore the external name",
			errIDChanged, createdClientID(cr), id,
		)
	}
	if !owned && meta.WasDeleted(cr) {
		// Nothing of ours exists, so there is nothing to delete: let the
		// resource go without touching Rauthy.
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if err := validateClientID(id); err != nil {
		return managed.ExternalObservation{}, err
	}
	if !owned && !policiesAllowCreate(cr) {
		return managed.ExternalObservation{}, errPolicies
	}

	got, err := e.api.GetClient(ctx, id)
	if rauthy.IsNotFound(err) {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	if err != nil {
		return managed.ExternalObservation{}, fmt.Errorf("cannot get Rauthy client %q: %w", id, err)
	}
	if !owned {
		return managed.ExternalObservation{}, adoptionError(id)
	}

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

	// Drop any record of an earlier client: this attempt either creates one
	// and records it below, or fails and leaves the resource owning nothing.
	meta.RemoveAnnotations(cr, AnnotationKeyCreatedClientID)
	cr.SetConditions(xpv2.Creating())

	if _, err := e.api.CreateClient(ctx, createRequest(id, &cr.Spec.ForProvider)); err != nil {
		// Rauthy's answer to a duplicate id is not a dedicated status code, so
		// ask directly: if the client exists after a failed POST, somebody
		// else owns it.
		if _, getErr := e.api.GetClient(ctx, id); getErr == nil {
			return managed.ExternalCreation{}, fmt.Errorf("%w (POST /clients: %w)", adoptionError(id), err)
		}
		return managed.ExternalCreation{}, fmt.Errorf("cannot create Rauthy client %q: %w", id, err)
	}

	meta.AddAnnotations(cr, map[string]string{AnnotationKeyCreatedClientID: id})
	return managed.ExternalCreation{}, nil
}

func (e *external) Update(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalUpdate, error) {
	id := meta.GetExternalName(cr)
	if !createdByThis(cr) || createdClientID(cr) != id {
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

	if !createdByThis(cr) || createdClientID(cr) != id {
		// Never delete a client this resource did not create.
		return managed.ExternalDelete{}, nil
	}

	if err := e.api.DeleteClient(ctx, id); err != nil && !rauthy.IsNotFound(err) {
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
