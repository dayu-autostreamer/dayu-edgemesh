# Managed Runtime Architecture

## Purpose

The managed runtime path gives Dayu a deterministic, revision-scoped route to
one runtime Pod without making application processes discover Kubernetes
objects. EdgeMesh projects the Service and Endpoints objects it already
receives from the KubeEdge MetaServer into immutable in-memory snapshots.

The design has three hard boundaries:

1. It is label-isolated. Existing `JointMultiEdgeService`, NodePort, and
   unlabelled Service behavior is preserved while the feature is enabled.
2. It is event-driven. No second Kubernetes client, periodic list, synchronous
   cache refresh, hosts file, or EdgeMesh checkpoint is added.
3. It is fail-closed. An incomplete or ambiguous managed route never enters
   the legacy/random load-balancer path.

KubeEdge MetaManager remains the durable edge-side cache for Service and
Endpoints objects. EdgeMesh shared informers remain the process cache. The
managed store is only a validated, immutable projection for data-plane reads.

## Compatibility boundary

The feature gate is under `modules.edgeProxy.managedRuntime.enable` and defaults
to `true` in the Dayu v1.1 distribution so Dayu v1.4 works without a deployment
override.

| Gate | Managed Service | Legacy Service / JMES / NodePort | Validation source |
|---|---|---|---|
| `false` | Not interpreted | Existing behavior | Existing independent validation with fallback |
| `true` | Exact managed route, fail-closed | Existing proxy semantics | Primary MetaServer-backed informer |

Enabling managed runtime requires EdgeProxy to be enabled and
`serviceFilterMode: FilterIfLabelExists`. Configuration validation rejects any
other combination. Rollback is therefore a single configuration change to
`managedRuntime.enable: false`; no legacy CRD or proxy code is removed.

The raw manifests and Helm chart default to the immutable
`dayuhub/edgemesh-agent:v1.1` image that contains this implementation. Private
registries can override `agent.modules.edgeProxy.managedRuntime.image` without
changing the feature gate.

## Dayu v1.3 and v1.4 coexistence

One v1.1 EdgeMesh agent per node can serve both generations concurrently:

- deploy Dayu v1.3 and v1.4 control planes in distinct namespaces;
- run a single matching dayu-sedna v1.1 GM/LC plane and one dayu-edgemesh v1.1
  agent DaemonSet across their target nodes;
- v1.3 JMES/NodePort Services remain unlabelled and use the legacy proxy and
  load-balancer state;
- v1.4 RuntimeService Services carry `dayu.io/mesh-managed=true` and use the
  exact fail-closed projection.

Do not run a second EdgeMesh agent on the same node. Both agents would own the
same host-network proxy/iptables state and compete for `127.0.0.1:10551`.
With the gate enabled, legacy validation also reuses the primary MetaServer
informer cache; routing semantics remain legacy, but stale-object validation is
no longer sourced from a separate cloud API client.

## Data and acknowledgement flow

```text
Sedna RuntimeService controller
  -> Deployment (one replica on one target node)
  -> ClusterIP Service (one named TCP port)
  -> Kubernetes Endpoints controller
  -> KubeEdge MetaManager / MetaServer
  -> existing EdgeMesh Service + Endpoints informers
  -> managed in-memory projection
  -> userspace portal lifecycle (PENDING -> APPLIED -> REMOVING -> REMOVED)
  -> loopback status API
  -> Sedna local-controller activation acknowledgement
```

There is no third cache to refresh. Informer handlers do no filesystem I/O,
and data-plane selection reads an atomic snapshot without Kubernetes API calls.

The userspace proxier emits `PENDING` before it attempts to program a managed
portal. This first event records the Service UID and marks the namespaced key as
managed even if the projection's Service handler has not run yet. The route is
therefore closed throughout setup. `APPLIED` is emitted only after both portal
programming and load-balancer registration succeed. `REMOVING` is emitted before
teardown and keeps the managed tombstone fail-closed; `REMOVED` is emitted only
after the exact Service incarnation's portal and socket are gone. A final
delete also removes legacy load-balancer state; a same-port replacement keeps
the shared endpoint cache so an unchanged legacy Endpoints object is not lost.
Failed setup and teardown are merged back into the proxier's existing bounded
sync loop, which always reconciles the latest desired Service UID. Only an exact validated
projection, source `SYNCED`, and the matching `APPLIED` identity can open the
route.

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
incarnations and prevent ABA reuse. Update events supersede older objects at a
namespaced key; delete events are UID-guarded so stale tombstones cannot remove
a newer route. Portal callbacks also carry the exact Service UID.

The proxy's `PENDING` and `REMOVING` lifecycle events force the route out of
`APPLIED`; `REMOVED` alone may release the tombstone. These transitions cannot
be masked by informer callback ordering.

`SYNCED` means the primary Service and Endpoints informer caches completed
their initial synchronization. It is intentionally not presented as a
continuous MetaServer connection-health signal.

`APPLIED` is a rollout acknowledgement, not a continuous application-health
signal. Runtime health remains the responsibility of Dayu health monitoring.

## Loopback status API

The API listens on `127.0.0.1:10551` only. EdgeMesh and the Sedna local
controller run with host networking, so no externally exposed Service is
needed.

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Process status |
| `GET /readyz` | `200` only after primary Service/Endpoints caches sync |
| `GET /v1/routes` | Diagnostic snapshot of all projected routes |
| `GET /v1/routes/{serviceUID}` | Exact activation query by Service UID |

The exact-route endpoint returns:

```json
{
  "serviceUID": "...",
  "runtimeServiceUID": "...",
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

Status codes are `404` for an unknown Service UID, `503` for any route not
exactly `APPLIED` with a synced source, and `200` only for the exact applied
incarnation. Callers must compare every identity field; a `200` for a different
revision or Pod UID is not an activation acknowledgement.

## Enablement and rollback

1. Deploy matching dayu-sedna v1.1 and dayu-edgemesh v1.1 components. The raw
   manifests and Helm values already enable managed runtime with a matching
   image.
2. Roll out EdgeMesh. For Helm, no feature-specific values are required:

   ```sh
   helm upgrade --install edgemesh ./build/helm/edgemesh \
     --namespace kubeedge
   ```

   Override only the image value when using a private registry.
3. Confirm existing v1.3 JMES/NodePort traffic remains healthy when both
   generations share the agent.
4. Confirm `curl -fsS http://127.0.0.1:10551/readyz` succeeds on each node.
5. Create a v1.4 RuntimeService revision and require an exact `200` activation
   response before publishing it in Dayu's runtime directory.
6. Roll back by setting the gate to `false`; legacy resources need no change.

Do not mutate a live revision in place. Create a new revision, wait for its
exact activation, publish it, and then retire the old revision. This keeps the
Service and Pod UID barrier meaningful and avoids partial routing transitions.
