package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
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
	errEmptyID        = errors.New("the client id (the external name) is empty")
	errInvalidID      = errors.New("invalid client id")
	errNotOwned       = errors.New("refusing to modify a Rauthy client this resource did not create")
	errBackendChanged = errors.New(
		"the Rauthy backend cannot change after creation; restore the original ProviderConfig URL",
	)
	errIDChanged             = errors.New("the client id is immutable")
	errNoSecret              = errors.New("no secret was reported for the new confidential client")
	errConfidentialImmutable = errors.New(
		"confidential cannot change after creation: the ownership record is bound to the client's secret; " +
			"delete and recreate the Client to switch",
	)
	errCRDLacksOwnership = errors.New(
		"the installed Client CRD does not keep status.ownership; upgrade the provider's CRDs and restart " +
			"the provider",
	)
	errPublicWithoutS256 = errors.New(
		"a public client must keep S256 among its PKCE challenges: set challenges explicitly to include S256; " +
			"nothing of this spec is sent to Rauthy until it is fixed",
	)
	errURIComma = errors.New(
		"redirectUris and postLogoutRedirectUris must not contain a comma: Rauthy stores them comma-separated " +
			"and would split the URI; encode it as %2C",
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
	BaseURL() rauthy.Secret
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
	// log is the provider's own log, never shown on the resource.
	log logging.Logger
	// cache looks Clients up by ClientIDIndex; nil sends those lookups to
	// reader.
	cache   kube.Reader
	issuer  string
	backend string
	// crd is set, for the life of the process, once the installed CRD is
	// found to prune status.ownership.
	crd *atomic.Bool
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
	backend, err := backendIdentity(c.BaseURL().Reveal())
	if err != nil {
		panic(err) // rauthy.New accepted this URL, so it parses.
	}
	return newExternal(c, status, reader, nil, issuer, backend, &atomic.Bool{})
}

// backendIdentity names the Rauthy instance in an ownership record by a hash
// of its scheme, host and API root path. Userinfo, query and fragment are
// dropped, and the path is hashed, so nothing of the ProviderConfig URL
// reaches Client status, which tenants can read.
func backendIdentity(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("cannot parse the Rauthy URL: %w", err)
	}
	id := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String()
	sum := sha256.Sum256([]byte("rauthy-backend-v1\x00" + id))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func newExternal(
	c api,
	status kube.SubResourceWriter,
	reader, cache kube.Reader,
	issuer, backend string,
	crd *atomic.Bool,
) *external {
	return &external{
		api: c, status: status, reader: reader, cache: cache, issuer: issuer, backend: backend, crd: crd,
		log: logging.NewNopLogger(),
	}
}

// Observe reads the client and decides whether this resource owns it. A
// resource that cannot be observed, or that the controller refuses to manage,
// is not Ready: an earlier Ready must not outlive the state that justified it.
func (e *external) Observe(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalObservation, error) {
	o, err := e.observe(ctx, cr)
	if err != nil && !meta.WasDeleted(cr) {
		cr.SetConditions(xpv2.Unavailable())
	}
	return o, err
}

