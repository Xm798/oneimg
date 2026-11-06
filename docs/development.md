# 开发指南

本篇面向维护者。用户安装与配置见[部署指南](deployment.md)，部署者上线前检查见[升级指南](upgrading.md#部署环境验收)。工作约定见根目录 [AGENTS.md](../AGENTS.md)。

当前源码默认使用 SQLite，也支持 PostgreSQL 16、17、18。安装与迁移分别见[部署指南](deployment.md#数据库选择)和[升级指南](upgrading.md#sqlite-迁移到-postgresql)；[设计评估](postgresql-assessment.md)保留实施前的历史背景。

## 工具链与项目结构

使用 Go 1.26.10、Node.js 22、pnpm 10。Go 构建需要 CGO 和 C / C++ 编译工具链；生产 Dockerfile 另外安装 WebP 开发库，运行镜像提供对应运行库。优先复用 Dockerfile 排除宿主机工具链差异，不随文档整理升级依赖。

本地检查与构建前执行 `export GOTOOLCHAIN=go1.26.10`，并用 `go env GOVERSION` 确认实际版本。`go.mod` 声明的是最低版本，不会强制较新的本机工具链降级；例如 Go 1.27.1 仍受 [CVE-2026-111943](https://pkg.go.dev/vuln/GO-2026-6809) 影响，不能因版本号高于 1.26.10 就视为已修复。

| 路径 | 职责 |
| --- | --- |
| `main.go` | 应用入口，嵌入前端构建产物和 OpenAPI |
| `backend/` | Go 服务、API v1、认证、媒体处理与存储 |
| `frontend/src/` | Vue 页面、组件及 API 调用 |
| `api/openapi.yaml` | 公共 API 契约源文件 |
| `frontend/src/api/generated/` | 从契约生成的 TypeScript 客户端，不手工编辑 |
| `frontend/tests/security/` | Playwright 三浏览器安全回归 |
| `internal/securityfixture/` | 临时数据库与媒体的浏览器测试服务 |
| `internal/imagebudgetprobe/` | 隔离 Linux 容器中的正常图像内存探针 |
| `Dockerfile.security` | 复用生产构建与运行库的隔离验证镜像 |

## 构建与本地运行

干净检出必须**先构建前端，再执行 Go 检查或构建**。`main.go` 的 `go:embed` 依赖 `frontend/dist/**`，不能依赖开发机上残留的 `dist`：

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm run build
cd ..
```

不要直接在含真实 `data`、`uploads` 或应用 `.env` 的检出目录中运行服务。以下示例在未加载生产环境变量的终端中，从仓库根目录构建到临时运行目录，再切换目录启动：

```bash
RUN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/oneimg-dev.XXXXXX")
CGO_ENABLED=1 go build -o "${RUN_DIR}/oneimg" .
mkdir -p "${RUN_DIR}/frontend"
cp -R frontend/dist "${RUN_DIR}/frontend/dist"
printf '本次临时运行目录：%s\n' "${RUN_DIR}"

(
  cd "${RUN_DIR}"
  ONEIMG_ENV_FILE="${RUN_DIR}/data/.env" \
  SQLITE_PATH="${RUN_DIR}/data/data.db" \
  APP_URL=http://localhost:8080 \
  SERVER_PORT=8080 \
  ./oneimg
)
```

访问 `http://localhost:8080`，初始密码位于打印目录下的 `data/.initial_admin_password`。此目录仅供本地开发；退出后只清理本次创建的临时数据，不清理项目或其他部署目录。

需要前端热更新时，另开终端在 `frontend` 执行 `pnpm run dev`，访问 `http://localhost:3000`；Vite 将 `/api` 代理到 `localhost:8080`。若要验证 Passkey，应将开发后端 `APP_URL` 改为实际使用的 `http://localhost:3000` 后重启。完整媒体代理、视频和最终构建行为仍应通过后端 `8080` 的构建产物或隔离容器验证，不能只以 Vite 页面加载成功作为验收。

## 接口契约与客户端生成

修改 API 请求、响应、权限或错误语义时，同步修改后端、[OpenAPI](../api/openapi.yaml)、回归测试和使用说明。按下面的命令生成前端客户端：

```bash
cd frontend
pnpm run openapi:generate
pnpm run typecheck
cd ..
```

审阅并将必要的生成差异纳入变更，不手工修补生成文件。无契约漂移时可运行 `pnpm run openapi:check`；该命令会重新生成客户端，并用 Git diff 检查与已跟踪版本是否一致，不是只读解析器。执行前先了解工作区中是否存在尚未提交的契约 / 生成代码修改，不用还原命令覆盖他人改动。

## 自动检查

以下是维护者在相关代码变更后、发布前运行的自动检查顺序，不是某次发布已经通过的结果：

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm run openapi:check
pnpm run build
cd ..
gofmt -l $(git ls-files --cached --others --exclude-standard '*.go')
go test ./...
go test -race ./...
go vet ./...
cd frontend
pnpm exec playwright install --with-deps chromium firefox webkit
pnpm run test:security
cd ..
```

`gofmt -l` 无输出表示格式通过；有输出时对列出的文件运行 `gofmt -w` 后复查。`pnpm run build` 包含 TypeScript 类型检查；Playwright 会启动 `internal/securityfixture`，监听 `127.0.0.1:18089`，使用临时 SQLite / 媒体、固定测试账号，不复用已有服务。fixture 不是生产入口，不应映射到公网，也不会代替真实远端存储验收。

浏览器回归覆盖上传页 / 图库恶意历史元数据、预览操作、移动端和历史 SVG 主动内容阻断。存储、上传、去重、公开读取、Range、动画 WebP、视频、本地导入、认证撤销等后端行为由相关 Go 回归覆盖；仍需按本次改动补充缺失用例。

`Security` 先构建一次前端并检查 OpenAPI 契约，产物供后续任务共用；随后并行执行 `gofmt` / `go vet` / govulncheck、SQLite 竞态测试、PostgreSQL 16、17、18 全量后端矩阵（17 执行竞态），以及在 Playwright 官方镜像中按浏览器拆分、分别连接 SQLite 与 PostgreSQL 17 的三浏览器检查；浏览器任务使用预编译的 fixture，镜像标签由 CI 从锁文件读取的 Playwright 版本决定。只改 `docs/` 或 Markdown 的 PR 跳过测试与矩阵，推送和标签始终全量执行。`Build` 以 `Security` 为门禁，再在原生 amd64 / arm64 runner 上分别构建镜像并合并为多架构清单，标签构建同样执行门禁；本地验证不触发发布。它们不验证部署者的真实代理、生产存储或目标机器内存上界。

纯文档与工作流显示名称变更，可检查链接、命令与当前实现的一致性、工作流结构、变更范围和 `git diff --check`，无需访问真实存储或重复跑资源压力测试。其他改动按范围选择检查；报告时分别列明本次通过、未运行及环境受限项目，不引用旧结果冒充本次验证。

## 双数据库测试

`internal/testdb` 默认使用临时 SQLite；显式设置 `ONEIMG_TEST_POSTGRES_URL` 后，普通应用、服务、API、路由、本地导入和迁移测试使用 PostgreSQL。URL 为空、连接失败或初始化失败会让测试失败，不跳过或回退。每个测试创建随机 schema，每条连接固定 `search_path`，清理只删除自己创建的 schema；测试角色需要 `CREATE SCHEMA` 权限。旧 SQLite schema 升级测试继续使用临时 SQLite。

在独立 PostgreSQL 测试实例上设置 URL，禁止使用生产连接或生产角色：

```bash
# ONEIMG_TEST_POSTGRES_URL 通过测试专用环境文件或安全的交互方式设置
go test ./...
go test -race ./...
go vet ./...
cd frontend
pnpm run test:security
```

浏览器 fixture 继承同一 URL，仍只监听 loopback，并使用独立 schema、临时媒体与测试账号。分别在 PostgreSQL 16、17、18 执行完整后端检查，17 执行三浏览器和竞态；未设置 URL 的运行只证明 SQLite 行为。

表结构统一走 `database.MigrateSchema`，不要在业务测试另列 `AutoMigrate` 模型；显式 ID 种子写入后调用 `SyncSequences`。新增搜索原字段更新必须在同一 SQL 中维护派生字段，变更搜索规则必须增加回填版本。

`backend/database` 覆盖连接配置、TLS CA / 主机名、超时、脱敏、Passkey 索引、64 位 ID、序列与回填；`backend/services` 覆盖 Unicode、分页、时区边界和配额并发；`backend/utils/dbmigrate` 覆盖只读源、全字段验证、错误密钥 / 数据、复制与约束失败、事务回滚及提交结果不确定。TLS 测试使用临时 CA 的本地握手；实际部署的证书链和远端存储仍需单独验收。

## 无外网隔离的浏览器验收

需要 Docker，并预先获取生产构建依赖及与项目测试版本一致的 Playwright 基础镜像。**构建阶段可能需要网络；无外网限制针对下面的测试运行阶段。** 测试不挂载生产目录，不提供生产凭据，也不映射宿主端口。

从候选源码的仓库根目录执行，后续命令保持在同一 Shell 中。标签包含当前提交和本次时间，六个镜像都必须本次重建；若有未提交修改，也应一并记录，不能只用提交号描述源码：

```bash
CHECK_TAG="verify-$(git rev-parse --short=12 HEAD)-$(date -u +%Y%m%dT%H%M%SZ)"
RUNTIME_IMAGE="oneimg:runtime-${CHECK_TAG}"
BACKEND_IMAGE="oneimg:backend-${CHECK_TAG}"
FRONTEND_IMAGE="oneimg:frontend-${CHECK_TAG}"
FIXTURE_IMAGE="oneimg:fixture-${CHECK_TAG}"
BROWSER_IMAGE="oneimg:browsers-${CHECK_TAG}"
MEMORY_IMAGE="oneimg:memory-${CHECK_TAG}"

docker build -t "${RUNTIME_IMAGE}" .
docker build --target backend-builder -t "${BACKEND_IMAGE}" .
docker build --target frontend-builder -t "${FRONTEND_IMAGE}" .

docker build -f Dockerfile.security --target fixture \
  --build-arg BACKEND_IMAGE="${BACKEND_IMAGE}" \
  --build-arg FRONTEND_IMAGE="${FRONTEND_IMAGE}" \
  --build-arg RUNTIME_IMAGE="${RUNTIME_IMAGE}" \
  -t "${FIXTURE_IMAGE}" .
docker build -f Dockerfile.security --target browsers \
  --build-arg BACKEND_IMAGE="${BACKEND_IMAGE}" \
  --build-arg FRONTEND_IMAGE="${FRONTEND_IMAGE}" \
  --build-arg RUNTIME_IMAGE="${RUNTIME_IMAGE}" \
  -t "${BROWSER_IMAGE}" .
docker build -f Dockerfile.security --target memory-probe \
  --build-arg BACKEND_IMAGE="${BACKEND_IMAGE}" \
  --build-arg FRONTEND_IMAGE="${FRONTEND_IMAGE}" \
  --build-arg RUNTIME_IMAGE="${RUNTIME_IMAGE}" \
  -t "${MEMORY_IMAGE}" .
```

三个 build arguments 显式指向本次基础镜像，不依赖 `Dockerfile.security` 中旧的默认日期标签。不要在构建期间切换源码，也不要将这些临时验证镜像当作版本镜像推送。

确认容器名称未被占用后运行。浏览器与 fixture 共享无外网的网络命名空间，仅通过容器内 loopback 通信；fixture 启动未就绪或执行失败时，检查本次容器日志，不改用已有生产服务：

```bash
FIXTURE_CONTAINER="oneimg-fixture-${CHECK_TAG}"
docker run -d --name "${FIXTURE_CONTAINER}" \
  --network none --memory 512m "${FIXTURE_IMAGE}"

BROWSER_STATUS=0
docker run --rm --network "container:${FIXTURE_CONTAINER}" \
  --shm-size=1g --memory 2g "${BROWSER_IMAGE}" || BROWSER_STATUS=$?

docker logs "${FIXTURE_CONTAINER}"
docker stop "${FIXTURE_CONTAINER}"
docker rm "${FIXTURE_CONTAINER}"
test "${BROWSER_STATUS}" -eq 0
```

无论测试成功或失败，都只清理本次创建的容器；不要执行全局容器 / 镜像清理。测试报告和失败 trace 需要留存时，应从本次浏览器容器单独导出；上述 `--rm` 示例不会保留容器内报告。

## 目标容器内存探针

先完成上一节的本次候选镜像构建，并保留对应变量。探针只接受正常 24MP / 40MP JPEG，在生产 Linux 运行库中执行真实解码、主图处理和缩略图生成；需要 cgroup v2 的 `memory.peak`。探针固定使用 20 MiB 输入上限、WebP 转换、主图质量 85 和缩略图，不读取部署数据库中的处理设置；目标设置不同需另行验证。

以下 `2 GiB`、`2 CPU` 只是可重复的测试条件，不是生产配置建议；部署验收应替换为目标环境限制。确认本次容器名称未被占用后执行：

```bash
docker image inspect "${RUNTIME_IMAGE}" "${MEMORY_IMAGE}" --format '{{.Id}}'
MEMORY_STATUS=0
for MP in 24 40; do
  PROBE_CONTAINER="oneimg-memory-${MP}-${CHECK_TAG}"
  docker run --name "${PROBE_CONTAINER}" \
    --network none --memory 2g --cpus 2 \
    "${MEMORY_IMAGE}" -mp "${MP}" || MEMORY_STATUS=1
  docker inspect --format 'exit={{.State.ExitCode}} oom_killed={{.State.OOMKilled}}' \
    "${PROBE_CONTAINER}"
  docker rm "${PROBE_CONTAINER}"
done
test "${MEMORY_STATUS}" -eq 0
```

输出 JSON 包含输入 / 输出 / 缩略图字节数、耗时、峰值 RSS（KiB）、cgroup 峰值（字节）、Go 版本和架构。记录镜像 ID、源码状态、容器限制、退出码与 `OOMKilled`；缺少测量能力或运行失败时如实报告，不填入旧测量值。

正常样本的峰值不是最坏输入上界，也不能替代真实部署复测。当前像素和并发预算不是 native 解码硬中断、进程级内存隔离或完整 DoS 防护。

## 真实远端存储验证

自动测试使用临时数据和本地模拟，不宣称已覆盖所有真实 S3/R2/WebDAV/FTP 服务。经明确授权后，使用一次性隔离桶 / 目录验证：

- 新对象创建与读取、`HEAD` / Range，以及同 key 冲突后旧字节保持不变。
- 目标服务是否真正支持条件创建；FTP 写前列目录检查不是原子保证。
- 配额 / 数据库失败清理、未知网络结果的孤儿 key 核查、共享主图 / 缩略图引用及计量。
- 更换端点需要新凭据、连接测试授权、存储更新与上传 / 删除协调。

不使用生产桶、生产媒体、真实用户凭据或默认挂载目录做破坏性测试。目标服务暂不可用或未经授权时，列为未验证边界，而不是改用生产服务。

## 文档与发布检查

README 只做入口；部署、使用、API、升级、开发分别维护对应主题；旧[安全文档入口](security-hardening.md)仅保留导航，不再次复制正文。GitHub Releases 是唯一的逐版本发布记录，仓库不维护正文副本或独立 CHANGELOG；长期兼容性与操作步骤写入升级指南。

发布说明约定如下：

- 对外标题为 `OneImg vX.Y.Z`，正文不重复一级标题。
- 正文顺序固定为 `## 更新内容`、`## 升级说明`、`## 容器镜像`，末尾为加粗的“完整变更”比较链接。
- 中文面向用户描述影响，用少量条目归组列出修复与功能。升级说明保留必要的备份、兼容性、凭证撤销、安全部署和回滚警告；详细步骤、参数与验证方法链接分类指南，不复述手册。不写内部任务过程，不单列流水线 / RSS / 提交哈希 / 镜像摘要清单，不将未经验证的部署条件写成已经通过。
- 镜像使用固定版本拉取示例并说明架构。正文文档链接使用完整仓库 URL；对应 tag 没有的新指南链接到 `main`。
- 发布前审核拟发布正文；按授权更新线上标题和正文，回读核对更新内容、链接和必要警告，不为了风格统一重写更早且已一致的 Release。

交付时检查 Markdown 目标和标题锚点、文档导航、旧入口、命令、scope、配置来源、迁移范围与 `git diff --check`。Actions 保持短名称 `Build` / `Security`；仅为显示更名时不改文件名、job、触发器、权限或步骤，也不写死成功徽章。

提交、推送、tag、发布和线上 Release 编辑遵循明确授权。推送到 `main` 会按现有机制触发镜像构建，不为文档整理另改发布机制或手动补触发。新增指南先合并到 `main` 并确认可访问，再更新引用它们的 Release；最后核对真实徽章文字、运行状态与线上正文。
