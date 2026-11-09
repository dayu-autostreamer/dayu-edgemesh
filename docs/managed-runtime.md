# Managed Runtime Architecture

## Purpose

EdgeMesh provides deterministic, revision-scoped routes without giving Dayu application processes Kubernetes clients.
DNS and proxy clients use one resolved `kubeAPIConfig`; managed and legacy Services share the proxy's Service/Endpoints
informers. The managed store validates these events into immutable snapshots. Incomplete or ambiguous managed routes
remain fail-closed and never fall back to random/legacy endpoint selection.

For Dayu, configure a real Kubernetes API endpoint reachable from every agent using the existing `master` and/or
`kubeConfig` fields. There is no private-address discovery, administrative kubeconfig search, second validation client,
MetaServer fallback on API failure, hosts projection, or managed repair poller.

## API configuration and availability

```yaml
kubeAPIConfig:
  master: https://api.example.com:6443
  # Optional mounted credentials; master overrides the server in this file.
  # kubeConfig: /etc/edgemesh/api.kubeconfig
```

`master` without `kubeConfig` uses in-cluster service-account credentials when available. Otherwise provide a mounted
kubeconfig with the agent's existing read-only permissions for Services, Endpoints, namespaces, Pods and any enabled
Istio resources. Use a directly reachable control-plane address and valid CA/server identity; bootstrapping this client
through a Service IP that requires EdgeMesh itself can create a dependency cycle. No Dayu runtime Pod needs new API
permissions. Helm uses the same fields under `agent.kubeAPIConfig`.

CoreDNS uses a private generated `dns-kubeconfig` (mode 0600) from the resolved client configuration, including the
master override, TLS and token-file path. Token rotation remains supported. This is connection configuration, not a
resource cache. CoreDNS retains its standard Kubernetes plugin/informer and DNS cache; positive TTL stays 30 seconds
and negative TTL remains bounded to 3 seconds. EdgeProxy and the managed projection share their own informer stream.
The watch client has no short global HTTP timeout; client-go owns watch renewal and LIST recovery.
Proxy UID/port validation uses this same Service informer; a separately ordered Namespace watch cannot veto a valid
Service. DNS and proxy caches are eventually consistent, not atomic: DNS resolution alone does not prove an exact route
is already APPLIED.

The upstream automatic edge mode still chooses MetaServer when neither `master` nor `kubeConfig` is supplied. That
explicitly retained configuration mode does **not** bypass KubeEdge cache defects: Dayu deployments requiring this
recovery must select the real API. No runtime failover silently switches an established source.

During an API outage, already synchronized in-memory caches remain available. API errors are not deletion evidence;
watch recovery/relist supplies missing updates and deletion tombstones when connectivity returns. A successful empty
LIST removes even previously `APPLIED` objects. Restarting an agent while disconnected cannot reconstruct its cache
from the cloud; this design does not add persistent offline metadata or promise recovery of an offline cold start.

## Compatibility boundary

`modules.edgeProxy.managedRuntime.enable` defaults to `true` in the Dayu distribution. It requires EdgeProxy and
`serviceFilterMode: FilterIfLabelExists`. With the gate disabled, legacy routing remains available; with it enabled,
labelled managed Services use exact identity validation. Both cases use the same configured authoritative API.

## Dayu v1.3 and v1.4 coexistence

Use one EdgeMesh agent per node. Unlabelled JMES/NodePort Services retain legacy forwarding semantics; Services labelled
`dayu.io/mesh-managed=true` use exact RuntimeService routes. Labels select routing behavior, not a different source of
metadata. Separate Dayu namespaces share the same host proxy. A stale teardown cannot release a replacement's NodePort.

## Data and acknowledgement flow

```text
Kubernetes Service + Endpoints
  -> shared EdgeProxy informers (LIST/WATCH)
  -> managed identity projection + userspace proxy
  -> PENDING / APPLIED / REMOVING / REMOVED callbacks
  -> loopback status API
  -> Sedna local-controller activation acknowledgement
```

Informer handlers perform no filesystem I/O. Data-plane selection reads an atomic snapshot without API requests.
`PENDING` marks the Service UID managed before portal creation, even if the projection handler has not run.
`APPLIED` follows successful portal and load-balancer setup. `REMOVING` closes the route before teardown; `REMOVED`
follows removal of that incarnation's portal/socket. Final deletion also removes legacy endpoint state, while a
same-port replacement retains the shared endpoint cache. Failed setup/teardown retries in the existing bounded sync
loop against the latest desired UID.

That loop also compares actual NAT/filter rules in the five EdgeMesh userspace chains with active portals. It removes
orphan rules even if the in-memory entry is gone, including old proxy targets under a reused Service name or NodePort.
Foundation chains and orphan rules are reconciled before consuming new Service changes or reporting APPLIED. Failed
initialization, observation or cleanup retains the pending changes for the normal retry loop. Active rules, replacement
port claims and other owners' chains remain untouched. It does not flush the host ruleset.

TCP accepts proceed independently of endpoint setup, with at most 32 setups per listener. Existing endpoint retry
budgets now bound actual local dialing and remote libp2p stream/handshake operations. The receiving peer also bounds
its handshake and endpoint dial. Failures close/reset owned connections; successful streams clear setup deadlines so
long-lived Redis and task traffic are unaffected. Established streams do not consume setup slots.

