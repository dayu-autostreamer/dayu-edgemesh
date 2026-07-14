# EdgeMesh app

Visit https://edgemesh.netlify.app/reference/config-items.html#helm-configuration for more configuration information.

访问 https://edgemesh.netlify.app/zh/reference/config-items.html#helm-配置 以了解更多的配置信息。

## Install

```
helm install edgemesh --namespace kubeedge \
--set agent.psk=<your psk string> \
--set agent.relayNodes[0].nodeName=<your node name>,agent.relayNodes[0].advertiseAddress=<your advertise address list> \
https://raw.githubusercontent.com/kubeedge/edgemesh/main/build/helm/edgemesh.tgz
```

**Install examples:**

You need to generate a PSK cipher first, please refer to: https://edgemesh.netlify.app/guide/security.html

Start with a relay node:
```
helm install edgemesh --namespace kubeedge \
--set agent.psk=<your psk string> \
--set agent.relayNodes[0].nodeName=k8s-master,agent.relayNodes[0].advertiseAddress="{1.1.1.1}" \
https://raw.githubusercontent.com/kubeedge/edgemesh/main/build/helm/edgemesh.tgz
```

Start with two relay nodes:
```
helm install edgemesh --namespace kubeedge \
--set agent.psk=<your psk string> \
--set agent.relayNodes[0].nodeName=k8s-master,agent.relayNodes[0].advertiseAddress="{1.1.1.1}" \
--set agent.relayNodes[1].nodeName=ke-edge1,agent.relayNodes[1].advertiseAddress="{2.2.2.2,3.3.3.3}" \
https://raw.githubusercontent.com/kubeedge/edgemesh/main/build/helm/edgemesh.tgz
```

## Uninstall

```
helm uninstall edgemesh -n kubeedge
```

## Dayu managed runtime profile

The chart enables Dayu managed runtime by default and pairs it with the
same-revision `dayuhub/edgemesh-agent:v1.1` image. A normal Dayu v1.4 install
therefore needs no feature-specific values:

```
helm upgrade --install edgemesh ./build/helm/edgemesh \
--namespace kubeedge
```

Override `agent.modules.edgeProxy.managedRuntime.image` only for a private
registry. Unlabelled Dayu v1.3 JMES/NodePort Services remain on the legacy data
path while labelled Dayu v1.4 RuntimeServices use managed routing. Set
`agent.modules.edgeProxy.managedRuntime.enable=false` only to roll back to the
pre-managed validation behavior.
