# 托管运行时（Managed Runtime）架构

## 目标与边界

托管运行时链路为 Dayu 的单个运行时 Pod 提供确定的、按版本隔离的路由，
并让业务进程彻底退出 Kubernetes 资源发现路径。EdgeMesh 复用现有的
Service/Endpoints informer，将 KubeEdge MetaServer 已同步到边缘节点的对象
投影为只读内存快照。

该设计有三个硬边界：

1. 默认不启用。原有 `JointMultiEdgeService`、NodePort 和未标记 Service 的
   行为保持不变。
2. 完全事件驱动。不新增 Kubernetes client、周期 List、同步强刷缓存、hosts
   文件或 EdgeMesh 持久化 checkpoint。
3. 托管对象失败关闭。资源不完整、身份不一致或代理尚未生效时，绝不会回退
   到旧的随机负载均衡链路。

KubeEdge MetaManager 仍是边缘侧 Service/Endpoints 的持久缓存，EdgeMesh
shared informer 仍是进程缓存；managed store 只负责验证并生成供数据面读取的
不可变投影，因此不存在第三份需要“强制刷新”的缓存真相。

## 兼容性开关

功能开关为 `modules.edgeProxy.managedRuntime.enable`，默认值是 `false`。

| 开关 | 托管 Service | 旧 Service / JMES / NodePort | 校验来源 |
|---|---|---|---|
| `false` | 不识别 | 完全保持原行为 | 原有独立校验，失败后回退 |
| `true` | 精确路由、失败关闭 | 保持原代理语义 | MetaServer 支撑的主 informer |

启用时必须同时启用 EdgeProxy，并设置
`serviceFilterMode: FilterIfLabelExists`；配置校验会拒绝其他组合。需要回滚时，
只需将 `managedRuntime.enable` 设回 `false`，旧 CRD 和代理实现均未被删除。

使用 Helm 时，启用操作同时也是一个 fail-fast 镜像配置：开关为 `true` 时必须
显式提供 `agent.modules.edgeProxy.managedRuntime.image`，且该 agent 镜像应由与
Chart 相同的源码 revision 构建。这样不会把新 ConfigMap 静默配给默认的上游旧
二进制。使用原始 YAML 时，也必须先替换 agent DaemonSet 镜像再打开开关。

## 数据与确认流程

```text
Sedna RuntimeService controller
  -> 单副本、固定目标节点的 Deployment
  -> 单端口 ClusterIP Service
  -> Kubernetes Endpoints controller
  -> KubeEdge MetaManager / MetaServer
  -> EdgeMesh 现有 Service + Endpoints informer
  -> managed 内存投影
  -> userspace portal 生命周期（PENDING -> APPLIED -> REMOVING -> REMOVED）
  -> 本机状态 API
  -> Sedna local-controller 激活确认
```

informer handler 不执行磁盘 I/O；数据面从原子快照取路由，不发起 Kubernetes
API 请求。

userspace proxier 会在尝试写入托管 portal 前先发出 `PENDING`。该事件携带精确
Service UID，并立即将 namespaced key 标记为 managed；即使它早于投影侧的
Service handler 到达，路由也会从第一刻起失败关闭。只有 portal 写入和负载均衡
注册均成功后才发出 `APPLIED`。拆除前先进入 `REMOVING` 并保留 managed
tombstone；只有当前 Service 实例的 portal 和 socket 全部清除后才进入
`REMOVED`。最终删除还会清除旧负载均衡状态；同端口替换则保留共享 endpoint
缓存，避免 Endpoints 对象未变化时旧链路丢失后端。初始化和拆除失败统一重新并入
proxier 现有的限频同步循环，并始终只协调最新期望 Service UID。因此必须同时满足精确投影、
source 为 `SYNCED`、且相同身份已 `APPLIED`，路由才能打开。

## 资源契约

只有带 `dayu.io/mesh-managed=true` 标签的 Service 才进入新链路。Service
必须是非 headless `ClusterIP`，只暴露一个有名称的 TCP 端口，`targetPort`
必须是正整数且 Service `port` 必须与之相等，同时不能分配 NodePort。Endpoints 必须恰好包含一个 subset、一个
匹配端口、一个 ready address，且不能有 not-ready address。唯一地址必须：

- IP 合法；
- `nodeName` 等于 Service 的 `dayu.io/target-node`；
- 通过 kind、name、非空 UID 和 Service namespace 精确引用一个 Pod。

Service 与 Endpoints 必须具有完全一致的身份标签：

