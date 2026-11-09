# 托管运行时（Managed Runtime）架构

## 目标与边界

EdgeMesh 为 Dayu 提供精确、按 revision 隔离的路由，业务进程不访问 Kubernetes。
DNS 与代理统一使用解析后的 `kubeAPIConfig`；托管和旧 Service 共享代理的 Service/Endpoints informer，
managed store 将事件验证为不可变内存快照。身份不完整或不一致的托管路由保持失败关闭，不回退随机转发。

Dayu 应通过现有 `master` / `kubeConfig` 明确选择所有 agent 均可直达的真实 Kubernetes API。
不再自动猜测私有地址、查找管理员 kubeconfig，或为旧/托管对象维护不同数据源；不再投影 hosts 或定时点查修复。

## API 配置与可用性

```yaml
kubeAPIConfig:
  master: https://api.example.com:6443
  # 可选：挂载认证配置；master 覆盖文件内的 server。
  # kubeConfig: /etc/edgemesh/api.kubeconfig
```

只指定 master 时，有可用的 in-cluster ServiceAccount 凭据就继续使用它；否则提供挂载的 kubeconfig。
复用 agent 原有的 Service、Endpoints、namespace、Pod 及启用的 Istio 资源只读权限，不增加 Dayu 业务 Pod 权限。
API 地址应直接可达并使用有效 CA/服务端身份，避免通过依赖 EdgeMesh 自身的 Service IP 引导连接。
Helm 对应 `agent.kubeAPIConfig`，无需增加新的选源开关。

CoreDNS 从相同的已解析配置生成权限为 0600 的 `dns-kubeconfig`，保留 master 覆盖、TLS 与 token 文件路径，
支持 token 轮转。该文件只保存连接配置，不是资源缓存。CoreDNS 保持标准 Kubernetes 插件及自己的 informer；
正缓存 TTL 仍为 30 秒，负缓存仍限制为 3 秒。代理与 managed 投影共享另一套 informer 事件流。
Watch 客户端不再继承短的全局 HTTP 超时，续订和重新 LIST 由 client-go 管理。
代理的 Service UID/端口校验只依赖同一 Service informer，不用独立 Namespace watch 的到达顺序否定合法 Service。
DNS 与代理是同源、最终一致的独立缓存；解析成功不代表精确路由已经 APPLIED。

未指定 master/kubeConfig 时，上游自动 edge 模式仍会选择 MetaServer。这个保留的配置模式不能绕过
KubeEdge 缓存问题；需要本次恢复能力的 Dayu 部署必须选择真实 API。运行中 API 失联不会自动切换元数据来源。
已有内存缓存继续服务，API 错误不视为删除；连接恢复后由 WATCH/LIST 补齐更新和删除，已 APPLIED 的对象同样会回收。
离线重启 agent 无法从云端重建缓存，本方案不增加离线持久化，也不保证离线冷启动。

## 兼容性开关

`modules.edgeProxy.managedRuntime.enable` 在 Dayu 发行版中默认开启，并要求启用 EdgeProxy 和
`serviceFilterMode: FilterIfLabelExists`。关闭时仍可使用旧转发语义；开启时带标签的对象遵守精确身份契约。
两种情况下均使用同一个配置的数据源。

## Dayu v1.3 与 v1.4 共存

每个节点只运行一个 agent。未标记的 JMES/NodePort Service 保留旧转发方式；
`dayu.io/mesh-managed=true` 的 RuntimeService 使用精确路由。标签决定路由语义，不再决定元数据来源。
不同 Dayu namespace 共享代理；旧对象清理不会释放新对象已经接管的 NodePort。

## 数据与确认流程

```text
Kubernetes Service + Endpoints
  -> 共享 EdgeProxy informer（LIST/WATCH）
  -> managed 身份投影 + userspace proxy
  -> PENDING / APPLIED / REMOVING / REMOVED 回调
  -> 本机状态 API
  -> Sedna local-controller 激活确认
```

