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

DNS, legacy proxying and managed routing use the same configured Kubernetes API authority. For Dayu, select a real
API endpoint reachable from every agent through `kubeAPIConfig.master` and/or `kubeConfig` (Helm:
`agent.kubeAPIConfig`). API errors preserve the last synchronized cache; client-go LIST/WATCH recovery supplies missing
updates and deletes. Automatic edge mode still uses MetaServer if neither field is set, so configure the real API to
bypass KubeEdge metadata-cache defects. There is no automatic source failover or administrative kubeconfig discovery.

The userspace proxy reconciles both in-memory portals and actual rules in its own NAT/filter chains. Stale Service
incarnations and orphan rules are removed while active routes and replacement NodePorts remain intact. TCP setup is
bounded and concurrent; dial/handshake failures cannot indefinitely block the listener. Established connections retain
their normal lifetime.

### RuntimeService Data Path

Dayu-Edgemesh provides a revision-scoped data path for Sedna RuntimeServices. The managed projection reuses EdgeProxy's
Service/Endpoints informers, validates exact Service/RuntimeService/Pod UIDs, and fails closed until its portal is applied.
CoreDNS keeps its standard Kubernetes plugin with the same resolved endpoint and credentials. No hosts projection,
managed repair poller or persistent resource cache is added. An agent restarted offline cannot rebuild cloud metadata.
See the [architecture and API configuration](docs/managed-runtime.md) for the complete availability boundary.

Dayu EdgeMesh enables this path by default for Dayu v1.4:

```yaml
modules:
  edgeProxy:
    enable: true
    serviceFilterMode: FilterIfLabelExists
    managedRuntime:
      enable: true
```

The raw manifests and Helm chart enable this managed contract by default and
retain the `dayuhub/edgemesh-agent:v1.1` image default. When rolling out changes
from this branch, build and select a matching image and configure the reachable
API endpoint before installation:

```sh
helm upgrade --install edgemesh ./build/helm/edgemesh \
  --namespace kubeedge
```

For a private registry, also override
`agent.modules.edgeProxy.managedRuntime.image` or the image in
`build/agent/resources/05-daemonset.yaml`; the gate and filter mode do not need
to be edited.

The gate adds exact routing only for ClusterIP Services labelled
`dayu.io/mesh-managed=true`; unlabelled Services retain legacy proxy behavior.
This allows one v1.1 agent on each node to serve Dayu v1.3 JMES/NodePort traffic
and Dayu v1.4 RuntimeService traffic in different namespaces at the same time.
Both service types use the configured authoritative API and one proxy event stream. Disabling the managed gate
changes routing interpretation only; it does not switch data sources. Configure the API fields above before rollout,
and publish an image built from this source revision.

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
git clone https://github.com/dayu-autostreamer/dayu-edgemesh
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
