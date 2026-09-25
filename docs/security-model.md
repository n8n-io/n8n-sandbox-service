# Security Model

What the sandbox service isolates, where each boundary is enforced, and what it
explicitly does not promise. Aimed at anyone reviewing the service or deciding
what it is safe to run on it.

## Trust boundaries

Four boundaries carry the guarantees. They are enforced by different components
and fail in different ways.

| Boundary | Enforced by | Protects against |
| --- | --- | --- |
| Client → API | API key auth and per-request tenant checks | One tenant reaching another tenant's sandboxes |
| API → Runner | mTLS on both listeners, plus a shared runner API key | Unauthorized control of sandbox lifecycles |
| Guest → host and network | Network namespaces, iptables, and the container or microVM boundary | Untrusted code escaping its sandbox or reaching other sandboxes |
| Build inputs → images and guest assets | Checksums on fetched binaries, digest-pinned releases, an optional manifest check | A substituted build input reaching every sandbox, for the inputs that are verified |

Everything trusts the layer above it. Sandboxes trust nothing.

## Client to API

The API is the only component that knows about tenants. Every other component
trusts it to have made the decision correctly.

Requests carry an `X-Api-Key` header. There are three key classes:

- Admin keys, listed in `SANDBOX_API_KEYS`: full access.
- Provisioner keys, listed in `SANDBOX_API_PROVISIONER_KEYS`: tenant create
  and delete only, see below.
- Tenant keys, minted by an admin or returned on tenant create, stored only as
  a SHA-256 hash alongside an 8-character lookup prefix, so a database read
  does not yield usable credentials. Revocation sets `revoked_at`, which the
  lookup query filters on.

### Provisioner keys

The class exists for automation that creates and tears down tenants without a
human in the path. Such automation needs a key with tenant-create rights, and
without this class the only one is an admin key, which would then be copied
into every deployment, config store and developer machine of that automation.
Credentials mostly leak by accident (a log line, a committed config file, a
laptop, an over-broad secret-store reader) and the odds grow with the number
of copies. The class makes those copies cheap to lose.

A provisioner key may `POST /admin/tenants` with `max_sandboxes` between `1`
and `SANDBOX_API_DEFAULT_MAX_SANDBOXES` (startup refuses the class when that
default is unlimited), and `DELETE /admin/tenants/{id}` for a tenant that owns
no sandboxes (`409` otherwise; the key cannot delete sandboxes). Everything
else returns `403`: the middleware confines the key to `/admin/tenants`, and
every other handler there requires admin. Withholding `POST
/admin/tenants/{id}/keys` is what keeps existing tenants out of reach: it
returns a plaintext key for that tenant's sandboxes, and tenant ids are not
secrets. The two env sets must not overlap; startup fails if they do, because
admin keys are checked first and the key would silently be admin.

Two limits of that guarantee. It bounds what the key grants. Automation that
provisions tenants usually stores the tenant keys it receives, so whoever
fully compromises it holds those keys regardless; the class still keeps the
admin surface and tenants that automation never created out of reach. And the
quota bounds each tenant; a leaked key can create any number of tenants.

A consequence worth keeping in mind: any key on a tenant that was not created
together with the tenant was minted by an admin.

Authorization runs on every sandbox request through `canAccessSandbox` in
[internal/api/middleware_auth.go](../internal/api/middleware_auth.go):

- Read, delete and all proxied routes (exec and files) load the sandbox record
  and compare its `tenant_id` against the caller's. An admin key passes this
  check for every sandbox, whoever owns it.
- List is scoped at the query level with `ListByTenant`.
- Create sets `tenant_id` from the authenticated identity. A caller cannot
  assign a sandbox to another tenant. Create and list switch on the role
  explicitly and refuse anything but admin or tenant, so a new role can never
  fall through to the admin pseudo-tenant.
- Sandboxes owned by the admin pseudo-tenant are not visible to tenant keys.
- Admin routes under `/admin` require an admin key, except the two provisioner
  routes above.

Cross-tenant and non-existent sandboxes both return `404`, so a tenant cannot
use the status code to learn whether an ID exists.