## Resource contract

Only a Service explicitly labelled `dayu.io/mesh-managed=true` enters this
path. The Service must be non-headless `ClusterIP`, expose exactly one named
TCP port whose Service `port` equals its positive numeric `targetPort`, and
have no NodePort. Its
Endpoints object must contain exactly one subset, one matching port, one ready
address, and no not-ready addresses. The address must:

- have a valid IP;
- identify the Service's `dayu.io/target-node`;
- reference a Pod by kind, name, non-empty UID, and the Service namespace.

Service and Endpoints must carry the same complete identity labels:

| Label | Meaning |
|---|---|
| `dayu.io/mesh-managed=true` | Explicit opt-in marker |
| `dayu.io/install-id` | Dayu installation identity |
| `dayu.io/deployment-revision` | Positive base-10 `int64` revision |
| `dayu.io/runtime-id` | RuntimeService identity; must equal the Service name |
| `dayu.io/component` | Dayu component role |
| `dayu.io/runtime-service-uid` | Exact RuntimeService UID |

Service annotations are:

| Annotation | Requirement |
|---|---|
| `dayu.io/target-node` | Required and must equal endpoint `nodeName` |
| `dayu.io/logical-service` | Optional Dayu directory metadata |

Endpoints annotations are intentionally not part of identity: the Kubernetes
Endpoints controller copies Service labels, but does not promise to copy its
annotations. EdgeMesh reads annotations only from the Service.

The resulting DNS name is
`<service>.<namespace>.svc.cluster.local`. Standard Service DNS remains the
only DNS source; EdgeMesh does not maintain a parallel hosts database.

## State and stale-event protection

Every route is one of:

- `READY`: the resource contract is valid, but proxy application or source
  synchronization is not yet confirmed;
- `APPLIED`: exact current informer identity, userspace portal, and source sync
  all agree;
- `DEGRADED`: the current Service/Endpoints contract is invalid or an observed
  required object disappeared.

Data-plane selection is allowed only for `APPLIED` plus source `SYNCED`.
`Service UID`, `RuntimeService UID`, and endpoint `Pod UID` distinguish object
incarnations and prevent ABA reuse. When both values are canonical decimal
integers, an incoming Service or Endpoints update with an older Kubernetes
`resourceVersion` is ignored before it can replace the current projection.
Newer invalid updates still fail closed. Delete events are UID-guarded so stale
tombstones cannot remove a newer route. Portal callbacks also carry the exact
Service UID.

`SYNCED` records completion of initial informer synchronization, not continuous API reachability. `APPLIED` is a
rollout acknowledgement, not proof of current application health. Dayu owns runtime health checks.

## Loopback status API

The API listens on `127.0.0.1:10551` only. EdgeMesh and the Sedna local
controller run with host networking, so no externally exposed Service is
needed.

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Process status |
| `GET /readyz` | `200` only after Service/Endpoints caches sync |
| `GET /v1/routes` | Diagnostic snapshot of all projected routes |
| `GET /v1/routes/{serviceUID}` | Exact activation query by Service UID |

The exact-route endpoint returns:

```json
{
  "serviceUID": "...",
  "runtimeServiceUID": "...",
  "serviceResourceVersion": "12345",
  "endpointsResourceVersion": "12346",
  "endpointPodUID": "...",
  "deploymentRevision": 7,
  "runtimeID": "runtime-a",
  "state": "APPLIED",
  "localSequence": 42,
  "sourceState": "SYNCED",
  "targetNode": "edge-1",
  "observedAt": "2026-01-01T00:00:00Z"
}
```

The two observed resource versions make it possible to compare EdgeMesh's
projection with the configured authoritative API objects without mutating the
resource to provoke another event.

Status codes are `404` for an unknown Service UID, `503` for any route not
exactly `APPLIED` with a synced source, and `200` only for the exact applied
incarnation. Callers must compare every identity field; a `200` for a different
revision or Pod UID is not an activation acknowledgement.

## Enablement and rollback

Configure the reachable authoritative API and build/publish a matching agent image before rollout; repository edits do
not update an existing `v1.1` image. The raw manifest and Helm values document the API fields without embedding a private
cluster address. Confirm `/readyz`, an exact Service/RuntimeService/Pod UID activation response, DNS resolution and a
real request through the Service on each target node. Cache sync alone is not end-to-end validation.

Disabling `managedRuntime.enable` disables managed interpretation without changing the API source. Do not mutate a live
revision in place: create and activate a new revision, publish it, then retire the previous one.

## Regression tests

Use the repository's Go 1.20-compatible vendored dependencies on Linux:

```sh
go test -mod=vendor ./pkg/clients ./pkg/dns ./pkg/meshstate ./pkg/proxy ./pkg/loadbalancer ./pkg/tunnel
go test -mod=vendor k8s.io/kubernetes/pkg/proxy/userspace
```

The tests use fake API watches/iptables, local sockets and in-memory streams. They cover API failure versus confirmed
delete, relist of applied routes, stale UIDs, orphan kernel rules, replacement NodePorts, concurrent setup and handshake
timeouts. They do not start an agent, alter host rules, or establish Kubernetes/edge-node deployment correctness.
