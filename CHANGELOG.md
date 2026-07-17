# Changelog

All notable changes to the Dayu EdgeMesh fork are documented in this file. The
versions below describe the Dayu integration layer; upstream EdgeMesh and
Kubernetes API versions remain unchanged.

## [v1.1] - 2026-07-17

### Added

- Added the opt-in managed `RuntimeService` data path derived from EdgeProxy's
  existing Service and Endpoints informer caches, without adding an edge-side
  Kubernetes client, watch, or polling loop.
- Added revision-, Service UID-, Pod UID-, node-, and endpoint-exact route
  projection with fail-closed selection semantics.
- Added the strict `PENDING -> APPLIED -> REMOVING -> REMOVED` portal lifecycle
  and loopback-only readiness/route-status APIs consumed by Sedna LC.
- Added managed-runtime configuration, raw manifests, Helm support, English and
  Chinese documentation, and focused validation, mesh-state, load-balancer,
  proxy, and userspace proxier tests.

### Changed

- Set the default Dayu EdgeMesh image build and raw-manifest tags to `v1.1`,
  and replaced temporary managed-runtime image examples with immutable `v1.1`
  references.
- Enabled EdgeProxy and managed RuntimeService routing by default across the Go
  configuration, raw manifests, and Helm values, with the matching v1.1 agent
  image embedded so Dayu v1.4 needs no feature-specific deployment override.
- Bumped the changed EdgeMesh and agent Helm charts to `0.2.1`, the gateway
  chart to `0.1.1`, and regenerated both tracked chart packages. Helm chart
  versions remain independent from the Dayu release tag.
- Documented one-agent coexistence for Dayu v1.3 JMES/NodePort and Dayu v1.4
  RuntimeService traffic in separate namespaces.

### Fixed

- Corrected the raw agent ConfigMap so `managedRuntime`, `loadBalancer`, and
  their nested settings are emitted under `modules.edgeProxy`, and added a
  regression test for the embedded configuration structure.
- Prevented out-of-order Service or Endpoints informer updates with older
  canonical decimal `resourceVersion` values from replacing a newer
  managed-runtime projection and leaving an otherwise ready route stuck in
  `DEGRADED`.
- Added a bounded, degraded-only Service/Endpoints point lookup through the
  existing MetaServer client so a lost recovery watch event can self-heal
  without introducing a second Kubernetes client or weakening fail-closed
  routing.
- Exposed the observed Service and Endpoints resource versions in the local
  route status response for cache-path diagnosis.

### Compatibility

- Managed runtime is enabled by default, but existing unlabelled Services,
  `JointMultiEdgeService`, and NodePort routing retain their legacy data path.
  Setting the gate to `false` remains available for strict pre-managed
  validation behavior.
- The current Dayu managed-runtime path requires both dayu-edgemesh `v1.1` and
  dayu-sedna `v1.1`; `v1.0` does not implement exact-route activation.

## [v1.0] - 2026-03-16

- Established the legacy Dayu EdgeMesh baseline, including Dayu image builds,
  NodePort allocation behavior, stale-Service validation, and iptables cleanup
  fixes.

[v1.1]: https://github.com/dayu-autostreamer/dayu-edgemesh/compare/v1.0...v1.1
[v1.0]: https://github.com/dayu-autostreamer/dayu-edgemesh/tree/v1.0
