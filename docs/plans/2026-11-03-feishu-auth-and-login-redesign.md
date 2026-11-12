# T-067 登录页面重设计（J 极客重奏双栏工作台）与飞书（Feishu/Lark）登录接入实施计划

- **TODO ID**: `T-067`
- **日期**: `2026-11-03`
- **关联原型**: `docs/prototypes/login-feishu-preview.html` (v4.1)
- **关联决策记录**: `docs/decisions/0004-auth-and-workspace.md` (ADR-0004), `docs/decisions/0006-user-registration-and-workspace-creation.md` (ADR-0006)

---

## 1. 背景、目标与非目标

### 1.1 背景
当前 TokenPivot 的浏览器登录页（`web/src/features/auth/pages/login-page.tsx:1-29`）与注册页（`web/src/features/auth/pages/register-page.tsx:1-29`）仍使用早期 `max-w-sm` 窄单栏白板布局（`web/src/features/auth/auth-layout.tsx:1-23`），仅支持邮箱与密码登录，尚未与总览页已落地的 **J 极客重奏（F++）暖黑琥珀终端视觉体系**（`web/src/features/overview/geek/geek-tokens.css:1-120`、`docs/prototypes/dashboard-j.html:1-120`）统一，也缺少企业内部高频使用的飞书（Feishu/Lark）单点登录与扫码登录能力。

### 1.2 核心目标（已由用户确认定稿）
1. **双栏视觉聚焦工作台（方案 III + 左栏内置 Dashboard 实时遥测快照 — 决策 1.A）**：
   - 100% 沿用 J 极客重奏暖黑琥珀色板（`--bg:#0b0a08`、`--panel:#120f0a`、`--panel-focus:#16120c`、`--amber:#e0a64b`、`--amber-dim:#7a6435`、`--copper:#d1604f`、`--olive:#8a8672`、`--green:#7fb069`，等宽数字 `tabular-nums`），不引入任何蓝紫外部色系。
   - **左栏（未登录态内置确定性 Dashboard 实时遥测快照）**：不调用未授权后端统计接口，前端直接内置确定性演示遥测快照（今日累计 Token Hero 大数 `67,248,910 tokens` + 4 格核心指标 + Braille `4×2` 分钟速率曲线支持鼠标悬浮读数 + 按模型/供应商/项目切换的字符进度条排行 `█`/`░`），以无厚重外框、`opacity: 0.82`（hover 提亮）降噪呈现。
   - **右栏（全场唯一高对比聚光登录主卡 —— 方案 III「左飞书扫码/一键 ‖ 右邮箱密码」并排双舱）**：
     - 卡片顶部带 `border-top: 3px solid var(--amber)` 琥珀亮顶与背光阴影，右上角带「登录 / 注册」切换胶囊。
     - **左舱（206px，飞书 SSO 舱）**：将飞书授权地址编码为二维码矩阵，以**干净琥珀极客点阵 + 中心琥珀飞书小徽标（无移动扫描线动画）**风格渲染，支持 180s 有效期倒计时、过期遮罩与 `[R]` 刷新；下方配实心琥珀主按钮「一键唤起飞书授权 `[F]`」。
     - **右舱（邮箱与密码舱）**：`>` 终端前缀输入框；密码框在聚焦或有输入时**渐进披露**字符码点（8–64）与 UTF-8 字节（≤256B）进度标尺，实时检测 `CapsLock` 大写锁定提示；支持 429 `RATE_LIMITED` 秒级退避倒计时、403 `REGISTRATION_CLOSED` 引导使用左侧飞书登录。
2. **飞书登录与现有邮箱账号关联策略（同邮箱首次需原密码确认绑定 — 决策 2.A）**：
   - 飞书 OAuth 2.0 回调取得已验证的企业邮箱与 `open_id` 后：
     - 若已绑定 `user_identities`，直接签发 `tokenpivot_session` Cookie 登录；
     - 若未绑定但 `users` 表已存在相同规范化 `email` 的本地账号，签发短期 HttpOnly 待绑定凭证 Cookie `tokenpivot_feishu_bind`（10 分钟有效），回到登录页展示「同邮箱绑定确认（`LINK_ONCE`）」横幅，用户输入一次原账号密码验证通过后完成绑定并下发正式 `tokenpivot_session` Cookie；
     - 若 `users` 表不存在该 `email`，按准入策略自动创建账号并绑定。
