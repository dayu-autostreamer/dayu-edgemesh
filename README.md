# Dayu-Edgemesh

## Brief Introduction
This project is based on [Edgemesh](https://github.com/kubeedge/edgemesh/) (v1.17.0)

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
git clone https://github.com/dayu-autostreamer/dayu-edgemesh
```

set meta information of building
```bash
# configure buildx buildkitd (default as empty, example at hack/resource/buildkitd_template.toml)
vim hack/resource/buildkitd.toml
# configure buildx driver-opt (default as empty, example at hack/resource/driver_opts_template.toml)
vim hack/resource/driver_opts.toml

# set docker meta info
# default REG is docker.io
# default REPO is dayuhub
# default TAG is v1.0
export REG=xxx
export REPO=xxx
export TAG=xxx
```

Cross build edgemesh-agent and edgemesh-server image
```bash
make docker-cross-build
```
