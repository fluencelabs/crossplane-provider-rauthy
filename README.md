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

## Known limits

- Rauthy has no conditional writes. A client can change during the single-request
  window between the provider's ownership check and its update or delete.
- For public clients recreated outside Kubernetes, see the policy under
  [What counts as ownership](#what-counts-as-ownership).
- A client update can persist before SCIM synchronization fails with HTTP 500.
  The resource can become `Ready` on the next observation if its managed fields
  match. `Ready` does not confirm SCIM completion. SCIM recovery depends on
  Rauthy's own subsequent synchronization, which the provider does not trigger.

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
- **A public client has no secret.** Its binding is the backend URL and id, so a public
  client deleted and recreated outside Kubernetes with the same id is still
  taken for the resource's own.
- **A finalized record must be the only one for its id in the same Rauthy instance.** Client ids are global within an instance,
  while `Client` objects are namespaced. If another `Client` (another UID)
  holds a finalized record for the same id, this one is refused. This holds for confidential clients too: two
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

Ownership records include a fingerprint of the Rauthy API root (`backend`): a hash, so no part of the URL appears in status. A changed
`providerConfigRef` or ProviderConfig URL cannot transfer ownership to another
Rauthy instance, even if it contains a public client with the same id. Restore
the original URL to resume reconciliation and deletion. Independent Rauthy
instances can each manage the same client id. A record written by an earlier
release has no `backend`; the provider binds it to the current
ProviderConfig URL on its first reconcile, so upgrade before changing the URL.

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

- **Create** (S0): before POST, persist a pending ownership record containing
  the hash of a temporary Rauthy client name. The API key, resource UID,
  backend, client id and persisted random nonce derive the name with HMAC-SHA256.
  Status readers cannot reproduce it. An absent observation retains pending
  proof, and each POST retry uses the same name, including after restart.
  Confirm the name, read the secret, then confirm the name again before
  recording the final secret fingerprint. A transient read or status failure
  leaves the pending proof for the next reconcile, including after restart.
  A different name is refused without publishing, updating or deleting the
  client. The first owned Update restores the requested name, including an
  unset name. A CRD that prunes the pending proof prevents POST entirely.
  Pending proofs do not reserve the id. Concurrent attempts are distinguished
  by the derived name in Rauthy. A losing attempt clears its stale proof on
  the next reconcile, and cannot block the successful creator.
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
- **A public client keeps `S256` among its PKCE methods.** Rauthy accepts
  `challenges: []` for a public client and afterwards stores no challenge
  methods for it. The provider refuses that: a public `Client` whose
  `challenges` is set must include `S256` (the API server refuses it at
  admission, and the controller refuses an object stored before that rule).
  Leaving `challenges` unset keeps what Rauthy holds. If those methods lack
  `S256`, reconciliation fails until `challenges` explicitly includes `S256`.
  Whether Rauthy itself enforces PKCE at login for a public client
  without challenge methods has not been verified.
- **Status is not backed up reliably.** Restoring a `Client` from a backup or
  re-applying it from Git recreates it without `status.ownership`. The resource
  then fails safe: it refuses its own client. To recover, either delete the
  client in Rauthy, after which the resource creates it again (with a new
  secret), or delete the `Client` (which leaves the Rauthy client alone) and
  remove the client in Rauthy by hand.

### When a create goes wrong

The create call itself knows how it ended, and its error says so once:

- **Rauthy refuses the create** (a 4xx answer) and a client with that id
  exists: a matching pending proof finishes ownership, including a late commit
  of an earlier POST. A different name is refused: `client "<id>" already exists
  in Rauthy and was not created by this resource; choose another id`.
- **The create request fails ambiguously** (a timeout, a transport error or a
  5xx answer) and a client with that id exists afterwards: `create outcome
  unknown: the request failed but a client with id "<id>" now exists; the next
  reconcile will verify the pending creation proof`.
- **Recording ownership fails**: before POST, no client is created. After
  POST, the pending proof is retained and finalization retries on reconcile.
- **The installed `Client` CRD is older** and silently drops
  `status.ownership` or its pending proof: no client is created. The provider
  stops creating clients until it is restarted with current CRDs.

On subsequent reconciles, a matching pending creation resumes. An unrelated
client is refused without mutation. If the API key changes while a create is
pending and no client exists yet, restore the original key to retry that proof.
An existing client can still be verified by its stored name hash.

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
  issuerURL: https://auth.example.com/auth/v1/
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
`url` with the `/auth/v1/` path, including the trailing slash reported by
discovery. This is only right when consumers reach Rauthy at
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
| `scopes`                 | []string | Scopes the client may request. At least one; cannot be cleared.            |
| `defaultScopes`          | []string | Scopes granted without being requested. At least one; cannot be cleared.   |
| `challenges`             | []string | PKCE methods, `plain` and `S256`. For a public client a set list must include `S256`: `[]` and `[plain]` are refused. |
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
`contacts`, `backchannelLogoutUri`, `restrictGroupPrefix`). `redirectUris` and
`confidential` are required. Should Rauthy keep a value of its own for a
cleared field, the resource stays out of sync and names the field.

