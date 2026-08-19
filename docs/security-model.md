# Security Model

What the sandbox service isolates, where each boundary is enforced, and what it
explicitly does not promise. [architecture.md](architecture.md) has a summary
table of the same mechanisms; this document is the longer form, aimed at anyone
reviewing the service or deciding what it is safe to run on it.

## Trust boundaries

Three boundaries carry the isolation guarantees. They are enforced by different
components and fail in different ways.

| Boundary | Enforced by | Protects against |
| --- | --- | --- |
| Client → API | API key auth and per-request tenant checks | One tenant reaching another tenant's sandboxes |
| API → Runner | mTLS and a shared runner API key | Unauthorized control of sandbox lifecycles |
| Guest → host and network | Network namespaces, iptables, and the container or microVM boundary | Untrusted code escaping its sandbox or reaching other sandboxes |

Everything trusts the layer above it. Sandboxes trust nothing.

## Client to API

The API is the only component that knows about tenants. Every other component
trusts it to have made the decision correctly.

Requests carry an `X-Api-Key` header. Keys listed in `SANDBOX_API_KEYS` are
admin keys with full access. All other keys are tenant keys, minted by an admin
and stored only as a SHA-256 hash alongside an 8-character lookup prefix, so a
database read does not yield usable credentials. Revocation sets `revoked_at`,
which the lookup query filters on.

Authorization runs on every sandbox request through `canAccessSandbox` in
[internal/api/middleware_auth.go](../internal/api/middleware_auth.go):

- Read, delete and all proxied routes (exec and files) load the sandbox record
  and compare its `tenant_id` against the caller's.
- List is scoped at the query level with `ListByTenant`.
- Create sets `tenant_id` from the authenticated identity. A caller cannot
  assign a sandbox to another tenant.
- Sandboxes owned by the admin pseudo-tenant are not visible to tenant keys.
- Admin routes under `/admin` require an admin key.

Cross-tenant and non-existent sandboxes both return `404`, so a tenant cannot
use the status code to learn whether an ID exists.

## API to runner

Runners do not know about tenants. A runner authenticates its caller and then
executes what it is asked to do, on the assumption that the API has already
decided the caller is entitled to it.

Runners register by opening a long-lived gRPC stream to the API, secured with
mTLS plus a bearer token, and advertise their ID, HTTP base URL and capacity.
Lifecycle calls go the other way, over the runner's `SandboxControl` gRPC
listener, which also requires mTLS with a client certificate signed by the
configured CA, plus an API key in the call metadata. Exec and file traffic is
proxied over the runner's HTTP listener, which is authenticated by that same
shared API key.

A runner only acts on sandbox IDs present in its own state, an in-memory map
for the Firecracker runtime and a Docker label filter for the Sysbox runtime.
An ID hosted by a different runner returns `404`. Routing is keyed entirely on
the `{id}` path segment; no request body, header or query parameter can
redirect a call to a different sandbox.

## Guest to host and network

Sandbox contents are untrusted. Both runtimes block egress to the same list of
IPv4 destinations, defined once in
[internal/runner/runtime/netpolicy/private_ranges.go](../internal/runner/runtime/netpolicy/private_ranges.go)
and covering RFC1918 space, loopback, link-local (which includes the cloud
metadata endpoint), carrier-grade NAT, benchmarking and reserved space.

The Firecracker runtime gives every slot its own network namespace with a
dedicated TAP device and veth uplink. The guest subnet is identical in every
namespace, and there is no bridge between them, so guests have no layer-2 path
to each other. The blocked ranges are dropped by iptables FORWARD rules inside
each namespace, which also covers the uplink addresses of other slots and the
runner's own addresses. Guest kernels boot with IPv6 disabled.

The Sysbox runtime puts sandboxes on a shared Docker bridge created with
inter-container communication disabled, and adds per-container chains in
`DOCKER-USER`: an egress chain dropping the same ranges, and an ingress chain
that only permits the bridge gateway to reach the sandbox daemon port. A third
chain in `INPUT` drops connections the container opens to the runner host itself,
which the egress chain cannot cover because `DOCKER-USER` is only consulted for
forwarded packets, while anything addressed to a host-local address is delivered
locally. Containers are created with IPv6 disabled.

Within a sandbox, the daemon runs as a non-root user, and file operations are
path-validated to keep them inside the sandbox.

## Non-guarantees

These are known and accepted. Do not read the section above as implying any of
them.

**Egress is not restricted to an allowlist.** Sandboxes can reach any public
address. The blocked ranges above stop a sandbox reaching internal
infrastructure, not the internet. Configurable per-sandbox allowlists are
planned, not implemented.

**The runner does not enforce tenancy.** It has no tenant concept at all. Anyone
holding a runner API key with network access to a runner's HTTP listener can
operate on any sandbox that runner hosts. Sandboxes themselves cannot reach that
listener: on Firecracker its addresses sit inside the blocked ranges, and on
Sysbox the per-container `INPUT` chain drops connections to the host. Runner
credentials are shared across the fleet rather than issued per runner or per
tenant, and the HTTP listener uses the API key alone, without mTLS. Deployments
are expected to keep runner listeners reachable only from the API.

**Client-supplied sandbox IDs leak existence.** Creating a sandbox with an ID
that already belongs to another tenant returns `409` rather than `404`, so a
caller can learn that an ID is taken. This is deliberate, so that a client can
reconnect to its own deterministic IDs. Since IDs are UUIDs, it confirms an ID
the caller already holds and does not help discover new ones.

**Tenant quotas are not atomic.** `max_sandboxes` is a check-then-act before
create, so concurrent requests can briefly exceed it.

**Health and metrics endpoints are unauthenticated.** `/healthz` and `/metrics`
bypass auth on both the API and the runner. They expose fleet-level counters —
sandbox and runner counts, capacity, request and operation rates — and carry no
tenant or sandbox identifiers. Sandboxes cannot reach the runner's, per the
section above; the API serves its own on the public HTTP port, so a deployment
that enables ingress publishes them. Restrict both to the monitoring system.

**A sandbox escape is a runner compromise.** If code escapes its sandbox, treat
every sandbox on that runner, and every credential mounted into it, as
compromised. The Firecracker runtime's boundary is a microVM, which is why it is
the target runtime for multi-tenant workloads. The macOS development setup runs
runners privileged and is not an isolation boundary at all.

**Sandboxes do not survive their runner.** If a runner is lost, its sandboxes
are lost with it.

## Verification

The boundaries above are covered by tests rather than asserted on paper.

| Boundary | Tests |
| --- | --- |
| Cross-tenant read, list, delete, exec, files | [internal/api/handlers_tenants_test.go](../internal/api/handlers_tenants_test.go), [internal/api/handlers_proxy_tenant_test.go](../internal/api/handlers_proxy_tenant_test.go), [e2e/tests/sandbox-api.spec.ts](../e2e/tests/sandbox-api.spec.ts) |
| Client-supplied ID conflicts | [internal/api/handlers_create_sandbox_test.go](../internal/api/handlers_create_sandbox_test.go) |
| Admin route gating and key revocation | [internal/api/handlers_tenants_test.go](../internal/api/handlers_tenants_test.go) |
| Sandbox-to-sandbox and blocked-range egress | [e2e/tests/network-isolation.spec.ts](../e2e/tests/network-isolation.spec.ts) |
| Egress rule generation | [internal/runner/runtime/firecracker.ee/network/egress_test.go](../internal/runner/runtime/firecracker.ee/network/egress_test.go) |

The network isolation suite is untagged, so it runs against both the Sysbox and
the Firecracker runner.