3. **注册关闭时的飞书准入策略（独立开关 `TOKENPIVOT_FEISHU_AUTO_PROVISION` — 决策 3.A）**：
   - 新增环境变量 `TOKENPIVOT_FEISHU_AUTO_PROVISION`（默认 `true`）。当 `TOKENPIVOT_REGISTRATION_OPEN=false` 时，普通邮箱密码注册继续拦截（返回 `403 REGISTRATION_CLOSED`，空库首用户除外），但通过飞书企业认证的员工允许自动创建 `users` + `user_identities` 并登录。
   - 遵守 ADR-0006（T-064）：飞书自动创建用户同样只写 `users` + `user_identities` + `sessions`，不自动创建默认工作空间。

### 1.3 非目标
- 不支持同一用户绑定多个不同飞书 `open_id`（`UNIQUE (user_id, provider)`）。
- 不做未登录态的真实租户遥测数据公开查询接口（避免多租户模型与用量信息泄露）。
- 不改动采集端点（`/api/v1/ingestion/events`）及工作空间 Bearer 凭证认证逻辑。

---

## 2. 数据库 Schema 迁移设计

最新迁移为 `db/migrations/00007_source_archival.sql`，本次新增 `db/migrations/00008_feishu_auth_identities.sql`。

### 2.1 Schema 变更要点
1. **允许纯飞书自动创建的账号无本地密码**：
   - 在 `db/migrations/00005_auth_users_sessions.sql:10-18` 中，`users.password_hash` 原为 `NOT NULL`。
   - 迁移执行 `ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;`，并更新非空空白检查约束为允许 `NULL` 或非空白哈希串。
   - 密码登录（`POST /api/v1/auth/login`）查询到 `password_hash IS NULL` 时，仍执行一次固定开销的虚拟 Argon2id 校验并返回 `ErrInvalidCredentials`（`401 INVALID_CREDENTIALS`），不向外部暴露该邮箱是否为纯飞书账号。
2. **新增 `user_identities` 表**：
   - 记录外部身份提供商（首版 `provider = 'feishu'`）与 `users.id` 的一对一绑定关系。

### 2.2 迁移 SQL 草案（`db/migrations/00008_feishu_auth_identities.sql`）

```sql
-- +goose Up
ALTER TABLE users
    ALTER COLUMN password_hash DROP NOT NULL;

CREATE TABLE user_identities (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id          uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider         text        COLLATE "C" NOT NULL,
    provider_user_id text        COLLATE "C" NOT NULL,
    union_id         text        COLLATE "C",
    tenant_key       text        COLLATE "C",
    email            text        COLLATE "C" NOT NULL,
    display_name     text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_user_identities_provider_nonempty CHECK (btrim(provider) <> ''),
    CONSTRAINT chk_user_identities_provider_uid_nonempty CHECK (btrim(provider_user_id) <> ''),
    CONSTRAINT uk_user_identities_provider_uid UNIQUE (provider, provider_user_id),
    CONSTRAINT uk_user_identities_user_provider UNIQUE (user_id, provider)
);

CREATE INDEX idx_user_identities_user_id ON user_identities (user_id);

-- +goose Down
DROP TABLE IF EXISTS user_identities;

UPDATE users SET password_hash = '!sso-disabled' WHERE password_hash IS NULL;
ALTER TABLE users
    ALTER COLUMN password_hash SET NOT NULL;
```

---

## 3. OpenAPI 接口文档变更与后端架构设计

所有新增端点同步写入 OpenAPI 接口文档 `docs/openapi/openapi.yaml` 的 `auth` 标签下，遵守 `internal/server/auth.go:1-260` 的严格 JSON 解析、`Cache-Control: no-store` 与 `Origin` 校验约定。