Error bodies the API generates itself have runner-side sandbox paths stripped
before they are returned.

## API to runner

Runners do not know about tenants. A runner authenticates its caller and then
executes what it is asked to do, on the assumption that the API has already
decided the caller is entitled to it.

Runners register by opening a long-lived gRPC stream to the API, secured with
mTLS plus a bearer token, and advertise their ID, HTTP base URL and capacity.
The API checks that the certificate chains to its CA and that the token
matches; it does not bind the advertised ID to the certificate, so the ID is
taken as given and becomes the key under which that runner receives placements
and lifecycle calls. Lifecycle calls go the other way, over the runner's
`SandboxControl` gRPC listener, which also requires mTLS with a client
certificate signed by the configured CA, plus an API key in the call metadata.

Exec and file traffic is proxied over the runner's HTTP listener, which requires
the same two things. It serves TLS using the certificate configured for the
control gRPC listener, so both channels to a runner share one identity and one
rotation path, and the API presents its control-plane client certificate on the
proxy transport. Because Kubernetes probes cannot present a client certificate,
the listener negotiates with `VerifyClientCertIfGiven` rather than rejecting an
unauthenticated peer during the handshake, and `AuthMiddleware` in
[internal/runner/middleware_auth.go](../internal/runner/middleware_auth.go)
requires a verified certificate on every route except the health and metrics
endpoints. A leaked runner API key is therefore not by itself enough to drive
a runner: the caller also needs a key pair the CA has signed. Neither listener
looks further than the chain, though. Any client certificate the CA issued
satisfies them, and a runner's own registration certificate is one such
certificate when the same CA issues both, as it does in the chart's
cert-manager mode. Runner credentials are fleet-wide rather than per-runner, so
what one runner holds drives every runner.

The proxy replaces the caller's `X-Api-Key` with the runner API key before
forwarding, so tenant and admin keys never reach a runner, and it forwards the
trace context it established rather than the caller's header. Responses are
relayed to the client as the runner returned them, except a 3xx, which is
refused with `502` so a runner cannot redirect a client, and the API key it
holds, to another host.

The API keeps the TLS guarantee from being negotiated away by the runner. A
runner names its own address in heartbeats, and Go applies a transport's
`TLSClientConfig` only to `https` URLs, so a runner advertising an `http://`
base would receive proxied traffic, including the runner API key, in plaintext.
The API therefore refuses a non-https base twice: at registration, so such a
runner never enters the registry, and again before proxying a stored sandbox
record, which catches rows written before this rule existed.

For the same reason the proxy transport sets no `ServerName`. One transport
reaches every runner, and Go derives the verification name from the host it
dialled only while that field is empty, so a fixed name would be checked in
place of the host each runner advertises. Every runner able to present a
certificate for that one name could then answer for any other. Runners must
advertise a host their own certificate covers, so per-host verification is what
the deployment already promises. `SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME`
therefore applies to the control gRPC channel alone, whose dial address may
legitimately be an address the certificate does not name.

A runner only acts on sandbox IDs present in its own state, an in-memory map
for the Firecracker runtime and a Docker label filter for the Sysbox runtime.
An ID hosted by a different runner returns `404`. Routing is keyed entirely on
the `{id}` path segment; no request body, header or query parameter can
redirect a call to a different sandbox.

## Guest to host and network

Sandbox contents are untrusted. Both runtimes block egress to the same list of
IPv4 destinations, defined once in
[internal/runner/runtime/netpolicy/private_ranges.go](../internal/runner/runtime/netpolicy/private_ranges.go)
and covering RFC1918 space, loopback, link-local (which includes the instance
metadata service address), carrier-grade NAT, benchmarking and reserved space.
That is egress `public`, the default. Egress `none` drops every forwarded
packet from the sandbox's interface instead, DNS included. The mode is fixed
at creation and re-applied on every wake; a reconnect by id that names another
mode is refused. The runner echoes the mode it
applied; on a mismatch the API fails the create, stores no record, and asks
the runner to remove the sandbox (best effort; a leftover is logged as
untracked and unreachable through the API). So a runner from before egress
modes cannot hand out a public sandbox recorded as `none`.