**`scopes`, `defaultScopes` and `flowsEnabled` cannot be empty.** Each keeps
`minItems: 1`, so the API server refuses `[]`. Rauthy answers an update with an
empty scope list with success but stores its default scope (`openid`) instead,
so a spec asking for `[]` would never match and would send an update on every
reconcile; it refuses an empty flow list outright. Leave them out to keep what
Rauthy holds.

A new client is created in two steps: the create call carries only the id,
name, confidentiality and redirect URIs (all Rauthy accepts there), and the
rest of the spec follows as an ordinary update on the next reconcile. If that
update fails, the resource still owns the client and keeps retrying it. The
`Ready` condition is true once the client exists, belongs to the resource and
matches the spec.

## Upgrading

From the release after v0.1.0, a spec with `scopes: []` or `defaultScopes: []`
is refused at admission. Such a value never took effect: Rauthy replaced it
with its default scope, and the resource kept reporting drift. Remove the field
to keep what Rauthy holds, or list the scopes explicitly.

From the release after v0.1.1, a public `Client` (`confidential: false`) whose
`challenges` is set without `S256` (for example `[]` or `[plain]`) is refused at
admission. To fix an object stored before the upgrade, explicitly include
`S256` in `challenges` (for example `challenges: ["S256"]`), in the same edit
as any other change to its `forProvider`: the rule sits on `forProvider`, so
the API server refuses every edit of `forProvider` with that message until
`challenges` is fixed. An omitted or null `challenges` field also fails
reconciliation if the observed Rauthy methods lack `S256`. Until then the
object is not rewritten: the provider
reports `a public client must keep S256 among its PKCE challenges` on its
`Synced` condition and sends nothing of its spec to Rauthy, so the Rauthy
client keeps its current settings. Status and metadata writes and deletion
still pass, through CRD validation ratcheting (on by default from Kubernetes
1.30), and deleting such a `Client` still deletes its Rauthy client; on an
older cluster fix the spec before upgrading.

## Rauthy versions

The wire types (through rauthy-go) mirror Rauthy 0.36.2. The API key needs the
`Clients` and `Secrets` access groups.

The following was observed by running the provider's requests against
throwaway containers of Rauthy 0.35.2 and 0.36.2, with the same results on
both unless noted:

- The managed `Client` fields behave the same on both versions.
- An update with an empty `scopes` or `default_scopes` list is accepted and
  the default scope (`openid`) is stored instead; `null` for either is refused.
- An update of a public client without challenge methods is accepted and the
  client afterwards holds none.
- The four settings the provider preserves but does not manage
  (`allowed_resources`, `default_aud`, `claims`, `claims_at_root`) are stored
  by 0.36.2 only: 0.35.2 accepts them in an update, drops them and never
  reports them.

One deployment of the provider on a real cluster, against Rauthy 0.35.2, was
since observed to: reconcile a claim to `Ready`; create the client once and
update it once, with no repeated updates; store the ownership record with a
secret fingerprint; publish the connection secret; and keep the client `Ready`
after that Rauthy was upgraded to 0.36.2. This is an observation from that
deployment only; this repository's tests do not reproduce it (the envtest
suite also uses the in-memory fake Rauthy).

The provider's own tests do not run against a live Rauthy: they use an
in-memory fake that reproduces these observations.

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
| `make test.envtest` | The controller against a real kube-apiserver and etcd (see below).        |
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

`make test.envtest` runs the controller against a real kube-apiserver and etcd
started by controller-runtime's envtest (binaries fetched by `setup-envtest`
for the Kubernetes version pinned in the `Makefile`), with the CRDs from
`package/crds` installed and the same fake Rauthy. It checks what a fake API
server cannot: pruning of `null` and survival of `[]` in the optional lists,
the CRD's admission rules, ownership recorded through the status subresource
(and found pruned under a CRD that lacks it), the field index on
`status.ownership.clientID` in a real cache, resourceVersion conflicts after
the status patch, and the controller as `Setup` builds it, from create to
delete. These tests are built only with the `envtest` tag, so `make test`
does not need the binaries.

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
