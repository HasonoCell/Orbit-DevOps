# Orbit-DevOps

Orbit-DevOps 是一个基于 Kubernetes 的应用持续交付平台，将代码仓库、镜像构建、发布和运行诊断串联在了一起，并提供一套管理界面来查看和管理交付过程。

## 项目架构

```mermaid
flowchart TB
    web["管理前端<br/>React · TypeScript"]
    github["GitHub"]

    subgraph orbit["Orbit 后端"]
        api["API<br/>登录授权 · 业务接口 · 运行诊断"]
        subgraph workers["异步执行进程"]
            pipeline["Pipeline Worker<br/>自动交付编排"]
            build["Build Worker<br/>镜像构建"]
            release["Release Worker<br/>发布与回退"]
            gateway["Gateway Worker<br/>路由与证书"]
        end
    end

    db[("PostgreSQL<br/>业务状态 · 执行记录<br/>Dispatch / Outbox · 审计")]
    queue[("Redis / Asynq<br/>任务队列")]
    registry["镜像 Registry"]

    subgraph cluster["Kubernetes 集群"]
        job["构建 Job<br/>Git 拉取 · BuildKit"]
        workload["应用运行<br/>Deployment · Service · Pod"]
        ingress["访问入口<br/>Gateway · HTTPRoute<br/>cert-manager · TLS Secret"]
    end

    web -->|HTTP API| api
    github -->|Webhook| api
    api <-->|读写| db
    workers <-->|读写 / 领取待投递任务| db
    workers <-->|投递 / 消费| queue
    pipeline -->|查询源码信息| github
    build -->|创建 / 观察| job
    release -->|部署 / 观察| workload
    gateway -->|同步 / 观察| ingress
    api -.->|只读诊断| workload
    api -.->|只读观测| ingress
    job -->|推送镜像| registry
    workload -->|拉取镜像| registry
    ingress -->|路由到 Service| workload
```

## 功能

- **项目与应用**：按项目组织应用，配置开发、生产部署目标，管理项目成员。
- **镜像构建**：指定 Git Commit 和 Dockerfile，在 Kubernetes Job 中运行 BuildKit，将镜像推送到 Registry，保存 Digest。
- **发布与回退**：选择构建产物或不可变镜像引用发布，查看执行记录，重试、取消或从历史发布回退。
- **自动交付**：接收 GitHub Webhook，通过 Pipeline 串起源码解析、构建和部署。
- **运行与入口**：查看 Deployment、Service、Pod 和事件，管理基于 Gateway API 的域名、路由和 TLS 证书。
- **账号与权限**：本地密码登录、OIDC 登录，平台管理员和项目级 owner / developer / viewer 权限，操作审计。

## 运行架构

一次自动交付的主要路径：

```text
GitHub push
    │
    ▼
Webhook → Pipeline → Build → 镜像制品 → Release → Kubernetes
                       │                   │
                  BuildKit Job   Deployment / Service
```

API 负责登录、授权、接纳请求和查询。耗时任务由四类独立 Worker 执行：

| 进程            | 职责                                  |
| --------------- | ------------------------------------- |
| Pipeline Worker | 处理 Webhook 和内部事件，推进自动交付 |
| Build Worker    | 创建和观察 BuildKit Job，记录镜像产物 |
| Release Worker  | 应用发布配置，观察部署结果            |
| Gateway Worker  | 同步 Gateway、HTTPRoute 和证书配置    |

PostgreSQL 保存业务状态、执行记录和待投递事件；Asynq / Redis 负责异步任务的排队和分发。各 Worker 同时负责自己这类任务的投递与消费。请求接纳后先写入数据库，再投递到队列；执行状态和 Kubernetes 观测可以在控制台分别查看。

## 技术栈

- 后端：Go、Gin、sqlx、PostgreSQL。
- 异步任务：Asynq、Redis。
- 集群与构建：client-go、Kubernetes、BuildKit、OCI Registry。
- 访问入口：Gateway API、cert-manager。
- 前端：React、TypeScript、Vite、TanStack Query、Tailwind CSS、shadcn/ui。
- 接口与观测：OpenAPI、OpenTelemetry、Prometheus。

## 本地运行

以下命令均在仓库根目录执行。

### 前端界面

需要 Node.js 24 和 Corepack，pnpm 版本由仓库的 `packageManager` 固定。

```bash
corepack pnpm install --frozen-lockfile --registry=https://registry.npmmirror.com
corepack pnpm demo
```

打开 <http://127.0.0.1:5173>。启动日志会显示 Demo 登录信息，场景控制页位于 <http://127.0.0.1:18090/__demo>，可以切换成功、失败和权限等场景。

这是带模拟 API 的界面 Demo，数据保存在内存中，不会连接数据库或集群。正式 `dev` / `build` 不加载这套模拟服务。

### 运行真实后端

还需要 Go 1.26+、Docker / Docker Compose、Kind、kubectl 和 ripgrep。Docker 需要配置可用的镜像源，Kind 镜像可以通过下面的变量从国内镜像拉取。

准备本地 Kind、Registry、PostgreSQL 和 Redis：

```bash
GOPROXY=https://goproxy.cn,direct go mod download
ORBIT_DEVOPS_NAMESPACE=orbit-devops-s3 ORBIT_DEVOPS_KIND_IMAGE_MIRROR_PREFIX=docker.m.daocloud.io npm_config_registry=https://registry.npmmirror.com make bootstrap
```