| 标签 | 含义 |
|---|---|
| `dayu.io/mesh-managed=true` | 显式选择新链路 |
| `dayu.io/install-id` | Dayu 安装实例身份 |
| `dayu.io/deployment-revision` | 正的十进制 `int64` 版本号 |
| `dayu.io/runtime-id` | RuntimeService 身份，必须与 Service 名称一致 |
| `dayu.io/component` | Dayu 组件角色 |
| `dayu.io/runtime-service-uid` | 精确 RuntimeService UID |

Service annotation 包括：

| Annotation | 要求 |
|---|---|
| `dayu.io/target-node` | 必填，必须与 endpoint `nodeName` 一致 |
| `dayu.io/logical-service` | 可选的 Dayu 目录元数据 |

身份校验不依赖 Endpoints annotation：Kubernetes Endpoints controller 会复制
Service label，但不保证复制 annotation，所以完整 annotation 只从 Service
读取。

最终 FQDN 固定为 `<service>.<namespace>.svc.cluster.local`。标准 Service DNS
是唯一 DNS 真相，EdgeMesh 不维护并行 hosts 数据库。

## 状态机与陈旧事件保护

路由状态只有三种：

- `READY`：资源契约已通过，但代理生效或数据源同步尚未确认；
- `APPLIED`：当前 informer 身份、userspace portal 和数据源同步完全一致；
- `DEGRADED`：当前 Service/Endpoints 契约不成立，或已观察到的必要对象消失。

只有 `APPLIED` 且 source 为 `SYNCED` 时数据面才会选择该路由。Service UID、
RuntimeService UID 和 endpoint Pod UID 共同区分对象实例并防止 ABA 复用。
Update 事件以 namespaced key 上的新对象为准；Delete 事件按 UID 防护，陈旧
tombstone 不会删除新路由。portal 回调同样携带精确 Service UID。

代理侧 `PENDING` 和 `REMOVING` 都会让路由退出 `APPLIED`；只有 `REMOVED`
才能释放 tombstone。这些状态不会因 informer handler 的先后顺序而短暂误开。

`SYNCED` 只表示主 Service/Endpoints informer 已完成首次 cache 同步，刻意不把
它描述为持续的 MetaServer 连接健康信号。

`APPLIED` 是发布/滚动升级屏障，不是持续的业务健康度。运行时健康仍由 Dayu
健康检查负责。

## 本机状态 API

API 只监听 `127.0.0.1:10551`。EdgeMesh 和 Sedna local-controller 使用
hostNetwork，因此无需额外暴露 Kubernetes Service。

| 接口 | 含义 |
|---|---|
| `GET /healthz` | 进程存活状态 |
| `GET /readyz` | 主 Service/Endpoints cache 完成同步后才返回 `200` |
| `GET /v1/routes` | 所有投影路由的诊断快照 |
| `GET /v1/routes/{serviceUID}` | 按精确 Service UID 查询激活状态 |

精确查询响应包含 Service UID、RuntimeService UID、endpoint Pod UID、deployment
revision、runtime ID、target node、本地序列号和 source 状态。未知 UID 返回
`404`；任一状态未达到 `APPLIED + SYNCED` 返回 `503`；只有身份与状态均精确
匹配时返回 `200`。调用方仍必须逐一比较身份字段，不能把其他 revision 或 Pod
UID 的 `200` 当作本版本的激活确认。

## 启用与回滚

1. 先部署同时包含 RuntimeService 支持的 Sedna 与 EdgeMesh 版本，但保持
   `managedRuntime.enable: false`。
2. 验证原有 JMES/NodePort 流量无变化。
3. 设置 `serviceFilterMode: FilterIfLabelExists` 和
   `managedRuntime.enable: true`，提供同 revision 的 agent 镜像并滚动更新
   EdgeMesh。Helm 命令如下：

   ```sh
   helm upgrade --install edgemesh ./build/helm/edgemesh \
     --namespace kubeedge \
     --set agent.modules.edgeProxy.serviceFilterMode=FilterIfLabelExists \
     --set agent.modules.edgeProxy.managedRuntime.enable=true \
     --set-string agent.modules.edgeProxy.managedRuntime.image=dayuhub/edgemesh-agent:v1.1
   ```
4. 在每个节点确认 `curl -fsS http://127.0.0.1:10551/readyz` 成功。
5. 创建新的 RuntimeService revision；只有精确状态查询返回 `200` 后，才将其
   发布到 Dayu runtime directory。
6. 回滚时将开关恢复为 `false`，旧资源无需修改。

不要原地修改已发布 revision。应创建新 revision，等待精确激活，发布新目录
记录，最后回收旧 revision。这样才能保持 Service/Pod UID 屏障的有效性，避免
半完成的路由切换。
