# crossplane-provider-rauthy

A Crossplane provider for [Rauthy](https://github.com/sebadob/rauthy), the
OIDC provider. It manages Rauthy OIDC clients as Kubernetes resources and
publishes each client's id, secret and issuer as a connection Secret in the
application's namespace.

The provider is written by hand on
[crossplane-runtime](https://github.com/crossplane/crossplane-runtime) v2 and
talks to Rauthy's admin API through
[rauthy-go](https://github.com/fluencelabs/rauthy-go). It targets Crossplane
v2: managed resources are namespaced, so a composite resource can compose a
`Client` straight into the application's namespace.

## Resources

| Kind                    | Scope      | Purpose                                                   |
| ----------------------- | ---------- | --------------------------------------------------------- |
| `Client`                | Namespaced | An OIDC client in Rauthy.                                 |
| `ClusterProviderConfig` | Cluster    | Where Rauthy is and which API key to use.                 |
| `ProviderConfigUsage`   | Namespaced | Bookkeeping: which `Client` uses which config (internal). |

All kinds are in the API group `rauthy.fluence.dev/v1alpha1`.

There is deliberately no namespaced `ProviderConfig`: the Rauthy API key lives
in one Secret in the provider's namespace, and application namespaces only
reference the `ClusterProviderConfig` by name. A `Client` whose
`providerConfigRef.kind` is anything other than `ClusterProviderConfig` is
rejected by the API server.

## The no-adoption rule

**The provider never takes over, modifies or deletes a Rauthy client it did not
create.** A `Client` manages exactly the Rauthy client it created itself:

- If a client with the same id already exists in Rauthy and the `Client`
  holds no ownership record for it, or another `Client` in any namespace
  records the id, the `Client` reports `a client with id "<id>" exists in
  Rauthy or is recorded by another Client, and this resource holds no
  ownership record for it, so it is not managed; if an interrupted create of
  this resource made it, delete it in Rauthy to retry, otherwise choose another
  id` and does nothing else: it does not create the client, does not mark the
  resource as existing, does not update the client and does not publish its
  secret. This one message is used whatever the reason, and it never names the
  other `Client`; the provider cannot tell a stranger's client from one its own
  interrupted create left behind, so check before deleting anything.
- Deleting a `Client` deletes only a Rauthy client it created. A `Client` that
  never created anything (for example one stuck on the error above) is deleted
  without touching Rauthy.
- Management policies must include `Update` (or `*`): a new client only gets
  most of its configuration through an update, so without it the client could
  never match the spec. Until the resource has created its client they must
  also include `Create`; without it the resource could only observe a client
  created elsewhere, which is an import. Other policy sets are refused with an
  error naming the missing action. Policies are not checked while a `Client`
  is being deleted.
- The client id is fixed once the client exists. Changing the external name of
  a `Client` that owns a Rauthy client is reported as an error rather than
  pointing the resource at a different client.

### What counts as ownership

The only evidence is `status.ownership` on the `Client`, as the API server
holds it: `clientID`, `createdAt` and, for a confidential client,
`secretFingerprint`. The provider writes it through the status subresource
immediately after Rauthy accepts the create. Annotations, including
Crossplane's `crossplane.io/external-create-*` marks, influence neither what
the provider does nor what it reports.

**The record is bound to the client's secret.** For a confidential client,
`secretFingerprint` is the hex SHA-256 of `rauthy-client-secret-v1`, the
client id and the secret Rauthy issued at creation, separated by NUL bytes.
The secret itself is never stored, logged or put in an event or condition.
A `Client` owns a Rauthy client when the record names its external name and,
for a confidential client, the fingerprint of the client's current secret
matches. A client with the right id but another secret is not the client the
record was made for.

- **Rotating a client's secret in the Rauthy UI orphans it from the provider,
  by design**: the provider can no longer tell it from a client somebody else
  created with the same id, so it stops managing it (state S2' below).
- **A public client has no secret.** Its binding is the id alone, so a public
  client deleted and recreated outside Kubernetes with the same id is still
  taken for the resource's own.
- **A record must be the only one for its id.** Rauthy client ids are global,
  while `Client` objects are namespaced. If another `Client` (another UID)
  records the same id, this one is refused. Every observation looks such a
  record up in the provider's cache (an index on `status.ownership.clientID`),
  which can miss a record written within the cache's lag. The create and the
  delete do not rely on that: right before the create call and right before
  the delete call the provider lists every `Client` in the cluster straight
  from the API server and refuses on a match. This holds for confidential clients too: two
  records of one id can only come from a record that outlived its client or
  from a forged one, and the provider cannot tell which of the two is right.
  A record left behind by a client deleted out of band therefore blocks other
  namespaces from creating that id until its own `Client` sees the client
  gone and clears it. Its next successful observation does that, but only if
  it gets one: a `Client` that is paused, whose provider config or API key is
  broken, or whose management policies are refused never observes, and its
  record keeps the id blocked indefinitely. The refusal message never names the
  holder; an operator finds it with `kubectl get clients -A` (the `RECORDED-ID`
  column shows `status.ownership.clientID`) or in the provider's log, which
  names both objects (`Refusing a client id another Client records`, with
  `recorded-by` set to the holder). To unblock the id, make the holder
  reconcile again (unpause it, fix its provider config or API key, fix its
  management policies): it then clears the stale record. Deleting the holder
  also clears it, since management policies are not checked while a `Client`
  is being deleted, except for a holder that is paused or whose provider
  config or API key is broken: its finalizer is removed by the provider, so
  such a holder stays until it can reconcile again. As a
  last resort, an operator allowed to write `clients/status` can remove
  `status.ownership` from the holder; that is safe only after checking that
  no client with that id exists in Rauthy.
- **`confidential` is immutable** (enforced by the CRD and by the
  controller): switching would change the binding. Delete and recreate the
  `Client` to switch.

Before it acts as the owner (reports the client as existing, reads or
publishes its secret, updates or deletes it), the provider re-reads the
`Client` straight from the API server, bypassing its cache, requires the
record there on the same object (same UID), checks that no other `Client`
records the id (in the cache, and uncached again before a delete), and checks
the fingerprint. The other-record check of an observation is answered from
the cache, so a record another `Client` wrote within the cache's lag can be
missed there, and everything that observation decides as the owner rests on
it: publishing the connection details (including a confidential client's
secret), reporting the client as existing and `Ready`, and the update that
follows. Create and Delete do not rest on it: right before the create call
and the delete call the check is repeated uncached, so a lagging cache never
makes the provider create or delete a client whose id another `Client`
records.

The provider keeps one invariant: **a record names a client id only while
this resource's client with that id exists in Rauthy.**

| State | Record | Rauthy client | What happens |
| ----- | ------ | ------------- | ------------ |
| S0    | none   | none          | The client is created. |
| S1    | none   | exists        | Refused: no ownership record (message above). |
| S2    | id     | ours          | Owned: observed, updated, deleted. |
| S2'   | id     | not ours      | Refused: the client with the id is not the one the record was made for (another secret, or public where the record has a fingerprint). Never published, updated or deleted. |
| S3    | id     | none          | Transient: the record is cleared, confirmed, before anything else. |
| S4    | id on another `Client` too | any | Refused with the message above: never created, published, updated or deleted. Deleting the `Client` finalizes it without touching Rauthy. |

- **Create** (S0): the POST, then the secret is read and the record (id and
  fingerprint) is written in one status patch. If the secret cannot be read,
  the new client is deleted again. If writing the record fails, the provider
  re-reads the `Client` without the cache: if the record landed anyway, the
  client is kept (S2); if it did not, the new client is deleted again (back to
  S0); if even the re-read fails, the client is neither deleted nor claimed,
  and the next reconcile decides from a fresh read (S2 or S1). Re-reads and
  the rollback run on their own bounded context, so an expired reconcile
  deadline does not stop them.
- **Delete** (S2): the DELETE, then the record is cleared at once.
- **A client that disappears** (S2 to S3, by Delete or out of band): the next
  observation clears the record and only then lets the resource create again
  or finish deleting.
- **S2'** reports `the client with id "<id>" in Rauthy is not the one this
  resource created (its secret differs: it was recreated or its secret was
  rotated outside this provider), so it is not managed`. The record is left
  as it is. To recover, delete that client in Rauthy (S3, then the record is
  cleared and the resource creates its own client again, with a new secret),
  or delete the `Client`, which finalizes without touching the Rauthy client.

What each interruption leaves: before the POST, S0; between the POST and the
record, S1 (refused until someone deletes the client in Rauthy); after the
record, S2; between a DELETE and clearing the record, S3, which the next
reconcile repairs.

Windows that remain:

- **Closed for confidential clients**: a client deleted in Rauthy and
  recreated by someone else with the same id while the record still exists
  (S3 looking like S2) now lands in S2', because the new client's secret
  differs.
- **Public clients across namespaces: closed.** A `Client` in another
  namespace can no longer create an id another `Client` records, and a
  `Client` whose record is shared with another never acts on the client, so a
  record that outlived its client cannot be used to update or delete another
  namespace's client.
- **Public clients recreated outside Kubernetes**: that window stays open;
  the binding is the id alone.
- **Anyone who can write `clients/status`** can write any record, including
  a fingerprint (see below). The single-record check limits what a forged
  record achieves while the real owner's record exists (both `Client`s are
  refused, which blocks the owner but does not hand over the client), but it
  does not stop a forged record for an id no other `Client` records, and once
  the real owner's `Client` is deleted the forged record is the only one left.
- **Anyone who knows a client's secret** can compute its fingerprint and, with
  `clients/status` write access, forge a record for it.

The API server ignores `status` when a `Client` is created or updated through
the main resource, so a manifest cannot carry ownership. What it does rely on:

- **Only the provider may write `clients/status`.** Anyone with write access to
  the `clients/status` subresource of `rauthy.fluence.dev` can forge ownership
  and so take over any Rauthy client, including its secret. Do not grant that
  permission to anyone but the provider's service account. Crossplane's RBAC
  manager can aggregate rules for provider resources into the built-in roles,
  so check who holds it, for example with `kubectl auth can-i patch
  clients.rauthy.fluence.dev --subresource=status -n NAMESPACE --as=USER`.
- **Tenants should not create `Client` objects directly** if a platform
  composes them. Ownership protects existing Rauthy clients, but a tenant who
  can create a `Client` can still create new Rauthy clients with any id that is
  free and any redirect URIs.
- **Status is not backed up reliably.** Restoring a `Client` from a backup or
  re-applying it from Git recreates it without `status.ownership`. The resource
  then fails safe: it refuses its own client. To recover, either delete the
  client in Rauthy, after which the resource creates it again (with a new
  secret), or delete the `Client` (which leaves the Rauthy client alone) and
  remove the client in Rauthy by hand.

### When a create goes wrong

The create call itself knows how it ended, and its error says so once:

- **Rauthy refuses the create** (a 4xx answer) and a client with that id
  exists: `client "<id>" already exists in Rauthy and was not created by this
  resource; choose another id`.
- **The create request fails ambiguously** (a timeout, a transport error or a
  5xx answer) and a client with that id exists afterwards: `create outcome
  unknown: the request failed but a client with id "<id>" now exists; verify
  and delete it in Rauthy to retry`.
- **Recording ownership fails**: see Create above. If the rollback DELETE
  fails, the error names the client to delete in Rauthy by hand.
- **The installed `Client` CRD is older** and silently drops
  `status.ownership`: the new client is deleted again and the provider stops
  creating clients, reporting that the CRD lacks the field, until it is
  restarted with current CRDs. This avoids creating and deleting a client on
  every reconcile.

From the next reconcile on, every one of these shows the single message from
the no-adoption rule above.

## Install

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: crossplane-provider-rauthy
spec:
  package: ghcr.io/fluencelabs/crossplane-provider-rauthy:v0.1.0
```

The package is pulled from GitHub Container Registry: either make the
`crossplane-provider-rauthy` package public in the organisation's package
settings, or give the `Provider` a pull secret with
`spec.packagePullSecrets`.

Create an API key in the Rauthy Admin UI (API Keys) with the `Clients` rights
(create, read, update, delete) and `Secrets: read`, store it in the provider's
namespace and point a `ClusterProviderConfig` at it:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: rauthy-api-key
  namespace: crossplane-system
stringData:
  api-key: REPLACE_WITH_NAME$SECRET   # exactly as Rauthy displays it
---
apiVersion: rauthy.fluence.dev/v1alpha1
kind: ClusterProviderConfig
metadata:
  name: default
spec:
  url: http://rauthy.rauthy.svc.cluster.local:8080
  issuerURL: https://auth.example.com/auth/v1
  credentials:
    source: Secret
    secretRef:
      namespace: crossplane-system
      name: rauthy-api-key
      key: api-key
```

The provider keeps no cache of Secrets: it reads the API key and writes the
connection Secrets with direct API calls, so it never lists or watches
Secrets. The permissions it uses on Secrets are `get`, `create` and `patch`;
it has no RBAC of its own beyond what Crossplane grants the provider's service
account.

`url` is where the provider reaches Rauthy; `/auth/v1` is appended
automatically. It must not carry a query or fragment, and the provider does not
follow redirects: a `url` that answers 3xx fails with a message naming where it
pointed. `issuerURL` is the issuer published to consumers; it defaults to
`url` followed by `/auth/v1`, which is only right when consumers reach Rauthy at
the same address as the provider.

## Examples

See [`examples/`](examples): a `ClusterProviderConfig` with its Secret, a
[confidential client](examples/client-confidential.yaml) and a
[public client](examples/client-public.yaml).

```yaml
apiVersion: rauthy.fluence.dev/v1alpha1
kind: Client
metadata:
  name: grafana
  namespace: monitoring
spec:
  providerConfigRef:
    kind: ClusterProviderConfig
    name: default
  writeConnectionSecretToRef:
    name: grafana-oidc
  forProvider:
    name: Grafana
    confidential: true
    redirectUris:
      - https://grafana.example.com/login/generic_oauth
```

The Rauthy client id is the external name (the `crossplane.io/external-name`
annotation), which defaults to `metadata.name`. It must match Rauthy's client id
pattern: 2 to 256 letters, digits, `.`, `_` or `-`.

### Connection details

| Key             | Present for              |
| --------------- | ------------------------ |
| `client_id`     | every client             |
| `issuer`        | every client             |
| `client_secret` | confidential clients only |

The secret is read from Rauthy on every observation, checked against the
ownership record, and never logged.

## Field reference

`spec.forProvider` mirrors the Rauthy client field by field, except for the
settings listed under "Not yet supported". Only `confidential` and
`redirectUris` are required.

| Field                    | Type     | Notes                                                                      |
| ------------------------ | -------- | -------------------------------------------------------------------------- |
| `name`                   | string   | Display name on the login page.                                            |
| `confidential`           | bool     | Required and immutable. A confidential client gets a secret generated by Rauthy. |
| `enabled`                | bool     | Whether the client may be used to log in.                                  |
| `redirectUris`           | []string | Required, at least one.                                                    |
| `postLogoutRedirectUris` | []string |                                                                            |
| `allowedOrigins`         | []string | Extra CORS origins, `scheme://host[:port]` without a path.                 |
| `flowsEnabled`           | []string | `authorization_code`, `client_credentials`, `urn:ietf:params:oauth:grant-type:device_code`, `password`, `refresh_token`. At least one; cannot be cleared. |
| `accessTokenAlg`         | string   | `RS256`, `RS384`, `RS512` or `EdDSA`.                                      |
| `idTokenAlg`             | string   | `RS256`, `RS384`, `RS512` or `EdDSA`.                                      |
| `authCodeLifetime`       | int      | Seconds, 10 to 300.                                                        |
| `accessTokenLifetime`    | int      | Seconds, 10 to 86400.                                                      |
| `scopes`                 | []string | Scopes the client may request.                                             |
| `defaultScopes`          | []string | Scopes granted without being requested.                                    |
| `challenges`             | []string | PKCE methods, `plain` and `S256`.                                          |
| `forceMfa`               | bool     | Require MFA for logins through this client.                                |
| `clientUri`              | string   | Informational URI of the application.                                      |
| `contacts`               | []string | Contacts of the application's maintainers.                                 |
| `backchannelLogoutUri`   | string   | OIDC back-channel logout endpoint.                                         |
| `restrictGroupPrefix`    | string   | Only users in groups with this prefix may log in.                          |

The CRD carries Rauthy's own validation rules (patterns, bounds and allowed
values), taken from rauthy-go; a unit test fails when the two drift apart.

**Unset fields keep what Rauthy has.** Rauthy's update is a full replacement
and requires a value for most fields. For every field the spec leaves out, the
provider sends back what Rauthy currently holds: the defaults Rauthy picked at
creation, or a value an earlier spec set. Removing a field from the spec
therefore does not revert it; to change a setting, set it explicitly. Lists are
compared as sets, so their order does not matter. `status.atProvider` shows
the client as Rauthy reports it.

**An explicit empty value clears.** To remove a setting, set the list to `[]`
or the string to `""`:

```yaml
  forProvider:
    allowedOrigins: []         # no extra CORS origins
    restrictGroupPrefix: ""    # no group restriction
```

The resource reports drift until Rauthy holds no value for the field. Rauthy
receives `null` for the fields its API marks nullable (`name`,
`postLogoutRedirectUris`, `allowedOrigins`, `challenges`, `clientUri`,
`contacts`, `backchannelLogoutUri`, `restrictGroupPrefix`) and `[]` for
`scopes` and `defaultScopes`, which it requires as lists. `flowsEnabled`
cannot be empty in Rauthy and keeps `minItems: 1`; `redirectUris` and
`confidential` are required. Should Rauthy keep a value of its own for a
cleared field, the resource stays out of sync and names the field.

A new client is created in two steps: the create call carries only the id,
name, confidentiality and redirect URIs (all Rauthy accepts there), and the
rest of the spec follows as an ordinary update on the next reconcile. If that
update fails, the resource still owns the client and keeps retrying it. The
`Ready` condition is true once the client exists, belongs to the resource and
matches the spec.

## Not yet supported

- SCIM configuration, allowed resources (RFC 8707), default audiences and
  custom claims (with `claims_at_root`). Whatever a client holds for them is
  kept unchanged on every update.
- Secret rotation.
- Adopting or importing an existing Rauthy client (by design, see the
  no-adoption rule).
- Removing `client_secret` from an existing connection Secret when a client
  switches from confidential to public: the stale key stays until the Secret is
  recreated.
- Any Rauthy resource other than OIDC clients.

## Development

Requires Go (the version in `go.mod`), Docker for images, and the
[crossplane CLI](https://docs.crossplane.io/latest/cli/) 2.x for packages.

| Target            | What it does                                                               |
| ----------------- | -------------------------------------------------------------------------- |
| `make generate`   | Regenerates deepcopy methods, the crossplane-runtime method sets and the CRDs under `package/crds`. |
| `make test`       | Unit tests with the race detector.                                          |
| `make lint`       | golangci-lint with `.golangci.yml`.                                         |
| `make build`      | The controller binary for the local platform under `_output/bin`.           |
| `make image`      | Controller images for `linux/amd64` and `linux/arm64`, saved as tarballs.   |
| `make xpkg.build` | One provider package per platform with its image embedded, under `_output/xpkg`. |

The unit tests run the external client against an in-memory fake of the Rauthy
API that behaves like the real server where it matters (the create call drops
everything but the create fields, updates replace the whole client). A second
set of tests drives crossplane-runtime's real managed reconciler against a fake
API server, to check that ownership recorded in status survives the runtime's
own updates of the object.

The upstream provider template's `build` submodule is not used; the plain
`Makefile` above covers what this provider needs.

## Releases

Releases are cut by [release-please](https://github.com/googleapis/release-please)
from conventional commits on `main`: it keeps a release pull request open, and
merging it tags `vX.Y.Z` and publishes the GitHub release. A published release
builds the provider package for `linux/amd64` and `linux/arm64` and pushes it
as one multi-platform package to
`ghcr.io/fluencelabs/crossplane-provider-rauthy:vX.Y.Z`.

## License

Apache License 2.0, see [LICENSE](LICENSE).
