# Dayu-Edgemesh

## Brief Introduction
This project is based on [Edgemesh](https://github.com/kubeedge/edgemesh/) (v1.17.0)

We remove the  loadbalancer policy of edgemesh.

## Features

In the special version of edgemesh for dayu system, we remove the fixed loadbalance mechanism, which forwards requests across corresponding nodes.

We decide the forwarding in scheduler of dayu system, thus unexpected forwarding action is dangerous to dayu.

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
