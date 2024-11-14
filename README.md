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

deploy crds
```bash
kubectl apply -f build/crds/istio/
kubectl apply -f build/agent/resources/
```

check edgemesh is running
```bash
kubectl get pods -n kubeedge
```

## How to Build 

clone repository
```bash
git clone https://github.com/dayu-autostreamer/dayu-edgemesh
```

Cross build edgemesh-agent and edgemesh-server image
```bash
make docker-cross-build
```