The Firecracker runtime gives every slot its own network namespace with a
dedicated TAP device and veth uplink. The guest subnet is identical in every
namespace, and there is no bridge between them, so guests have no layer-2 path
to each other. The blocked ranges are dropped by iptables FORWARD rules inside
each namespace, which also covers the uplink addresses of other slots and the
runner's own addresses. Egress `none` is a `FORWARD` drop on the TAP inserted
ahead of those rules at activation; it goes with the namespace at teardown, so
it never carries over to the next sandbox on the slot. Guest kernels boot with
IPv6 disabled.

The Sysbox runtime puts sandboxes on one of two Docker bridges, both with
inter-container communication disabled: `runner-bridge` for `public`, and
`runner-no-egress` (a Docker internal network; bridge device `br-no-egress`)
for `none`. Two chains cover every container on a bridge: one in `DOCKER-USER`
for forwarded traffic, dropping the blocked ranges on the public bridge and
everything on the no-egress bridge, where Docker's internal-network isolation
sits behind it as defense in depth; and one in `INPUT` dropping the connections
a container opens to the runner host itself. The `INPUT` chain is needed
because `DOCKER-USER` is only consulted for forwarded packets, while anything
addressed to a host-local address is delivered locally and never reaches the
egress chain. Both match on the bridge interface rather than on the address the
runner assigned. Sandboxes cannot normally change their interface configuration
without `CAP_NET_ADMIN`, but interface matching keeps the network policy
independent of that capability boundary as defense in depth. A per-container
chain, keyed on the container's address as a destination, drops connections to
its daemon port. The runner's own connections are unaffected, being locally
generated and so never subject to a chain that only sees forwarded packets; the
exception for the gateway address applies only to packets that did not arrive
on the bridge, for the same reason the rules above avoid matching on addresses.
The two shared chains are rebuilt from empty at runner startup, after stale
containers are removed and before any sandbox can be created, so an upgraded
runner replacing the rules of an earlier version never does so with a container
on the bridge. Containers are created with IPv6 disabled. Every rule above, and
Docker's own inter-container block, applies to bridged traffic only on a host
running `br_netfilter` with `bridge-nf-call-iptables` enabled.
[scripts/setup-sysbox.sh](../scripts/setup-sysbox.sh) loads the module, and the
kernel enables the sysctl by default once it is loaded; a host that overrides
that default, or was not prepared by the script, must provide both.

The runner drops all Linux capabilities from every sandbox container and
restores none. The container runs as uid 1000, so no process holds an effective
capability, and the empty bounding set stops one being gained.

That alone would not close the path to root. A setuid-root binary still makes
its caller uid 0, and uid 0 owns `/usr/local` whatever capabilities it holds.
Two things close it: the runner sets `no-new-privileges`, which makes every
setuid binary inert, and the image ships no `sudo` and no setuid binary at all,
because the Firecracker runtime has no equivalent flag. Setgid bits are left in
place; a setgid binary can change its group, never its user.

A sandbox therefore has no root and no `apt-get`. Packages install unprivileged,
under `/home/user`: `pip` runs from a virtual environment at `/home/user/venv`,
and `npm install -g` writes to `/home/user/.npm-global`.
[internal/daemon/exec.go](../internal/daemon/exec.go) puts both first on the
`PATH` every command gets. The image also carries a compiler and the Python
headers, because a source build could not otherwise install.

The policy holds in every environment. Sysbox isolation and privileged local
DinD apply to the runner container, not the sandbox container. A sandbox
container is never privileged, because Docker then ignores `--cap-drop` and the
policy has no effect.

Within a sandbox, the daemon runs as uid 1000. File operations resolve every
path against the sandbox's root filesystem, so the containment is the sandbox
itself and the daemon's uid, not a workspace directory: whatever uid 1000 can
read or write in the guest is reachable through the file API. The daemon
authenticates nobody, so reachability is the whole boundary in front of its
exec and file APIs, on both runtimes.

