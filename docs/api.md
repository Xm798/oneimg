# API 指南

API 基础路径为 `/api/v1`。外部客户端使用个人访问令牌（PAT），不要复用旧版全局 API Token，也不要把管理员密码写入集成脚本。

## 创建个人访问令牌

1. 在浏览器登录，先完成初始改密或账号恢复。
2. 打开“账户设置 → API 令牌”，创建命名明确的令牌。
3. 按用途选择最小 scope 和有效期：30、90、365 天或永不过期。
4. 创建成功后立即安全保存明文，界面与创建接口仅显示一次，之后不能再次读取。

下面的列表和上传示例使用同一枚 PAT，因此需要同时勾选 **`images:read` 和 `images:write`**。仅查看列表时只需 `images:read`；上传、读取上传选项或 URL 导入必须有 `images:write`，只读令牌不能用于上传。

PAT 使用 `Authorization: Bearer <token>` 请求头，不放在 URL、截图、公开日志或源码中。scope 只是令牌上限，实际操作仍受所属用户当前角色、功能权限、存储桶授权和资源所有权约束。用户降权、删除及凭证撤销会影响已有令牌。

### 权限范围

| scope | 用途 |
| --- | --- |
| `images:read` | 查询媒体列表和详情 |
| `images:write` | 上传、URL 导入、读取上传选项、修改文件标签 |
| `images:delete` | 删除媒体 |
| `tags:read` / `tags:write` | 查询 / 管理标签 |
| `storage:read` / `storage:write` | 查询 / 管理存储桶、连接测试 |
| `users:read` / `users:write` | 查询 / 管理用户 |
| `settings:read` / `settings:write` | 查询 / 修改系统设置 |
| `stats:read` | 读取统计 |

例如已有存储连接测试除了 `storage:write`，还要求用户具备 `storage:update` 权限；scope 不能绕过这一检查。

## 常见请求

以下命令使用 Bash。将地址换成自己的部署，通过交互读取 PAT，避免将明文写入命令历史；不会在本地保存令牌文件：

```bash
ONEIMG_URL='https://oneimg.example.com'
read -r -s -p 'PAT: ' ONEIMG_TOKEN
printf '\n'
```

### 查询列表与上传限制

```bash
curl -sS --get "${ONEIMG_URL}/api/v1/images" \
  -H "Authorization: Bearer ${ONEIMG_TOKEN}" \
  --data-urlencode 'page=1' \
  --data-urlencode 'page_size=20' \
  --data-urlencode 'media_type=image'

curl -sS "${ONEIMG_URL}/api/v1/upload-options" \
  -H "Authorization: Bearer ${ONEIMG_TOKEN}"
```

列表可用 `media_type=image|video` 筛选，不传则返回全部。还支持 `q`、`bucket_id`、逗号分隔的 `tag_ids`、`untagged`、`sort` 和 `order`；`uploader_role` 仅供管理员筛选。页码从 1 开始，`page_size` 默认 20、最大 100。

`upload-options` 返回当前允许的 MIME 类型、图片 / 视频分类大小上限、每批数量及可用存储桶。`allowed_types` 设置项只配置图片类型，视频是否可用由 `video_upload_enabled` 决定。

`GET /api/v1/image-storages`（需要 `images:read`）返回当前用户可见媒体实际所在的存储 `id`、`name`、`type` 和 `disabled`，管理员包含全部媒体。已停用、容量已满或无上传权限的存储同样返回，适合显示存储名称和填充 `bucket_id` 筛选；选择上传目标仍应使用 `upload-options`。