// observe does the work of Observe. The cached object only decides cheap
// refusals; anything that acts as the owner (reporting the client as existing
// and reading its secret) needs ownership confirmed by an uncached read.
// Update and Delete confirm it again themselves, right before they write.
func (e *external) observe(ctx context.Context, cr *v1alpha1.Client) (managed.ExternalObservation, error) {
	if err := e.checkBackend(ctx, cr); err != nil {
		return managed.ExternalObservation{}, err
	}
	id := meta.GetExternalName(cr)
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
		if err := checkSpec(&cr.Spec.ForProvider); err != nil {
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

	o, secret, err := e.verifyOwner(ctx, cr, id, got)
	if err != nil {
		return managed.ExternalObservation{}, err
	}
	if o != ownedClient {
		if meta.WasDeleted(cr) {
			// Nothing of ours exists: let the resource go without touching
			// Rauthy.
			return managed.ExternalObservation{ResourceExists: false}, nil
		}
		if o == otherClient {
			return managed.ExternalObservation{}, otherClientError(id)
		}
		return managed.ExternalObservation{}, notOwnedError(id)
	}
	return e.observeOwned(cr, got, secret)
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
	if err = checkRecordID(fresh, meta.GetExternalName(cr)); err != nil {
		return false, err
	}
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
	// Only a record for this very id may be cleared: one naming another id
	// belongs to a client that still exists under that id.
	if err = checkRecordID(fresh, meta.GetExternalName(cr)); err != nil {
		return err
	}
	return e.clearOwnership(ctx, cr)
}

// observeOwned reports a client whose ownership is confirmed.
func (e *external) observeOwned(
	cr *v1alpha1.Client,
	got *rauthy.ClientResponse,
	secret *rauthy.Secret,
) (managed.ExternalObservation, error) {
	cr.Status.AtProvider = observation(got)
	if meta.WasDeleted(cr) {
		// Deleting an owned client: existence is all the reconciler needs.
		return managed.ExternalObservation{ResourceExists: true, ResourceUpToDate: true}, nil
	}
	if err := checkConfidential(cr, got); err != nil {
		return managed.ExternalObservation{}, err
	}
	if err := checkPKCE(&cr.Spec.ForProvider, got); err != nil {
		return managed.ExternalObservation{}, err
	}

	details := e.connectionDetails(got, secret)

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
	if err := e.checkBackend(ctx, cr); err != nil {
		return managed.ExternalCreation{}, err
	}
	id := meta.GetExternalName(cr)
	if err := validateClientID(id); err != nil {
		return managed.ExternalCreation{}, err
	}
	if e.crd.Load() {
		return managed.ExternalCreation{}, errCRDLacksOwnership
	}
	if taken, err := e.recordedElsewhere(ctx, cr, id); err != nil || taken {
		if err == nil {
			err = notOwnedError(id)
		}
		return managed.ExternalCreation{}, err
	}
	cr.SetConditions(xpv2.Creating())

	if _, err := e.api.CreateClient(ctx, createRequest(id, &cr.Spec.ForProvider)); err != nil {
		return managed.ExternalCreation{}, e.createFailed(ctx, id, err)
	}
	fingerprint, err := e.bindingFor(ctx, cr, id)
	if err != nil {
		cr.Status.Ownership = nil
		return managed.ExternalCreation{}, fmt.Errorf(
			"created Rauthy client %q but cannot bind ownership; left unclaimed: %w",
			id,
			err,
		)
	}
	if err = e.recordOwnership(ctx, cr, id, fingerprint); err != nil {
		return managed.ExternalCreation{}, err
	}
	return managed.ExternalCreation{}, nil
}

// bindingFor reads the secret Rauthy issued for the client just created and
// returns its fingerprint; "" for a public client.
func (e *external) bindingFor(ctx context.Context, cr *v1alpha1.Client, id string) (string, error) {
	if !cr.Spec.ForProvider.Confidential {
		return "", nil
	}
	s, err := e.api.GetClientSecret(ctx, id)
	if err != nil {
		return "", fmt.Errorf("cannot read the secret of the new client: %w", err)
	}
	if s.Secret == nil {
		return "", errNoSecret
	}
	return secretFingerprint(id, *s.Secret), nil
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
	o, got, err := e.ensureOwned(ctx, cr, id)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}
	if o != ownedClient {
		return managed.ExternalUpdate{}, fmt.Errorf("client %q: %w", id, errNotOwned)
	}
	if err = checkConfidential(cr, got); err != nil {
		return managed.ExternalUpdate{}, err
	}
	if err = checkPKCE(&cr.Spec.ForProvider, got); err != nil {
		return managed.ExternalUpdate{}, err
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

	// Other records are checked uncached first; the client itself is
	// confirmed last, right before the one call that cannot be undone.
	taken, err := e.recordedElsewhere(ctx, cr, id)
	if err != nil || taken {
		return managed.ExternalDelete{}, err
	}
	o, _, err := e.ensureOwned(ctx, cr, id)
	if err != nil {
		return managed.ExternalDelete{}, err
	}
	if o != ownedClient {
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
// confidential client has a secret; it was read when ownership was confirmed.
func (e *external) connectionDetails(got *rauthy.ClientResponse, secret *rauthy.Secret) managed.ConnectionDetails {
	details := managed.ConnectionDetails{
		ConnectionKeyClientID: []byte(got.ID),
		ConnectionKeyIssuer:   []byte(e.issuer),
	}
	if got.Confidential && secret != nil {
		details[ConnectionKeyClientSecret] = []byte(secret.Reveal())
	}
	return details
}

// checkConfidential refuses a spec whose confidentiality differs from the
// client's: the ownership record is bound to the secret, so confidentiality
// cannot change after creation. The CRD refuses the change too; this covers
// objects written past it.
func checkConfidential(cr *v1alpha1.Client, got *rauthy.ClientResponse) error {
	if cr.Spec.ForProvider.Confidential != got.Confidential {
		return errConfidentialImmutable
	}
	return nil
}

// checkPKCE refuses public clients without S256. Unset methods preserve the
// observed methods; before creation there are no observed methods to check.
func checkPKCE(p *v1alpha1.ClientParameters, got *rauthy.ClientResponse) error {
	if p.Confidential {
		return nil
	}
	methods := p.Challenges
	if methods == nil {
		if got == nil {
			return nil
		}
		methods = got.Challenges
	}
	if slices.Contains(methods, pkceS256) {
		return nil
	}
	return errPublicWithoutS256
}

const pkceS256 = "S256"

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

// checkRecordID refuses a record that names an id other than the external
// name: the resource created that client, which still exists under its own
// id, so the record must be neither cleared nor treated as "nothing owned".
func checkRecordID(cr *v1alpha1.Client, id string) error {
	if owned := ownedClientID(cr); owned != "" && owned != id {
		return fmt.Errorf(
			"%w: this resource created client %q but its external name is now %q; restore the external name",
			errIDChanged, owned, id,
		)
	}
	return nil
}

// checkSpec refuses a spec that must not reach Rauthy, before anything is
// observed or sent.
func checkSpec(p *v1alpha1.ClientParameters) error {
	if err := checkPKCE(p, nil); err != nil {
		return err
	}
	return checkRedirectURIs(p)
}

// checkRedirectURIs refuses a redirect URI with a comma before anything is
// sent: Rauthy stores these lists comma-separated, so such a URI would come
// back as two and the callback would never match. The CRD cannot refuse it
// itself: a CEL rule over an unbounded list exceeds the API server's cost
// budget.
func checkRedirectURIs(p *v1alpha1.ClientParameters) error {
	for _, uri := range slices.Concat(p.RedirectURIs, p.PostLogoutRedirectURIs) {
		if strings.Contains(uri, ",") {
			return errURIComma
		}
	}
	return nil
}