## Build inputs

The service images, the sandbox image and the Firecracker guest assets are
built from inputs fetched at build time, and a substituted input would reach
every sandbox built from it. What is verified today:

- The Firecracker release tarball is checked against a SHA-256 that
  [scripts/firecracker.ee/install-runner-host.sh](../scripts/firecracker.ee/install-runner-host.sh)
  carries for the versions it knows; any other version needs
  `FIRECRACKER_TARBALL_SHA256`.
- The Node.js tarball in the sandbox image is checked against the SHA-256
  values recorded in [Dockerfile.sandbox](../Dockerfile.sandbox).
- Service images are published by version. The release workflow mirrors each
  version into the deployment registry by digest and refuses to replace a
  version that is already there.
- The SDK is published to npm through trusted publishing, with provenance.
- The Firecracker runner can check its guest assets against a release manifest.
  Setting `SANDBOX_RUNNER_FIRECRACKER_MANIFEST_PATH` alone makes it refuse to
  start if the daemon binary's SHA-256 differs from the one in the manifest;
  adding `SANDBOX_RUNNER_FIRECRACKER_EXPECTED_GIT_SHA` also pins the manifest's
  `git_sha`. Neither is set by default.

## Non-guarantees

These are known and accepted. Do not read the section above as implying any of
them.

**Egress is `public` or `none`.** A `public` sandbox can reach any address
outside the blocked ranges, which cover private and special-purpose IPv4 space
and are not a definition of what is internal. Per-sandbox allowlists are not
implemented.

**The runner does not enforce tenancy.** It has no tenant concept at all. Anyone
holding a runner API key and a client certificate the CA has signed can operate
on any sandbox any runner hosts, because the runner does not know who owns any
of them and its credentials are shared across the fleet rather than issued per
runner or per tenant; one leaked pair is a fleet-wide leak. Sandboxes
themselves cannot reach that listener: on Firecracker its addresses sit inside
the blocked ranges, and on Sysbox the bridge's `INPUT` chain drops connections
to the host. The chart can restrict the runner's listeners to the API's pods
with a NetworkPolicy (`networkPolicy.enabled`), off by default because many
clusters manage network policy elsewhere. Who else can reach a runner is a
property of the deployment, not of the service.

**Client-supplied sandbox IDs leak existence.** Creating a sandbox with an ID
that already belongs to another tenant returns `409` rather than `404`, so a
caller can learn that an ID is taken. This is deliberate, so that a client can
reconnect to its own deterministic IDs. Since IDs are UUIDs, it confirms an ID
the caller already holds and does not help discover new ones.

**Tenant quotas are not atomic.** `max_sandboxes` is compared against the
tenant's recorded sandboxes before a create is recorded, so concurrent creates
that all pass the check are all admitted. The excess is bounded by how many
creates the caller runs in parallel, not by time, and the tenant then holds more
than its quota until it deletes sandboxes.

**Per-sandbox disk usage is not bounded by default.** On the Sysbox runtime a
per-sandbox disk quota applies only when `SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB`
is non-zero and the runner's entrypoint managed to mount its quota pool at
startup; it defaults to `0`, and sandboxes then share the runner's storage with
no per-sandbox limit. The Firecracker runtime gives each sandbox a fixed-size
root filesystem image.

**Health and metrics endpoints are unauthenticated.** `/healthz` and `/metrics`
bypass auth on both the API and the runner, as do the runner's `/livez` and
`/readyz`. Requests to them are not access-logged. They expose fleet-level
counters — sandbox and runner counts, capacity, request and operation rates —
and carry no tenant or sandbox identifiers. Sandboxes cannot reach the runner's,
per the section above; the API serves its own on the public HTTP port by default,
so a deployment that enables ingress publishes them. Set
`SANDBOX_API_METRICS_LISTEN_ADDR` to move the API's `/metrics` to a port of its
own, which an ingress does not publish and a NetworkPolicy can restrict on its
own terms. `/healthz` stays on the public port either way. Restrict both to the
monitoring system.