### 3.1 环境变量配置
在服务启动配置中新增以下环境变量：
- `TOKENPIVOT_FEISHU_APP_ID`：飞书应用 App ID（默认为空；为空时表示当前实例未配置飞书登录）。
- `TOKENPIVOT_FEISHU_APP_SECRET`：飞书应用 App Secret（默认为空，绝不写入日志或响应）。
- `TOKENPIVOT_FEISHU_REDIRECT_URI`：可选覆盖飞书 OAuth 回调地址；未设置时默认由可信 Web Origin 拼接 `/api/v1/auth/feishu/callback`。
- `TOKENPIVOT_FEISHU_AUTO_PROVISION`：布尔值，只接受精确的 `true` / `false`，未设置时默认为 `true`。控制当 `TOKENPIVOT_REGISTRATION_OPEN=false` 时是否允许新飞书用户自动建号。
- `TOKENPIVOT_FEISHU_BASE_URL`：可选，默认 `https://open.feishu.cn`（便于集成测试指向本地 `httptest.Server` 桩服务）。

### 3.2 新增认证端点设计

#### (1) `GET /api/v1/auth/feishu/authorize` — 获取飞书授权会话与二维码 URL
- **用途**：前端登录页初始化或点击 `[R]` 刷新二维码时调用。
- **行为**：
  - 若服务端未配置 `TOKENPIVOT_FEISHU_APP_ID` / `TOKENPIVOT_FEISHU_APP_SECRET`，返回 `200 OK`：
    ```json
    {
      "enabled": false,
      "authorize_url": null,
      "expires_in_seconds": 0
    }
    ```
  - 若已配置：生成 128-bit 随机 `state`（` crypto/rand.Text()`），将 `SHA-256(state)` 写入短期 HttpOnly Cookie `tokenpivot_feishu_state`（`Max-Age=180`, `HttpOnly`, `SameSite=Lax`, `Path=/api/v1/auth/feishu`），构造飞书授权地址 `https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=...&redirect_uri=...&state=...`，返回 `200 OK`：
    ```json
    {
      "enabled": true,
      "authorize_url": "https://open.feishu.cn/open-apis/authen/v1/authorize?app_id=cli_xxx&redirect_uri=...",
      "expires_in_seconds": 180
    }
    ```
  - 前端使用返回的 `authorize_url` 同时驱动：
    1. 左舱的**琥珀极客点阵二维码**（手机飞书扫码后在手机端确认授权，或桌面端飞书扫码跳转回调）；
    2. 左舱底部的**「一键唤起飞书授权 `[F]`」**按钮跳转。

#### (2) `GET /api/v1/auth/feishu/callback` — 飞书 OAuth 2.0 授权回调
- **查询参数**：`code` (必填)、`state` (必填)。
- **校验与处理流程**：
  1. 校验 `tokenpivot_feishu_state` Cookie 是否存在且与 `SHA-256(state)` 恒定时间相等；无论成功失败均立即清除 `tokenpivot_feishu_state` Cookie。若不匹配或缺失，重定向至 `/login?error=FEISHU_STATE_INVALID`。
  2. 按远端 IP 执行认证限流检查（复用 `internal/auth/limiter.go`）。
  3. 服务端使用 `app_id` + `app_secret` 调飞书开放平台接口换取 `user_access_token` 并拉取用户信息（`open_id`, `union_id`, `tenant_key`, `email`, `name`）。若飞书未返回有效邮箱，重定向至 `/login?error=FEISHU_EMAIL_REQUIRED`。
  4. **分支 A（已绑定）**：查询 `user_identities` 命中 `(provider='feishu', provider_user_id=open_id)` → 为该 `user_id` 创建 `sessions` 行，种下 `tokenpivot_session` Cookie，重定向 `302` 至 `/`。
  5. **分支 B（未绑定，但 `users` 表已存在同 `email` 账号 — 决策 2.A）**：
     - 生成短期绑定令牌 `tpb_` + `crypto/rand.Text()`，服务端内存短期表（或签名令牌，有效期 10 分钟）保存 `(tokenHash, userID, openID, unionID, tenantKey, email, displayName)`；
     - 种下 HttpOnly Cookie `tokenpivot_feishu_bind`（`Max-Age=600`, `HttpOnly`, `SameSite=Lax`, `Path=/api/v1/auth/feishu`）；
     - 重定向 `302` 至 `/login?feishu_bind=1&email=<urlencoded_email>`。
  6. **分支 C（未绑定，且 `users` 表不存在该 `email` — 决策 3.A）**：
     - 在同一数据库事务内：若 `!registrationOpen && !feishuAutoProvision`， masih 检查是否空库首用户（`LockRegistration` + `HasUsers`）；若非空库且两开关均关，重定向至 `/login?error=REGISTRATION_CLOSED`。
     - 若允许建号（`feishuAutoProvision == true` 或 `registrationOpen == true` 或空库首用户）：在事务内插入 `users(email, password_hash=NULL)` → 插入 `user_identities` → 插入 `sessions`，种下 `tokenpivot_session` Cookie，重定向 `302` 至 `/`。

