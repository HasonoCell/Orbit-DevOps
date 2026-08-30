# OrbitOps

OrbitOps 是面向 Kubernetes 的应用交付控制平面。当前 S1 提供一条可在本地真实运行的已有镜像交付闭环：创建项目与应用、配置开发部署目标、接纳不可变镜像发布、由独立 Worker 调和 Kubernetes，并从 Kubernetes 权威回读运行状态。

## 当前能力

- OpenAPI 3 定义 HTTP 契约，Go 与 TypeScript 代码均由契约生成或受其约束。
- Gin API 支持创建与查询 Project、Application、Deployment Target，并可幂等更新 Deployment Target 的可变期望配置。
- 发布接纳在单个 PostgreSQL 事务中保存 Release、`pending` Operation、幂等记录和审计记录。
- 独立 Worker 使用数据库租约领取 Operation，保留每次 Attempt，并写入明确的成功或结构化失败终态。
- Kubernetes 适配器只连接经过核验的本地 Kind context 和 OrbitOps 管理的 Namespace，使用固定 Field Manager 执行 Server-Side Apply。
- Runtime Snapshot 只来自 Kubernetes；观测失败会明确返回 `unavailable`。
- Web 控制台可完成 Project → Application → Deployment Target → Release，并持续展示 Operation 和 Runtime Snapshot。
- API 与 Worker 提供 JSON 日志、W3C Trace Context、Prometheus Metrics 和健康检查。

## 本地依赖

- Go 1.26+
- Node.js 26 与 Corepack
- Docker 与 Docker Compose
- Kind 0.32.0
- kubectl
- ripgrep

Kind 节点镜像和 PostgreSQL 镜像在仓库脚本与 Compose 文件中固定到 Digest。S1 默认只允许 `kind-orbitops-s1` 和 `orbitops-s1` Namespace；启动脚本会把当前 kubeconfig context 切换到这个本地集群。

## 启动

首次准备环境：

```bash
make bootstrap
```

随后分别在三个终端启动：

```bash
make api
make worker
make web
```

打开 `http://127.0.0.1:5173`。API 默认监听 `127.0.0.1:8080`，Worker 健康与指标端口默认为 `127.0.0.1:9091`。

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

Kind 验收覆盖成功发布、镜像拉取失败、外部资源归属冲突，以及完整的 API → PostgreSQL → Worker → Kubernetes → Runtime Snapshot 路径。

## 运行接口

| 进程 | 健康检查 | Metrics |
| --- | --- | --- |
| API | `http://127.0.0.1:8080/healthz` | `http://127.0.0.1:8080/metrics` |
| Worker | `http://127.0.0.1:9091/healthz` | `http://127.0.0.1:9091/metrics` |

Metrics 覆盖 HTTP 请求、待处理 Operation、Operation 耗时与终态分类，以及 Kubernetes 回读失败。Release 接纳时会把规范化的 `traceparent` 和 `tracestate` 持久化到 Operation，Worker 领取后恢复同一条 Trace；结构化日志同时携带稳定资源标识和 Trace ID。

## 配置

所有运行配置通过环境变量提供，默认值面向本地 S1：

| 变量 | 默认值 | 使用者 |
| --- | --- | --- |
| `ORBITOPS_DATABASE_URL` | `postgres://orbitops:orbitops@127.0.0.1:5432/orbitops?sslmode=disable` | API、Worker |
| `ORBITOPS_API_ADDRESS` | `127.0.0.1:8080` | API |
| `ORBITOPS_WORKER_ADDRESS` | `127.0.0.1:9091` | Worker |
| `ORBITOPS_ACTOR_ID` | `local-developer` | API |
| `ORBITOPS_KUBERNETES_CONTEXT` | `kind-orbitops-s1` | API、Worker |
| `ORBITOPS_CLUSTER_REF` | `kind-orbitops-s1` | API、Worker |
| `ORBITOPS_NAMESPACE` | `orbitops-s1` | API、Worker |
| `ORBITOPS_WORKER_LEASE_DURATION` | `10s` | Worker |
| `ORBITOPS_OPERATION_TIMEOUT` | `2m` | Worker |

API 不接受客户端提供操作者、Cluster Ref、Namespace、kubeconfig、Secret 或任意 Kubernetes Manifest。S1 的身份是明确配置的本地开发身份；正式 OIDC、项目 RBAC、自动重试、取消和回滚属于后续切片。

## 目录

```text
api/              OpenAPI 契约与 Go 生成配置
cmd/              API、Worker 进程入口
internal/         领域模块、传输层、适配器与平台装配
scripts/          本地环境确定性脚本
test/integration/ 真实 PostgreSQL 的控制平面与 Worker 测试
test/kind/        受环境开关保护的真实 Kind 验收
web/              React 控制台、生成客户端与 Playwright 测试
```