S3/R2 存储的直链域名是存储配置的 `config.public_domain`。`PATCH /api/v1/storage-buckets/{id}` 的 `config` 只需包含要修改的字段，只改域名不需要重新提供凭证，传空字符串表示改回经 OneImg 访问；格式错误或用于其他存储类型时返回 `422`，`errors[].field` 为 `config.public_domain`。系统设置已不再接受 `public_image_domain`，见[升级指南](upgrading.md#升级至-v430-的直链域名)。

`PATCH /api/v1/settings` 只需包含要修改的字段，所有字段在一个事务中保存：任一字段无效时全部不保存，返回 `422`，`errors[].field` 是出错的字段名（与请求体一致，如 `main_image_quality`），`errors[].code` 一般为 `invalid`，默认存储已停用时为 `storage_disabled`。

`GET /api/v1/tags`（需要 `tags:read`）返回全部标签的 `id`、`name` 和 `count`。`count` 是标签在当前用户可见文件中的使用次数：管理员统计全部文件，普通用户只统计自己的文件，未使用的标签为 0。

### 搜索与排序

`GET /api/v1/images?q=...` 搜索文件名、原始文件名、原始 URL 和内容哈希；`GET /api/v1/users?username=...` 搜索用户名。两种数据库均先去除查询首尾空白，再用 `NFC(UnicodeCaseFold(NFC(value)))` 做包含匹配：

- `STRASSE` 可匹配 `Straße`，`Σ` 可匹配 `σ` 和 `ς`。
- 组合形式与预组合形式的重音字符一致，但 `e` 与 `é`、半角与全角不合并。
- `%` 匹配任意长度，`_` 匹配归一化后的一个字符；反斜杠是字面值，不是转义符。
- 搜索字段仅内部使用，不出现在响应；展示原文、用户名登录 / 唯一约束、Passkey 名称规则和媒体原始路径保持既有语义。

图库排序 `sort` 允许 `created_at`、`file_size`、`filename`、`id`，`order` 为 `ASC` / `DESC`；无效参数返回查询参数错误。每种排序追加同方向 ID 次排序，静态数据中相同时间、大小、文件名的翻页顺序稳定。文件名按二进制顺序；时间按实际时刻，空时间在升序最前、降序最后。分页过程中有新增或删除时，页码分页仍可能发生位置变化。用户列表按 `username` 排序同样使用二进制顺序，不随 PostgreSQL 数据库的区域设置变化。

统计接口 `GET /api/v1/stats/dashboard` 和 `GET /api/v1/stats/images` 查询数据库失败返回 HTTP `500`、问题代码 `stats_read_failed`，不会以全零成功响应掩盖失败。时间桶按应用时区划分。

存储请求 `capacity_bytes` 允许 `0` 到 `9223372036854775807`，`0` 表示无限容量；超过上限返回 `422` 容量校验错误。文件字节数和计量同受非负 `int64` 上限约束，无限容量仍不能突破数值上限。

### 上传图片或视频

先确认 PAT 有 `images:write`，并将本地文件名替换为实际存在的文件：

```bash
curl -sS -X POST "${ONEIMG_URL}/api/v1/images" \
  -H "Authorization: Bearer ${ONEIMG_TOKEN}" \
  -F 'images=@photo.jpg' \
  -F 'images=@second-photo.webp'
```

每批最多 10 个文件，重复使用 multipart `images` 字段；可用 `-F 'bucket_id=1'` 指定有权使用的桶。需要标签时，先创建或查询现有标签，再重复传入 `-F 'tag_ids=2' -F 'tag_ids=3'`，不要使用不存在的示例 ID。

启用视频后，同一接口和 `images` 字段也接受 MP4 / WebM，例如 `-F 'images=@clip.mp4'`。不是另设 `/videos` 接口，视频大小与兼容性规则见[使用指南](usage.md#视频上传与播放)。

**批量上传返回 HTTP 200 也可能包含文件失败。** 必须检查 `data.files` 中每项的 `success`、`error`，以及 `data.summary` 的 `total`、`succeeded`、`failed`；`duplicate` 表示复用已有记录，成功项通过 `image` 返回媒体信息。

### 从 URL 导入图片

此接口要求 `images:write`，仅接受允许访问的 HTTP / HTTPS 图片 URL，不用于导入视频：

```bash
curl -sS -X POST "${ONEIMG_URL}/api/v1/image-imports" \
  -H "Authorization: Bearer ${ONEIMG_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://images.example.com/photo.jpg"}'
```

示例域名需要替换为实际图片来源。服务端先检查 URL、目标存储和标签，再下载远端图片；下载不占用图像处理槽。导入专有的错误：

| 状态与代码 | 含义与处理 |
| --- | --- |
| `403` `remote_address_forbidden` | URL 或其解析结果指向内网、保留或特殊用途地址，改用公网地址 |
| `422` `invalid_import_url` / `remote_image_too_large` / `remote_content_not_image` | URL 无效、超过单文件大小限制或内容不是图片，重试不会成功 |
| `429` `import_concurrency_exceeded` | 同一用户最多同时进行 2 个导入；等待当前导入完成或按 `Retry-After` 重试 |
| `503` `import_busy` | 全站导入容量已满，按 `Retry-After` 重试 |
| `503` `image_processing_busy` | 下载完成但图像处理繁忙，按 `Retry-After` 重试 |
| `504` `remote_image_too_slow` | 远端停滞超过 10 秒、速度过低或 60 秒内无法完成 |
| `502` `remote_image_download_failed` | 远端返回错误状态、连接失败或 DNS 解析失败 |

完成调用后可清除当前 Shell 中的令牌变量：

```bash
unset ONEIMG_TOKEN
```

## 响应与错误处理

大多数成功响应以 `data` 包装结果；分页列表另有 `meta.pagination`，包含 `page`、`page_size`、`total`、`total_pages`。删除、撤销等接口可能返回 `204` 且无正文。

请求级错误使用 `application/problem+json`。例如错误的媒体类型筛选：

```json
{
  "type": "urn:oneimg:problem:invalid_query_parameter",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "media_type 只能是 image 或 video",
  "code": "invalid_query_parameter",
  "instance": "/api/v1/images",
  "request_id": "example-request-id"
}
```

部分校验错误还提供 `errors` 字段，列出 `field`、`code`、`message`。排查时保留 `request_id` 或响应头 `X-Request-ID`，但去掉密码、完整认证头和存储凭据。

| 情况 | 处理方式 |
| --- | --- |
| `401`：令牌无效、过期、撤销或会话失效 | 重新登录或创建合适的新 PAT，不循环重放旧凭据 |
| `403`：scope / 用户权限不足或 `password_change_required` | 检查 scope、用户授权，或先在浏览器完成改密 / 恢复 |
| `403`：`user_not_manageable` / `permission_not_held` | 目标管理员的权限不少于自己，或要授予自己没有的权限；需由权限更多的管理员操作 |
| `409`：`concurrent_update_conflict` | 用户数据被并发修改，请求未生效；重新读取后重试 |
| `409` / 文件级 `storage_object_conflict` | 保留失败信息，核对存储，不使用覆盖写绕过冲突 |
| `409`：`storage_disabled` / `storage_is_upload_default` | 目标存储已停用，或操作的是当前默认存储；改用其他存储或先调整设置，重试不会成功 |
| `413`、`422` 或文件级格式 / 大小 / 尺寸错误 | 调整请求或输入；SVG、不支持的格式及超限图像不会因重试变合法 |
| `429`：限流 | 遵循 `Retry-After` 等待，减少调用频率 |
| `503` 或文件级 `image_processing_busy` | 稍后重试失败项；请求级繁忙响应可带 `Retry-After`，不要无限自动重试 |
| `5xx`：远端或服务错误 | 先核对结果及日志，避免盲目重发造成重复任务 |

密码修改或管理员重置会撤销该用户全部旧会话和 PAT；管理员重置还会撤销其 Passkey，并要求首次登录改密。普通升级不会无条件清空正常历史 PAT；v4.1.3 的历史默认密码风险账号是例外。具体规则见[升级指南](upgrading.md#初始化与账号恢复)。

## 浏览器认证与完整契约

项目网页使用 Session Cookie，写请求还需匹配的 `X-OneImg-CSRF`；外部集成优先使用 PAT，无需自行模拟网页登录。令牌管理和头像上传 / 移除仅接受浏览器会话，不能使用现有 PAT 创建另一枚 PAT。修改密码、注册或撤销 Passkey 等敏感操作还需当前密码。

在自己的部署中访问以下地址，将 `https://oneimg.example.com` 换成实际 Origin：

| 入口 | 地址 |
| --- | --- |
| OpenAPI YAML | `https://oneimg.example.com/api/openapi.yaml` |
| OpenAPI JSON | `https://oneimg.example.com/api/openapi.json` |
| Swagger UI | `https://oneimg.example.com/api/docs` |

仓库契约源文件为 [api/openapi.yaml](../api/openapi.yaml)。Swagger UI 的界面资源由外部 CDN 加载；无外网环境可直接读取 YAML / JSON 契约。新增接口或生成客户端的维护方式见[开发指南](development.md#接口契约与客户端生成)。
