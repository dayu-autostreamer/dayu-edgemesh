# Dayu-Edgemesh

## Brief Introduction
This project is based on [Edgemesh](https://github.com/kubeedge/edgemesh/) (v1.17.0)

The current Dayu EdgeMesh version is [`v1.1`](https://github.com/dayu-autostreamer/dayu-edgemesh/tree/v1.1).
The legacy `v1.0` baseline and the managed-runtime changes in `v1.1` are documented in the
[changelog](CHANGELOG.md).

We remove the loadbalancer policy of edgemesh and add extra validation for service state reconciliation in edgemesh-agent, to meet the requirements of dayu system.

## Features

###  Loadbalance Policy Removal

In the special version of edgemesh for dayu system, we remove the fixed loadbalance mechanism, which forwards requests across corresponding nodes.

We decide the forwarding in scheduler of dayu system, thus unexpected forwarding action is dangerous to dayu.


### EdgeProxy State Reconciliation

This version also improves the stability of EdgeProxy service state on edge nodes.

In some edge deployments, deleted services may still exist in KubeEdge MetaServer cache for a period of time. In the original behavior, these stale services could be restored into `iptables` after `edgemesh-agent` restarted, which might cause:

- deleted services and namespaces to "revive" in `KUBE-PORTALS-*` and `KUBE-NODEPORT-*`
- `Failed to ensure portal` errors caused by stale `NodePort` conflicts
- current live services to be blocked by dirty proxy state

To avoid this problem, Dayu-Edgemesh now adds an extra validation path before keeping service state in userspace proxy:

- prefer native in-cluster Kubernetes API access
- fallback to serviceaccount token plus the real apiserver address discovered from `default/kubernetes` Endpoints
- fallback to well-known kubeconfig paths such as `/etc/kubeedge/config/kubeconfig`

With this change, stale services from deleted namespaces are filtered out from proxy state and cleaned from `iptables`, while current live services can still be programmed normally.

If direct validation is not available on a device, `edgemesh-agent` will log:
```text
Falling back to primary Kubernetes client for EdgeMesh proxy validation
```

If direct validation is enabled successfully, `edgemesh-agent` will log:
```text
Using independent Kubernetes API validation source for EdgeMesh proxy state
```

### Opt-in RuntimeService Data Path

Dayu-Edgemesh now includes a revision-scoped data path for Sedna
`RuntimeService` workloads. It replaces per-runtime Kubernetes discovery with
one local, in-memory projection built from EdgeProxy's existing Service and
Endpoints informers. No extra watch, polling loop, hosts file, or persistent
cache is introduced on an edge node.

This path is deliberately disabled by default:

```yaml
modules:
  edgeProxy:
    enable: true
    serviceFilterMode: FilterIfLabelExists
    managedRuntime:
      enable: false
```

The Helm chart treats enablement as a managed install profile and requires an
explicit agent image built from this source revision. This prevents a new
ConfigMap from being paired silently with the default upstream binary:

```sh
helm upgrade --install edgemesh ./build/helm/edgemesh \
  --namespace kubeedge \
  --set agent.modules.edgeProxy.serviceFilterMode=FilterIfLabelExists \
  --set agent.modules.edgeProxy.managedRuntime.enable=true \
  --set-string agent.modules.edgeProxy.managedRuntime.image=dayuhub/edgemesh-agent:v1.1
```

Rendering fails if the gate is enabled without that image. Raw-manifest users
must likewise replace the agent DaemonSet image before changing the gate.

With `enable: false`, the existing `JointMultiEdgeService`/NodePort path and
its independent stale-service validation remain unchanged. Enabling the gate
adds exact routing only for ClusterIP Services labelled
`dayu.io/mesh-managed=true`; unlabelled Services retain legacy proxy behavior.
When the gate is enabled, both managed and legacy Services use the primary
MetaServer-backed informer as their validation source, avoiding a second
cloud API request path on edge nodes.

A managed route becomes selectable only after all of the following agree on
the same revision and Kubernetes object incarnation:

- one valid Service and one ready Endpoints address are present;
- the endpoint references the expected node and an exact Pod UID;
- the userspace portal and load-balancer state have been applied; and
- the primary informer caches have completed initial synchronization.

The proxy reports a strict
`PENDING -> APPLIED -> REMOVING -> REMOVED` portal lifecycle.
`PENDING` is emitted before portal programming, so a proxier event that arrives
before the projection's Service event is still marked managed and fails closed.
`REMOVING` preserves that tombstone while the exact portal and socket are
dismantled; only then is `REMOVED` emitted. Final deletion also removes legacy
load-balancer state, while same-port replacement preserves its endpoint cache.
Only the
exact projection plus a synced source and `APPLIED` callback opens the route.

Any missing or mismatched state fails closed instead of falling back to random
legacy load balancing. The Sedna local controller can confirm a route through
the loopback-only endpoint `GET http://127.0.0.1:10551/v1/routes/{serviceUID}`.
See [Managed Runtime Architecture](docs/managed-runtime.md) and its
[Chinese version](docs/zh/managed-runtime.md) for the resource contract,
compatibility boundary, state model, status API, and rollout procedure.

## Quick Start

clone repository
```bash
git clone --branch v1.1 https://github.com/dayu-autostreamer/dayu-edgemesh
```

add relay node
```bash
vim build/agent/resources/04-configmap.yaml
# add cloud server as relay node
```

deploy crds
(specify the image if necessary)
```bash
kubectl apply -f build/crds/istio/
kubectl apply -f build/agent/resources/
```

check edgemesh is running
```bash
kubectl get pods -n kubeedge
```

uninstall edgemesh
```bash
kubectl delete -f build/crds/istio/
kubectl delete -f build/agent/resources/
```


## How to Build 

clone repository
```bash
git clone --branch v1.1 https://github.com/dayu-autostreamer/dayu-edgemesh
```

set meta information of building
```bash
# configure buildx buildkitd (default as empty, example at hack/resource/buildkitd_template.toml)
vim hack/resource/buildkitd.toml
# configure buildx driver-opt (default as empty, example at hack/resource/driver_opts_template.toml)
vim hack/resource/driver_opts.toml

# set docker meta info
# default REG is docker.io
# default IMAGE_REPO is $(REG)/dayuhub
# default IMAGE_TAG is v1.1
export REG=xxx
export IMAGE_REPO=xxx
export IMAGE_TAG=v1.1
```

Cross build edgemesh-agent and edgemesh-server image
```bash
make docker-cross-build
```
