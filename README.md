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

- If a client with the same id already exists in Rauthy, the `Client` reports
  `client "<id>" already exists in Rauthy and was not created by this resource;
  adoption is not supported` and does nothing else: it does not mark the
  resource as existing, does not update the client and does not publish its
  secret. This holds whether the existing client is found while observing or
  when the create request is refused.
- Deleting a `Client` deletes only a Rauthy client it created. A `Client` that
  never created anything (for example one stuck on the error above) is deleted
  without touching Rauthy.
- Management policies that cannot create, such as `["Observe"]`, would only
  ever observe a client created elsewhere, which is an import. They are refused
  with the same error until the resource has created its own client. A
  resource that did create its client may switch to such policies later.
- The client id is fixed once the client exists. Changing the external name of
  a `Client` that owns a Rauthy client is reported as an error rather than
  pointing the resource at a different client.

Ownership is recorded on the `Client` itself: the runtime's
`crossplane.io/external-create-succeeded` annotation, newer than any later
create attempt, together with `rauthy.fluence.dev/created-client-id`, which the
provider writes in the same update. Copying these annotations onto another
resource transfers ownership, so treat them as part of the resource.

Two situations need a human:

- **Interrupted create.** If the provider stops after Rauthy accepted the
  create but before the result was recorded, crossplane-runtime refuses to
  continue (`cannot determine creation result`). The provider cannot prove the
  client is its own, so it does not guess: check the client in Rauthy, delete
  it there if it is the one this resource made, and remove the
  `crossplane.io/external-create-pending` annotation.
- **A client deleted out of band and recreated by someone else with the same
  id.** Rauthy keeps no record of who created a client, so a resource that once
  created a client cannot tell it apart from a stranger's client with the same
  id that appeared later. Avoid reusing client ids across owners.

## Install

```yaml
apiVersion: pkg.crossplane.io/v1
kind: Provider
metadata:
  name: crossplane-provider-rauthy
spec:
  package: ghcr.io/fluencelabs/crossplane-provider-rauthy:v0.1.0
```

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

`url` is where the provider reaches Rauthy; `/auth/v1` is appended
automatically. `issuerURL` is the issuer published to consumers; it defaults to
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

The secret is read from Rauthy on every observation and never logged.

## Field reference

`spec.forProvider` mirrors the Rauthy client one to one. Only `confidential`
and `redirectUris` are required.

| Field                    | Type     | Notes                                                                      |
| ------------------------ | -------- | -------------------------------------------------------------------------- |
| `name`                   | string   | Display name on the login page.                                            |
| `confidential`           | bool     | Required. A confidential client gets a secret generated by Rauthy.          |
| `enabled`                | bool     | Whether the client may be used to log in.                                  |
| `redirectUris`           | []string | Required, at least one.                                                    |
| `postLogoutRedirectUris` | []string |                                                                            |
| `allowedOrigins`         | []string | Extra CORS origins, `scheme://host[:port]` without a path.                 |
| `flowsEnabled`           | []string | `authorization_code`, `client_credentials`, `urn:ietf:params:oauth:grant-type:device_code`, `password`, `refresh_token`. |
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
compared as sets, so their order does not matter, and an empty list is
rejected rather than silently ignored. `status.atProvider` shows the client as
Rauthy reports it.

A new client is created in two steps: the create call carries only the id,
name, confidentiality and redirect URIs (all Rauthy accepts there), and the
rest of the spec follows as an ordinary update on the next reconcile. If that
update fails, the resource still owns the client and keeps retrying it. The
`Ready` condition is true once the client exists, belongs to the resource and
matches the spec.

## Not yet supported

- SCIM configuration. An existing SCIM setting on a client is passed through
  unchanged on every update.
- Secret rotation.
- Clearing an optional field or list once set (removing it from the spec keeps
  the last value, see above).
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
everything but the create fields, updates replace the whole client).

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