**The database connection is encrypted but not verified by default.**
`SANDBOX_API_POSTGRES_SSLMODE` defaults to `require`, which encrypts the
connection without checking the server's certificate. Set `verify-full` to
check it against the API's trusted roots.

**A sandbox escape is a runner compromise.** If code escapes its sandbox, treat
every sandbox on that runner, and every credential mounted into it, as
compromised. Since those credentials are fleet-wide, so is the reach they give:
the runner's API key is the fleet's, and its certificate satisfies every
listener that trusts the CA which issued it. The Firecracker runtime's boundary
is a microVM, which is why it is the target runtime for multi-tenant workloads.
The macOS development setup runs runners privileged and is not an isolation
boundary at all.

**Sandboxes do not survive their runner.** If a runner is lost, its sandboxes
are lost with it. A restart counts: both runtimes remove every sandbox they find
at startup and rebuild nothing, so a runner upgrade or crash ends every sandbox
it hosted.

## Verification

The boundaries above are covered by tests rather than asserted on paper.

| Boundary | Tests |
| --- | --- |
| Cross-tenant read, list, delete, exec, files | [internal/api/handlers_tenants_test.go](../internal/api/handlers_tenants_test.go), [internal/api/handlers_proxy_tenant_test.go](../internal/api/handlers_proxy_tenant_test.go), [e2e/tests/sandbox-api.spec.ts](../e2e/tests/sandbox-api.spec.ts) |
| Client-supplied ID conflicts | [internal/api/handlers_create_sandbox_test.go](../internal/api/handlers_create_sandbox_test.go) |
| Admin route gating and key revocation | [internal/api/handlers_tenants_test.go](../internal/api/handlers_tenants_test.go) |
| Provisioner keys: the two allowed routes, `403` on every other route, quota bounds, no fall-through to the admin pseudo-tenant | [internal/api/handlers_provisioner_test.go](../internal/api/handlers_provisioner_test.go), [internal/api/config/config_test.go](../internal/api/config/config_test.go), [e2e/tests/provisioner-key.spec.ts](../e2e/tests/provisioner-key.spec.ts) |
| Runner listeners require a CA-signed client certificate; the API verifies each runner's host name and refuses a non-https base | [internal/runner/mtls_test.go](../internal/runner/mtls_test.go), [internal/api/runnertls_test.go](../internal/api/runnertls_test.go), [internal/api/registry/validate_test.go](../internal/api/registry/validate_test.go) |
| Sandbox-to-sandbox and blocked-range egress; egress `none` across stop/wake and slot reuse | [e2e/tests/network-isolation.spec.ts](../e2e/tests/network-isolation.spec.ts) |
| Docker capability policy, absence of a root path, and denied network administration | [e2e/tests/sandbox-capabilities.spec.ts](../e2e/tests/sandbox-capabilities.spec.ts) |
| Unprivileged npm and PyPI installation, and the build toolchain | [e2e/tests/sandbox-packages.spec.ts](../e2e/tests/sandbox-packages.spec.ts) |
| Egress rule generation | [internal/runner/runtime/firecracker.ee/network/egress_test.go](../internal/runner/runtime/firecracker.ee/network/egress_test.go) |
| Egress mode: API to runner to network and rules, refused when the runner does not confirm it | [internal/api/handlers_create_sandbox_test.go](../internal/api/handlers_create_sandbox_test.go), [internal/runner/grpc_control_test.go](../internal/runner/grpc_control_test.go), [internal/runner/runtime/docker/runtime_test.go](../internal/runner/runtime/docker/runtime_test.go), [internal/runner/runtime/firecracker.ee/network_wirer_test.go](../internal/runner/runtime/firecracker.ee/network_wirer_test.go) |

The network isolation suite is untagged, so it runs against both the Sysbox and
the Firecracker runner. Its blocked-range test probes one address per listed
range, so it shows the rules are applied, not that the list is complete. The
capability suite is Docker-only: the setuid clearing it depends on is a property
of the image both runtimes share, but no Firecracker test asserts it, and
`no-new-privileges` has no Firecracker counterpart to test.