#### (3) `POST /api/v1/auth/feishu/bind` — 同名邮箱首次密码验证并绑定飞书身份
- **安全前置**：强制校验可信 `Origin`（与 `handleLogin` 一致）、校验 `tokenpivot_feishu_bind` Cookie、按邮箱与远端 IP 限流。
- **请求体**（严格 JSON 解析，仅接受 `password` 字符串字段）：
  ```json
  {
    "password": "user-existing-password"
  }
  ```
- **行为**：
  1. 根据待绑定凭证中的 `userID` / `email` 读取 `users` 的 `password_hash` 并执行 Argon2id 校验。
  2. 密码错误返回 `401 INVALID_CREDENTIALS`（待绑定凭证保留至过期或限流耗尽）。
  3. 密码正确：在同一事务内写入 `user_identities`（`ON CONFLICT (user_id, provider) DO NOTHING`）并创建新 `sessions` 行；清除 `tokenpivot_feishu_bind` Cookie，种下正式 `tokenpivot_session` Cookie，返回 `200 OK`（标准 `SessionResponse`）。

---

## 4. 前端双栏登录工作台设计（`web/src/features/auth/`）

与已定稿原型 `docs/prototypes/login-feishu-preview.html` (v4.1) 保持 1:1 视觉与交互一致：

### 4.1 组件结构拆分
1. **`web/src/features/auth/geek-auth-workbench.tsx`**：
   - 顶栏 Shell Prompt（`tokenpivot:~/overview$ tp-watch --window=1h --granularity=1m` + `J · F++` 徽标）+ 主舞台双栏网格（`grid-template-columns: 1fr 560px`，`<1100px` 响应式单栏）+ 底部琥珀 `tmux` 状态栏（展示当前模式与实时认证状态反馈）。
2. **`web/src/features/auth/auth-dashboard-snapshot.tsx`（左栏内置确定性 Dashboard 实时遥测快照 — 决策 1.A）**：
   - 内置确定性遥测常量数据（不发起网络请求）：
     - `00 TOTAL TOKENS`：今日累计 `67,248,910 tokens` + `ROLLING 60S: 148,920 tok/min` + 4 格核心指标（`INPUT TOKENS 52.4M`、`OUTPUT TOKENS 14.8M`、`REQUESTS 4,812`、`60S RATE 2,482/s`）；
     - `01 MINUTE TOKEN VELOCITY`：Braille `4×2`（`U+2800`）34 分钟序列曲线，支持鼠标 `mousemove` 横向高亮对应列并实时回显 `T-Nm: XXX,000 tok/min (in XXk · out XXk)`；
     - `02 TOP CONSUMPTION RANKING`：支持「按模型 / 按供应商 / 按项目」三档切换与 `█`/`░` 字符进度条。
3. **`web/src/features/auth/feishu-qr-matrix.tsx`（左舱琥珀点阵二维码组件 — 无扫描线）**：
   - 使用轻量无外部 Canvas 污染的 QR 矩阵生成逻辑（或纯 SVG/CSS Grid 点阵渲染），将 `authorize_url` 渲染为琥珀色方格点阵（实心模块 `var(--amber)`、空白模块 `rgba(224,166,75,0.06)`），正中央叠加 `26×26px` 暖黑底琥珀边飞书图标徽标（`.qr-center-badge`）。
   - **严格不渲染移动扫描线动画**。
   - 内置 180 秒倒计时，归零时显示「二维码已过期 · 点击刷新 `[R]`」遮罩；服务端未配置飞书时显示「当前实例未配置飞书应用凭证」静态提示遮罩并禁用一键按钮。
