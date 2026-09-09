# OrbitOps

OrbitOps 是面向 Kubernetes 的应用交付控制平面。当前已经完成 S3 的后端运行时诊断闭环，并开始进入 S4。现有部署链路已经收敛为明确的 ReleaseOperation 领域：项目成员可以查询不可变发布历史，安全地重试、取消、重新检查和回滚；多个 Release Worker 在进程中断与结果未知时仍能保持目标队列有序，并从 Kubernetes 权威状态恢复或停止等待人工处理。S4 的 BuildOperation、Build Worker 和 OCI 制品链路尚未实现。

## 当前能力

- OpenAPI 3 定义 HTTP 契约，Go 与 TypeScript 代码均由契约生成或受其约束。
- Gin API 支持创建与查询 Project、Application、Deployment Target，并可幂等更新 Deployment Target 的可变期望配置。
- Project Member 提供 `owner`、`developer` 和 `viewer` 三类角色；创建者原子成为首个 owner，最后一个 owner 受到保护。
- 发布接纳在单个 PostgreSQL 事务中保存 Release、`pending` ReleaseOperation、幂等记录、审计记录和持久化调度意图。
- Release 历史支持稳定游标分页；详情同时展示不可变快照、当前目标差异、ReleaseOperation、ReleaseAttempt 和审计时间线。
- 普通失败可以显式重试，瞬时失败可以按预算自动退避；运行中或排队中的 ReleaseOperation 均可受控取消。
- 回滚从历史 Release 的完整快照创建新的 Release 与 ReleaseOperation，不修改旧发布或执行历史。
- 独立 Release Worker 使用数据库租约和完成围栏领取 ReleaseOperation，同一 Deployment Target 串行、不同目标可以并行，并为每次执行保留不可变 ReleaseAttempt。
- Release Worker 失联后先读取 Kubernetes 再决定补记成功、继续观察或重新 Apply；无法安全解释的结果进入 `attention_required`，不会盲目覆盖。
- Kubernetes 适配器只连接经过核验的本地 Kind context 和 OrbitOps 管理的 Namespace，使用固定 Field Manager 执行 Server-Side Apply。
- 运行时诊断只从 PostgreSQL 与 Kubernetes 权威来源组合证据；观测失败会明确返回 `partial` 或 `unavailable`，诊断信号保持确定性。
- Web 控制台保留 Project → Application → Deployment Target → Release 验证入口，并通过 `/api/v1/release-operations/{releaseOperationId}` 持续展示 ReleaseOperation 与 Kubernetes 观测。
- API 与 Release Worker 提供 JSON 日志、W3C Trace Context、Prometheus Metrics 和健康检查。

## 本地依赖

- Go 1.26+
- Node.js 26 与 Corepack
- Docker 与 Docker Compose
- Kind 0.32.0
- kubectl
- ripgrep

Kind 节点镜像和 PostgreSQL 镜像在仓库脚本与 Compose 文件中固定到 Digest。当前本地环境仍沿用 `kind-orbitops-s1` context 和 `orbitops-s1` Namespace 名称；它们是受控的测试边界，不代表当前产品阶段仍为 S1。启动脚本会把当前 kubeconfig context 切换到这个本地集群。

## 启动

首次准备环境：

```bash
make bootstrap
```

随后分别在三个终端启动：

```bash
make api
make release-worker
make web
```

打开 `http://127.0.0.1:5173`。API 默认监听 `127.0.0.1:8080`，Release Worker 健康与指标端口默认为 `127.0.0.1:9091`。

如果本机 Docker 使用非默认 Socket，在运行 Compose 或 Go 集成测试前显式设置 `DOCKER_HOST`。例如 OrbStack 可以使用：

```bash
export DOCKER_HOST=unix://$HOME/.orbstack/run/docker.sock
```

本地 PostgreSQL 数据保存在 Compose 命名卷中。`make db-down` 只停止并移除容器，不删除数据卷。

## 验证

生成并检查后端与 Web：

```bash
make check
```

运行 Go 测试与 Playwright Web 验收：

```bash
make test
```

真实 Kind 测试默认关闭，避免意外触碰非测试集群。先执行 `make kind-up`，再次确认当前 context 后运行：

```bash
make test-kind
```

Kind 验收覆盖成功发布、镜像拉取失败、外部资源归属冲突、失联后自动调和、运行中取消和回滚，以及完整的 API → PostgreSQL → Release Worker → Kubernetes → Diagnostics 路径。真实 PostgreSQL 集成测试另外覆盖多 Release Worker、目标级调度、自动与显式重试、权限、幂等、审计、历史查询和独立进程中断恢复。

## 运行接口

| 进程 | 健康检查 | Metrics |
| --- | --- | --- |
| API | `http://127.0.0.1:8080/healthz` | `http://127.0.0.1:8080/metrics` |
| Release Worker | `http://127.0.0.1:9091/healthz` | `http://127.0.0.1:9091/metrics` |