脚本使用 `kind-orbit-devops-s1` context，会切换当前 kubeconfig。应用 Namespace 使用 `orbit-devops-s3`，构建 Namespace 使用 `orbit-devops-s4-build`。

然后迁移数据库、初始化管理员。下面的连接串对应仓库 Compose 中的本地开发配置：

```bash
export ORBIT_DEVOPS_DATABASE_URL='postgres://orbitdevops:orbitdevops@127.0.0.1:5432/orbitdevops?sslmode=disable'
go run ./cmd/orbit-devops-migrate
go run ./cmd/orbit-devops-identity-admin initialize-admin --login-name admin --display-name Admin --maintenance-ref local-bootstrap
```

初始化命令会要求设置 Orbit 管理员的登录密码。密码至少 12 个字符，并需通过弱密码检查；同一个数据库只初始化一次。

Apple Silicon 上运行构建时，先在 API、Build Worker 和 Pipeline Worker 的终端一致设置 `ORBIT_DEVOPS_BUILD_PLATFORM=linux/arm64`；默认构建平台为 `linux/amd64`。

在不同终端分别运行下面各行：

```bash
ORBIT_DEVOPS_NAMESPACE=orbit-devops-s3 make api
ORBIT_DEVOPS_NAMESPACE=orbit-devops-s3 make release-worker
ORBIT_DEVOPS_BUILD_REGISTRY_INSECURE=true ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR=docker.m.daocloud.io go run ./cmd/orbit-devops-build-worker
make pipeline-worker
make web
```

前端默认在 <http://127.0.0.1:5173>，API 在 <http://127.0.0.1:8080>。登录后，先创建项目、应用和部署目标，再提交构建或发布。

入口管理还需要 Gateway API 控制器和 cert-manager。安装控制器，并为 API 和 Gateway Worker 配置 GatewayClass 和 Issuer 策略后，再运行 `ORBIT_DEVOPS_NAMESPACE=orbit-devops-s3 go run ./cmd/orbit-devops-gateway-worker`。

配置通过环境变量传入，具体变量和默认值见 [envconfig](internal/platform/envconfig/config.go)。本地 Vite 将 `/api` 代理到 API；需要修改代理地址时，设置 `ORBIT_DEVOPS_WEB_API_PROXY`。

停止本地数据库和 Redis 使用 `make db-down`，数据卷会保留。

## 集群部署

[deploy/private-k3s](deploy/private-k3s/) 提供个人单节点 K3s 部署样例，包括 API、四类 Worker、PostgreSQL、Redis、Registry 和访问控制。首次安装使用 [scripts/cloud](scripts/cloud/) 中的引导脚本，默认通过 SSH 隧道访问；公网前端可部署到 Vercel，通过服务端代理连接后端 HTTPS 入口。

已运行的平台使用独立维护命令，不要重新套用首次安装清单：

- [backup-control-plane.py](scripts/cloud/backup-control-plane.py)：在服务器受限目录备份数据库与运行配置，并在隔离数据库恢复验证。备份不离开服务器，也不包含 Registry 数据卷或 K3s 数据存储，不能替代整机灾难恢复。
- [manage-control-plane.py](scripts/cloud/manage-control-plane.py)：提供 `upgrade`、`rollback`、`status` 和 `accept`。升级前校验集群身份、已验证备份、schema 与镜像，关闭 API 接纳并等待任务排空；超时恢复入口，失败恢复旧镜像。只支持既有个人 amd64 K3s、同 schema 升级，不更新数据库或迁移 schema。
- [harden-ssh.py](scripts/cloud/harden-ssh.py)：确认 `ubuntu` 公钥登录可用后关闭 root 与密码 SSH；只新增受管配置，可独立撤销。
- [production-health.yaml](.github/workflows/production-health.yaml)：每半小时检查公网页面、数据库访问、匿名认证边界与源站证书。配置仓库变量 `ORBIT_PUBLIC_URL` 和 `ORBIT_ORIGIN_IP`，并开启 GitHub Actions 失败邮件通知；手动运行可测试通知。它不检查每个 Worker 或备份状态。

维护命令必须在目标服务器上由管理员执行。先查看各脚本 `--help`，核对 Cluster / Namespace UID；备份验证成功后，将检查点 ID 传给升级命令。五个进程就绪只记录为 `runtime_ready`；确认公网登录、权限和维护窗口 Webhook 补投，并完成一条新的自动构建与发布后，才运行 `accept`。

## 开发与测试

修改接口契约后，重新生成 Go 和 TypeScript 代码：

```bash
make generate
```

检查 Go、前端类型和构建：

```bash
make check
```

运行普通 Go 测试和 Web 验收。Web 验收需要先安装 Playwright Chromium，浏览器下载源可通过 `PLAYWRIGHT_DOWNLOAD_HOST` 配置：

```bash
corepack pnpm --filter @orbit-devops/web exec playwright install chromium
go test -p 1 -parallel 1 -timeout 20m ./... -count=1
corepack pnpm web:test:e2e
```

真实 Kind 构建、发布测试使用 `make test-kind`，会准备并操作本地测试集群。Gateway / TLS 测试需要另外准备控制器，并显式启用对应的环境开关。

## 目录

```text
api/                 OpenAPI 契约与生成配置
cmd/                 API、Worker、身份管理和迁移入口
internal/            领域模块、HTTP 适配层、Kubernetes 适配器与运行装配
deploy/private-k3s/  单节点 K3s 部署样例
scripts/             本地环境和云端部署脚本
test/                数据库集成测试、Kind 验收和 OIDC 测试配置
web/                 管理前端、模拟 API 与 Playwright 测试
```