4. **`web/src/features/auth/auth-dual-cabin-card.tsx`（右栏方案 III 双舱登录主卡）**：
   - `border-top: 3px solid var(--amber)` 琥珀亮顶与径向背光；
   - 头部「登录 / 注册」胶囊切换（点击在 `/login` 与 `/register` 间平滑切换）；
   - 状态横幅（支持飞书握手进度、`LINK_ONCE` 同邮箱密码绑定确认、`403 REGISTRATION_CLOSED`、`429 RATE_LIMITED` 秒级倒计时）；
   - 左舱（飞书扫码 + 实心琥珀按钮「一键唤起飞书授权 `[F]`」）+ 右舱（`>` 终端前缀邮箱/密码表单、聚焦时渐进披露码点与 UTF-8 字节标尺、CapsLock 检测）；
   - 全局快捷键支持：`F` 唤起飞书授权、`/` 聚焦邮箱、`R` 刷新二维码、`1/2` 切换登录/注册、`Esc` 失焦。

---

## 5. 执行切分（Workstreams）

按照 `delegation-protocol` 的「共享基础 + 不相交并行块」原则，切分为 1 个共享基础块 + 2 个完全不相交的前后端并行块：

### Workstream 0：共享基础（数据库迁移 + OpenAPI 接口文档）
- **执行方式**：先派发 1 个 `dev` 子代理独立完成并合入主工作分支，作为后续并行块的共同基线。
- **文件范围**：
  - 新建 `db/migrations/00008_feishu_auth_identities.sql`
  - 修改 `docs/openapi/openapi.yaml`（新增 `/api/v1/auth/feishu/authorize`、`/api/v1/auth/feishu/callback`、`/api/v1/auth/feishu/bind` 三个端点定义与相关 schema）
- **不得改动的接口**：
  - 既有 `/api/v1/auth/register`、`/api/v1/auth/login`、`/api/v1/auth/session`、`/api/v1/auth/logout` 的请求与响应格式保持 100% 兼容。
- **验证命令与验收标准**：
  - 迁移文件包含完整可逆的 `-- +goose Up` 与 `-- +goose Down`；
  - OpenAPI YAML 语法合法且無「契约」「门禁」禁用词。

### Workstream 1：Go 后端飞书 OAuth 2.0、同邮箱绑定与准入开关实现
- **依赖**：Workstream 0 合入后从最新 HEAD 派发。
- **文件范围**（仅限 Go 后端，不触碰 `web/`）：
  - `internal/auth/service.go`、`internal/auth/feishu.go`（新建）、`internal/auth/postgres.go`
  - `internal/auth/service_test.go`、`internal/auth/feishu_test.go`（新建）、`internal/auth/postgres_integration_test.go`
  - `internal/server/auth.go`、`internal/server/feishu_auth.go`（新建）、`internal/server/server.go`、`internal/server/auth_test.go`
  - `cmd/tokenpivot/main.go`（或对应配置解析文件，读取 `TOKENPIVOT_FEISHU_*` 环境变量）
- **不得改动的接口**：
  - 不修改 `web/` 下任何文件；不修改采集与统计查询包（`internal/ingestion/`, `internal/usage/`）。