Metrics 覆盖 HTTP 请求、各 ReleaseOperation 状态、ReleaseDispatch 队列、自动与显式重试、租约接管、结果未知、稳定错误代码、授权拒绝、幂等冲突、执行阶段耗时和 Kubernetes 回读失败。Release 接纳时会把规范化的 `traceparent` 和 `tracestate` 持久化到 ReleaseOperation，Release Worker 领取后恢复同一条 Trace；结构化日志使用 `release_operation_id`、`release_attempt_id` 和 `release_worker_id` 等领域限定标识。

## 配置

所有运行配置通过环境变量提供，默认值面向本地开发环境：

| 变量 | 默认值 | 使用者 |
| --- | --- | --- |
| `ORBITOPS_DATABASE_URL` | `postgres://orbitops:orbitops@127.0.0.1:5432/orbitops?sslmode=disable` | API、Release Worker |
| `ORBITOPS_API_ADDRESS` | `127.0.0.1:8080` | API |
| `ORBITOPS_RELEASE_WORKER_ADDRESS` | `127.0.0.1:9091` | Release Worker |
| `ORBITOPS_ACTOR_ID` | `local-developer` | API |
| `ORBITOPS_MIGRATE_ON_BOOT` | `true` | API |
| `ORBITOPS_RELEASE_WORKER_ID` | 当前主机名 | Release Worker |
| `ORBITOPS_RELEASE_WORKER_POLL_INTERVAL` | `500ms` | Release Worker |
| `ORBITOPS_RELEASE_WORKER_LEASE_DURATION` | `10s` | Release Worker |
| `ORBITOPS_RELEASE_OPERATION_TIMEOUT` | `2m` | Release Worker |
| `ORBITOPS_RELEASE_MAX_AUTOMATIC_RETRIES` | `2` | Release Worker |
| `ORBITOPS_RELEASE_RETRY_BASE_DELAY` | `1s` | Release Worker |
| `ORBITOPS_REDIS_ADDRESS` | `127.0.0.1:6379` | Release Worker |
| `ORBITOPS_REDIS_USERNAME` | 空 | Release Worker |
| `ORBITOPS_REDIS_PASSWORD` | 空 | Release Worker |
| `ORBITOPS_REDIS_DB` | `0` | Release Worker |
| `ORBITOPS_RELEASE_QUEUE_NAME` | `orbitops-release` | Release Worker |
| `ORBITOPS_RELEASE_QUEUE_CONCURRENCY` | `4` | Release Worker |
| `ORBITOPS_RELEASE_QUEUE_REPAIR_INTERVAL` | `5s` | Release Worker |
| `ORBITOPS_RELEASE_QUEUE_CONSUMPTION_GRACE` | `30s` | Release Worker |
| `ORBITOPS_RELEASE_QUEUE_TASK_TIMEOUT` | `2m30s` | Release Worker |
| `ORBITOPS_RELEASE_QUEUE_SHUTDOWN_TIMEOUT` | `15s` | Release Worker |
| `ORBITOPS_KUBERNETES_CONTEXT` | `kind-orbitops-s1` | API、Release Worker |
| `ORBITOPS_CLUSTER_REF` | `kind-orbitops-s1` | API、Release Worker |
| `ORBITOPS_NAMESPACE` | `orbitops-s1` | API、Release Worker |
| `ORBITOPS_KUBECONFIG` | 当前用户默认 kubeconfig | API、Release Worker |
| `ORBITOPS_FIELD_MANAGER` | `orbitops-worker` | API、Release Worker |
| `ORBITOPS_KUBERNETES_POLL_INTERVAL` | `500ms` | API、Release Worker |

`ORBITOPS_FIELD_MANAGER` 的默认值暂时保留为 `orbitops-worker`，用于兼容已有 Kubernetes ManagedFields 所有权；它不是当前进程名称。旧的 `ORBITOPS_WORKER_*`、`ORBITOPS_OPERATION_TIMEOUT` 和 `ORBITOPS_QUEUE_*` 不会被新 Release Worker 静默读取。

API 不接受客户端提供操作者、Cluster Ref、Namespace、kubeconfig、Secret 或任意 Kubernetes Manifest。当前已实现基于本地 Actor 的项目角色授权，但身份仍由服务端 `ORBITOPS_ACTOR_ID` 明确配置；正式 OIDC、生产集群治理、Secret 管理、BuildOperation 和完整管理前端不属于当前范围。

## 目录

```text
api/              OpenAPI 契约与 Go 生成配置
cmd/              API、Release Worker 与离线 ReleaseDispatch 准备入口
internal/         领域模块、传输层、适配器与平台装配
scripts/          本地环境确定性脚本
test/integration/ 真实 PostgreSQL 的控制平面与 Release Worker 测试
test/kind/        受环境开关保护的真实 Kind 验收
web/              React 控制台、生成客户端与 Playwright 测试
```
