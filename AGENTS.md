# OneImg 维护准则

适用于本仓库。保持改动范围清晰；详细命令与测试方法见[开发指南](docs/development.md)。

## 项目结构与工具链

- `backend/` 是 Go 服务，`frontend/` 是 Vue 界面；使用 Go 1.26.10、Node.js 22、pnpm 10，后端构建依赖 CGO。
- `api/openapi.yaml` 是公共契约，`frontend/src/api/generated/` 是生成客户端。
- Go 测试、`frontend/tests/security/`、`internal/securityfixture/` 和 `internal/imagebudgetprobe/` 分别承担后端、浏览器、临时服务及容器资源验证。

## 构建与生成规则

- 干净检出先安装前端锁定依赖并构建 `frontend/dist`，再执行依赖嵌入资源的 Go 检查或构建。
- API 变更同步后端、OpenAPI、生成客户端及回归测试；通过生成命令更新客户端，不手工编辑生成代码。
- 不依赖残留产物，不无故升级依赖或重写锁文件。

## 兼容性与安全边界

- 修改媒体 URL、权限、认证撤销、存储对象生命周期或计量时，先检查既有契约并补回归。
- 保留历史外链、资源归属与共享对象保护；不要以降级覆盖写、放宽权限或恢复旧凭据绕过失败。
- 安全收紧必须说明兼容性影响、部署要求和回滚限制；不将像素预算或普通样本测量宣传为完整安全保证。

## 操作与数据保护

- 测试使用临时目录、数据库、容器和经授权的隔离存储，不访问实际部署的数据目录或生产存储。
- 不读取、提交或输出真实密码、恢复码、PAT、应用密钥或存储凭据。
- 不擅自操作相邻插件仓库；涉及插件的改动需单独明确授权。
- 保留他人未提交改动；提交、推送、tag、发布及线上 Release 修改均遵循明确授权。

## 验证与汇报

- 按改动范围选择检查。纯文档 / 显示名称变更以链接、示例、工作流结构及 diff 检查为主，不重复资源压力测试。
- 分别报告本次通过、未运行和环境受限项目，不沿用历史测试结果冒充本次验证。
- 自动测试不代替真实远端存储和目标容器验收；缺少授权或环境时明确列出边界。

## 文档与发布约定

- 中文正文面向读者，用简短标题、操作步骤和必要警告，不堆内部任务过程或重复手册。
- README 仅做入口；`docs/deployment.md`、`usage.md`、`api.md`、`upgrading.md`、`development.md` 分别负责部署、使用、API、升级和开发。旧安全文档只做迁移导航，不新增重复索引。
- Actions 顶层名称使用 `Build`、`Security`；显示更名不扩展为流水线行为变更，徽章使用真实状态。
- GitHub Releases 是唯一的逐版本发布记录，仓库不维护正文副本或独立 CHANGELOG；长期兼容性与操作步骤写入升级指南。
- Release 标题为 `OneImg vX.Y.Z`；正文依次为“更新内容”“升级说明”“容器镜像”，最后是加粗的“完整变更”比较链接，不重复一级标题。
- 使用固定版本镜像示例和完整仓库文档链接。新指南在旧 tag 中不存在时链接到 `main`，先确认链接可访问，再按授权更新线上 Release 并回读核对。
- Release 简述用户影响，保留必要的备份、兼容性、凭证撤销、部署验证边界和回滚警告；详细步骤、参数与验证方法链接分类指南，不复述手册。不单列流水线、RSS、提交哈希或镜像摘要清单，不因格式调整移动 tag、重发镜像或改写此前风格一致的 Release。

## Agent skills

### Issue tracker

Issues live in GitHub Issues of `Xm798/oneimg` (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: root `GLOSSARY.md` + `docs/adr/`. See `docs/agents/domain.md`.