handler 不执行文件 I/O，数据面不访问 API。PENDING 在安装 portal 前标记精确 Service UID；
只有 portal 和负载均衡注册成功才进入 APPLIED。REMOVING 先关闭路由，精确 portal/socket 清除后进入 REMOVED。
最终删除清理旧 endpoint 状态，同端口替换保留共享 endpoint 缓存。安装/拆除失败继续进入既有的限频协调循环。

协调还检查内核中 EdgeMesh 自有五条 NAT/filter 链，按当前 portal 删除内存里已经没有的孤儿规则、
复用名称/NodePort 下指向旧代理端口的规则。先完成基础链与孤儿规则协调，再消费新 Service 变更和报告 APPLIED；
初始化、读取或清理失败时保留待处理变更，由原协调循环重试。保留活跃规则和新对象端口占用，不清空主机规则或其他组件的链。

TCP 接收与连接建立分开，每个 listener 最多同时建立 32 条连接；已建立连接不占该额度。
原有 endpoint 重试预算用于约束真实 Dial、libp2p stream 建立和握手，接收端同样限制握手与本地拨号。
失败关闭/reset 所有已取得的连接；成功清除建立阶段的 deadline，保留 Redis 和任务传输长连接。

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
当当前值和传入值均为规范十进制整数时，Service 或 Endpoints 中较旧的 Kubernetes
`resourceVersion` 会在覆盖当前投影前被忽略；较新的无效更新仍然保持
fail-closed。Delete 事件按 UID 防护，陈旧 tombstone 不会删除新路由。portal
回调同样携带精确 Service UID。

SYNCED 仅代表首次 informer 同步完成，不代表持续 API 可达；APPLIED 是发布确认，不是应用实时健康。
运行时健康仍由 Dayu 检查。

## 本机状态 API

API 只监听 `127.0.0.1:10551`。EdgeMesh 和 Sedna local-controller 使用
hostNetwork，因此无需额外暴露 Kubernetes Service。

| 接口 | 含义 |
|---|---|
| `GET /healthz` | 进程存活状态 |
| `GET /readyz` | Service/Endpoints cache 完成同步后才返回 `200` |
| `GET /v1/routes` | 所有投影路由的诊断快照 |
| `GET /v1/routes/{serviceUID}` | 按精确 Service UID 查询激活状态 |

精确查询响应包含 Service UID、RuntimeService UID、当前观察到的 Service 和
Endpoints resourceVersion、endpoint Pod UID、deployment revision、runtime ID、
target node、本地序列号和 source 状态。两个 resourceVersion 可直接与配置的权威 API 对象对比，不需要再通过修改 annotation 人为触发事件。
未知 UID 返回
`404`；任一状态未达到 `APPLIED + SYNCED` 返回 `503`；只有身份与状态均精确
匹配时返回 `200`。调用方仍必须逐一比较身份字段，不能把其他 revision 或 Pod
UID 的 `200` 当作本版本的激活确认。

## 启用与回滚

配置真实 API 并编译发布匹配的 agent 镜像后再部署；仓库修改不会更新已存在的 v1.1 镜像。
原始清单与 Helm values 给出配置字段，不写死私有集群地址。逐节点验证 readyz、精确 UID 激活响应、
DNS 和真实 Service 请求；缓存同步成功不能替代端到端验证。

关闭 managedRuntime.enable 只关闭托管解释，不改变 API 来源。不要原地修改已发布 revision；
创建新 revision、等待精确激活、发布目录后再回收旧版本。

## 回归验证

使用兼容当前 vendored 依赖的 Go 1.20，在 Linux 执行：

```sh
go test -mod=vendor ./pkg/clients ./pkg/dns ./pkg/meshstate ./pkg/proxy ./pkg/loadbalancer ./pkg/tunnel
go test -mod=vendor k8s.io/kubernetes/pkg/proxy/userspace
```

测试使用假 API/iptables、本地 socket 和内存 stream，覆盖失联与删除、APPLIED 路由重新 LIST、旧 UID、
内核孤儿规则、NodePort 复用、建连并发与握手超时。不启动 agent、不修改主机规则，也不替代真实集群验证。