- **验证命令与验收标准**：
  - `go test ./internal/auth/... ./internal/server/... ./cmd/...` 全部通过；
  - 覆盖以下核心单测与集成测试：
    1. 未配置飞书环境变量时 `GET /api/v1/auth/feishu/authorize` 返回 `enabled: false`；
    2. 已绑定飞书 `open_id` 回调直接签发 `tokenpivot_session` Cookie 并 `302` 到 `/`；
    3. 未绑定但本地存在同名 `email` 时（决策 2.A），回调种下 `tokenpivot_feishu_bind` 并重定向 `/login?feishu_bind=1&email=...`，随后调用 `POST /api/v1/auth/feishu/bind` 校验原密码成功后完成绑定并签发会话，密码错误返回 `401 INVALID_CREDENTIALS`；
    4. `TOKENPIVOT_REGISTRATION_OPEN=false` 且 `TOKENPIVOT_FEISHU_AUTO_PROVISION=true` 时（决策 3.A），普通邮箱注册返回 `403 REGISTRATION_CLOSED`，但新飞书用户回调可自动创建 `users(password_hash=NULL)` + `user_identities` + `sessions`；
    5. 纯飞书账号（`password_hash IS NULL`）尝试用邮箱密码登录时安全返回 `401 INVALID_CREDENTIALS`。

### Workstream 2：React 前端 J 极客双栏登录工作台实现
- **依赖**：与 Workstream 1 并行派发（仅修改 `web/` 目录，文件集合与 Workstream 1 完全不相交）。
- **文件范围**（仅限 `web/`）：
  - `web/src/features/auth/api.ts`、`web/src/features/auth/model.ts`、`web/src/features/auth/queries.ts`
  - `web/src/features/auth/auth-dashboard-snapshot.tsx`（新建：左栏内置确定性 Dashboard 遥测快照 + Braille 曲线 + 排行）
  - `web/src/features/auth/feishu-qr-matrix.tsx`（新建：琥珀点阵二维码 + 中心飞书徽标，无扫描线动画）
  - `web/src/features/auth/auth-dual-cabin-card.tsx`（新建：右栏方案 III 双舱聚光登录卡）
  - `web/src/features/auth/auth-layout.tsx`、`web/src/features/auth/auth-form.tsx`
  - `web/src/features/auth/pages/login-page.tsx`、`web/src/features/auth/pages/register-page.tsx`
  - `web/src/features/auth/auth.css`（新建或纳入 `web/src/index.css`）
  - `web/src/features/auth/__tests__/*`（前端单测更新与新增）
- **不得改动的接口**：
  - 不修改 Go 后端文件；保持 `useCredentialsMutation`、`sessionQueryOptions` 等对外導出签名兼容。
- **验证命令与验收标准**：
  - `pnpm --dir web run typecheck`、`pnpm --dir web run test`、`pnpm --dir web run build` 全部通过；
  - 组件测试覆盖：左栏内置 Dashboard 快照渲染与维度切换、右栏双舱（飞书扫码无扫描线 + 一键唤起按钮 + 邮箱密码表单）渲染、密码聚焦渐进披露标尺、同邮箱绑定确认模式（`?feishu_bind=1&email=...`）提交与 429 退避倒计时。

---

## 6. 总体发布前检查与验收标准清单

| 编号 | 验收项 | 对应决策 | 验证方式 |
|---|---|---|---|
| AC-1 | 左栏展示内置确定性 J 风格 Dashboard 快照（今日大数、4 格指标、Braille 4×2 曲线、Top 排行），不发未登录统计请求 | 决策 1.A | 前端单测 + 构建检查 |
| AC-2 | 右栏为唯一高对比琥珀亮顶双舱卡（左飞书扫码+一键唤起 ‖ 右邮箱密码），二维码为干净琥珀点阵+中心飞书徽标，无移动扫描线 | 方案 III + 二维码定稿 | 前端单测 + 样式与 DOM 断言 |
| AC-3 | 飞书回调命中已有同名本地邮箱账号时，要求输入一次原密码完成绑定（`POST /api/v1/auth/feishu/bind`），验证通过后写入 `user_identities` | 决策 2.A | Go 后端单测/集成测试 + 前端绑定态单测 |
| AC-4 | `TOKENPIVOT_REGISTRATION_OPEN=false` 且 `TOKENPIVOT_FEISHU_AUTO_PROVISION=true` 时，拦截普通邮箱注册但允许飞书认证新用户自动建号 | 决策 3.A | Go 后端单测/集成测试 |
| AC-5 | 全仓 Go 测试、前端类型检查、前端单测与生产构建全部通过 | D9 发布前检查 | `go test ./...` & `pnpm --dir web run build` |
