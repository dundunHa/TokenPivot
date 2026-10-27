# T-051 开发运行、构建与基础检查

> 日期：2026-10-27。状态：规划及只读评审完成；仅规划，未授权实施。前置 T-049、T-050、T-031 已完成归档，以当前代码为准。本文不修改接口、数据库格式或工具版本。

## 目标与非目标

目标：新环境按根说明安装既定依赖、注入本地配置，能够启动 Go + Vite、分别构建 Go/前端、构建内嵌静态资源的 Go 二进制，并执行有明确失败状态的基础检查。需要 PostgreSQL 的检查必须单独、明确调用。

非目标：业务表/API、登录或认证授权、产品页面、Redis、容器/编排、自动安装或切换工具链、热重载工具、通用任务框架、Git hooks、CI/CD 和发布平台。当前探针、占位页和测试不能证明业务、迁移版本或认证已可用。不新增检查依赖，不升级锁定版本，不修改迁移或运行时代码。

## 现状与缺口

| 已核实现状 | T-051 要补的缺口 |
|---|---|
| [根 README:7–44](/Users/lxp/code-space/gitspace/TokenPivot/README.md#L7-L44) 已写 Go 1.27.3、PG 18.6、本地角色、配置和凭证边界；[go.mod:3–7](/Users/lxp/code-space/gitspace/TokenPivot/go.mod#L3-L7) 固定 Go、pgx、goose。 | 尚无根配置示例；首次安装、开发、构建和检查分散，没有可组合的根入口。 |
| [前端包定义:6–16](/Users/lxp/code-space/gitspace/TokenPivot/web/package.json#L6-L16) 已声明 npm 11.19.1、Node `>=24.22.1 <25` 及六个脚本；[锁文件:1–33](/Users/lxp/code-space/gitspace/TokenPivot/web/package-lock.json#L1-L33) 为 lockfileVersion 3。[前端 README:7–23](/Users/lxp/code-space/gitspace/TokenPivot/web/README.md#L7-L23) 指定 Node 24.22.1。 | 不缺脚本、锁文件或格式/类型工具；应复用，而不是另建根 npm workspace 或第二套检查配置。 |
| [根 README:94–103](/Users/lxp/code-space/gitspace/TokenPivot/README.md#L94-L103) 的 Go 格式命令只列文件，不自动把“有差异”转成失败，也未覆盖前端目录内的 Go 文件。 | 新的格式入口要覆盖全部现有 Go 源码（含 build-tag 文件），非零失败、绝不自动改写；前端继续用既有 Prettier。 |
| [配置加载:20–53](/Users/lxp/code-space/gitspace/TokenPivot/internal/config/config.go#L20-L53) 仅读进程环境；服务/迁移连接分别必填。服务 [run:27–50](/Users/lxp/code-space/gitspace/TokenPivot/cmd/tokenpivot/main.go#L27-L50) 先初始化前端、连库 Ping，再监听；不自动迁移。 | 启动说明必须先准备专用本地 PG、独立迁移、注入服务连接，再启动 Go/Vite；不能把 `go run` 描述成无需 PG 即可成功。 |
| [Vite 配置:6–45](/Users/lxp/code-space/gitspace/TokenPivot/web/vite.config.ts#L6-L45) 已实现固定回环端口、精确 `/api` 代理和进程环境覆盖；[前端说明:34–38](/Users/lxp/code-space/gitspace/TokenPivot/web/README.md#L34-L38) 明确不从 dotenv 读取该覆盖变量。 | 示例不能暗示自动加载 `.env`，不能用 `VITE_` 存连接信息，也不新开 CORS。 |
| [纯后端入口](/Users/lxp/code-space/gitspace/TokenPivot/web/noembed.go#L1-L10) 无需静态产物；[内嵌入口](/Users/lxp/code-space/gitspace/TokenPivot/web/embed.go#L1-L22) 要求 `uiembed` 和真实 dist；[内嵌测试](/Users/lxp/code-space/gitspace/TokenPivot/web/embed_test.go#L14-L74) 验证真实 HTML、JS/CSS、MIME 和开发 target 不泄漏。 | 根构建需明确 Go 独立构建、前端独立构建，以及“先 web、后 Go 内嵌”的顺序，避免误发纯后端二进制。 |
| [集成测试辅助函数](/Users/lxp/code-space/gitspace/TokenPivot/internal/database/database_integration_test.go#L18-L115) 已验证显式测试变量、回环 IP、`_test` 库名、双角色权限、随机 schema 与清理归属；缺配置直接失败，不 skip。 | 不重写数据库安全检查；提供独立且不使用测试缓存的联合标签入口。根 README 所称“失败保留 schema”过宽：实际 cleanup 仍会执行，仅归属检查/清理失败等情形保留并报错，须改正文案。 |

根 Makefile、根 `.env.example` 和 scripts 入口未发现；[当前忽略规则](/Users/lxp/code-space/gitspace/TokenPivot/.gitignore#L1-L5) 仅覆盖 web 依赖、dist 和 web 环境文件。没有证据表明需要新增脚本目录。

## 决策及理由

### 1. 提供一个薄 Makefile，不另建任务系统

新增根 Makefile 只组合已有 Go/npm 命令，解决两种工具链的执行顺序、格式失败状态和统一发现入口这三个实际缺口。既有 npm 脚本保持不变，直接命令继续可用。不引入 Task/Just、根 package.json、可配置执行器或辅助脚本。

默认 `make` / `make help` 只展示命令，不安装依赖、迁移或启动服务。全部目标声明为 `.PHONY`。Go 命令均显式使用 `GOTOOLCHAIN=local`；build/test/vet 使用 `-mod=readonly`，不自动修复模块声明。该参数不是“不修改 go.sum”的承诺，干净环境验收仍需对照模块文件和前端锁文件。

新增目标固定如下；表中均为**待实施**入口，不是当前已有命令：

| 新目标 | 行为及输出 |
|---|---|
| `make dev-go` | `GOTOOLCHAIN=local go run -mod=readonly ./cmd/tokenpivot`；继承当前进程环境，前台运行，不迁移。 |
| `make dev-web` | `npm --prefix web run dev`；前台运行，不同时管理 Go 进程。 |
| `make build-go` | 建立忽略提交的 `bin/`；分别用 `go build -mod=readonly` 构建 `./cmd/tokenpivot` → `bin/tokenpivot-api`、`./cmd/tokenpivot-migrate` → `bin/tokenpivot-migrate`，不加 `uiembed`，不依赖 Node/dist。 |
| `make build-web` | `npm --prefix web run build`；现有脚本先 typecheck，再生成 `web/dist/`。不自动安装依赖。 |
| `make build` | 顺序执行 build-web，再构建 `-tags=uiembed` 的服务 → `bin/tokenpivot`，再构建迁移二进制 → `bin/tokenpivot-migrate`。失败立即退出，不启动任何进程。 |
| `make format-check` | Go 格式只检查 `cmd`、`internal`、`db` 的递归 Go 文件及 `web/*.go`，避开 `web/node_modules`/dist；再执行 `npm --prefix web run format:check`。 |
| `make check` | 顺序执行 format-check、普通 Go vet、前端 lint、普通 Go race 测试、前端测试、build-go、build，最后执行 `uiembed` 的 Go race 测试与 vet。Go 测试加 `-count=1`。build 已包含前端 typecheck，不在集合内重复调用；独立类型入口仍是现有 `npm --prefix web run typecheck`。无 PG 依赖。 |
| `make test-integration` | 顺序执行 build-web，然后 `GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed,integration -count=1 -p 1 -v ./...`。要求专用 PG 和两个测试连接变量；不包含在 check 内。 |

有顺序要求的组合必须用顺序 recipe/递归 `$(MAKE)` 调用，不能写成允许 `make -j` 同时构建 web 和内嵌 Go 的平行 prerequisites。不得使用忽略退出码的 `-`、`|| true` 或“失败后继续”的汇总成功状态。组合目标失败时可能留有旧/部分构建产物；本次命令非零即整体失败，不得继续启动或把旧产物当作新构建成功。

Go 格式检查用 shell 捕获 `gofmt -l cmd internal db web/*.go` 的输出：先传播命令本身的错误；有列出的文件则打印文件列表并退出 1；为空才继续。不能只执行 `gofmt -l` 后返回成功，也不能加 `-w`。此处不新建检查程序，是因为一个短 recipe 已能表达实际失败条件。前端继续使用既有 `prettier --check`，不使用 `--write`。

### 2. 配置继续只经进程环境，不新增加载行为

根 `.env.example` 仅是变量清单和安全占位示例，**不是可直接启动的有效配置，也不会自动加载**；不要求复制为 `.env`，Makefile 不 source 文件，不增加 dotenv/YAML 库。

- 可展示的公开默认值仅为 `TOKENPIVOT_HTTP_ADDR=127.0.0.1:8080`、`TOKENPIVOT_DEV_API_TARGET=http://127.0.0.1:8080`。
- `TOKENPIVOT_DATABASE_URL`、`TOKENPIVOT_MIGRATION_DATABASE_URL`、`TOKENPIVOT_TEST_DATABASE_URL`、`TOKENPIVOT_TEST_APP_DATABASE_URL` 只写变量名及注释中的 `<由本地凭证工具安全注入>` 占位，不写真实或形似可用的连接串，不互相回退。
- README 说明哪个进程需要哪些变量：Go 需要服务连接；独立迁移只需迁移连接；Vite 只需可选公开上游地址；集成测试使用独立的两个测试连接。
- 明确关闭 shell 跟踪，不 echo/printenv、不把连接串放命令参数、日志、源码或前端环境变量中。沿用现有 PG 默认配置隔离及 TLS 约束，不另建凭证工具。
- 示例文件不承诺角色、数据库或 PG 安装自动完成。管理员仍按已有本地准备步骤，在专用实例交互执行；迁移仍由人显式调用。

### 3. 区分普通检查与真实 PG 检查

`check` 需要 Go、同安装的 gofmt、Node/npm、make、POSIX shell 和 race 所需本地 C 编译工具链，但不需要 PG、连接变量、业务表或登录。前端测试边界是 [现有 node:test](/Users/lxp/code-space/gitspace/TokenPivot/web/test/proxy.test.mjs#L57-L188)：它会临时启动回环 HTTP/Vite 进行代理验证，并非 UI 自动化或完整产品测试。TypeScript strict 继续覆盖 src 与 Vite 配置，[配置范围](/Users/lxp/code-space/gitspace/TokenPivot/web/tsconfig.json#L1-L17) 不扩大；ESLint/Prettier 分工不变。

`test-integration` 是有数据库写入的显式操作，不伪装成普通检查。采用联合 `uiembed,integration` 标签是为了保留[现有联合验收要求](/Users/lxp/code-space/gitspace/TokenPivot/README.md#L155)，不能用两个标签分别跑过代替。`-count=1` 避免历史测试缓存掩盖本次配置或数据库状态；`-p 1` 保持包级串行。同一测试库仍须人工保证独占，不新增跨进程锁或自动重试。

直接复用三处已有集成测试的安全检查，不在 Makefile 解析连接串或重复查询权限。只允许专用回环 `_test` 数据库、明确的同库双角色和既定认证方式；测试拥有新建的随机 schema，不扫描/清空旧对象，不重置数据库，不连接现有业务库。缺变量、连接拒绝、角色/权限错误、认证错误或清理失败都必须非零，输出安全诊断而非原始连接/SQL。若测试被杀或清理失败，保留其报告的 schema，由人核对归属，不提供自动清扫目标。

## 实施文件清单

以下是**用户另行授权实施后**的完整源文件范围；本轮实际只新增本文。

| 文件 | 操作与要点 |
|---|---|
| `Makefile`（拟新增） | 上述九个目标（含 help）；顺序、错误传播、Go 格式失败处理；无安装/迁移/清理/后台进程管理目标。 |
| `.env.example`（拟新增） | 公开默认值、四个连接变量的注释占位、各使用方与“不自动加载”警告；不含凭证或连接串。 |
| [根 README](/Users/lxp/code-space/gitspace/TokenPivot/README.md) | 增补工具前提、首次安装、命令表与最短双终端启动；保留数据库/TLS/迁移安全说明，把撤销演练与最短启动分开；补失败退出、检查边界、输出路径、普通/内嵌/联合验收。修正“失败保留 schema”和“不建设 Makefile”的过时描述，不将 probe 说成业务成功。 |
| [.gitignore](/Users/lxp/code-space/gitspace/TokenPivot/.gitignore) | 在原规则上仅增加 `/bin/`、`/.env`、`/.env.*` 和 `!/.env.example`，保留全部 web 规则。忽略文件不是密钥管理措施。 |
| [前端 README](/Users/lxp/code-space/gitspace/TokenPivot/web/README.md) | 只补根启动前置/统一入口的相对链接，提示 Go 先连库；保留独立 npm 命令和详细代理/静态服务说明，不复制第二套安装步骤。若新增文字被现有 Prettier 判定失败，仅调整本次文字格式。 |

不改 Go/TS 源码、迁移、模块文件、前端包定义/锁文件/检查配置、架构文档或 TODO。若实际实施发现现有依赖/代码无法通过既定检查，报告具体阻塞，不用升级依赖、降低检查或扩大文件范围掩盖。

## 实施步骤与执行切分

**单块，不拆。** 唯一共享基础是根命令表、输出路径和说明；五个文件均由同一实施者负责。并行拆分会反复修改同一命令表和 README，没有独立代码工作流可获益。

1. 按上述目标表新增 Makefile；先验证不带 Node/dist 的 Go 构建路径，再验证 Go 格式错误和下游错误能退出非零。
2. 新增只作参考的环境示例，追加忽略规则；不创建或读取实际环境文件，不自动安装 PG/创建库。
3. 更新两份 README；根说明作为完整启动/检查入口，前端说明保留专项细节。首次启动只用独立 `up`，撤销保持单独人工演练，不纳入任何默认目标。
4. 在新/一次性验收 checkout 按下面顺序安装和检查。真实 PG 验收仅在明确准备并授权使用的专用本地/测试库中执行；不得为验收接管现有实例或业务库。
5. 检查预期产物、源文件未被格式检查改写、依赖文件未漂移、日志不含敏感值。基础检查失败先修本任务入口/文档；若根因需越出文件清单，回报主代理。

## 可复制验收命令与结果

以下均供**未来实施验收**，本次规划未执行。除明确注明外从仓库根运行；`make ...` 为本任务待新增入口。Go、Node、npm、make 和 C 编译工具链由使用者预装；现有固定基线是 Go 1.27.3 / Node 24.22.1 / npm 11.19.1 / 本地 PG 18.6，不追逐最新版本。先对照版本，不匹配就停止并修正 PATH，不自动安装/下载另一工具链。

### A. 干净环境安装与独立构建（不需要 PG）

在没有 node_modules/dist 的新 checkout：

```sh
(
  set -eu
  GOTOOLCHAIN=local go version
  node --version
  npm --version
  GOTOOLCHAIN=local go mod download
  GOTOOLCHAIN=local go mod verify
  make help
  make build-go
  npm --prefix web ci
  npm --prefix web ls --depth=0
  make build-web
  make build
  test -x bin/tokenpivot-api
  test -x bin/tokenpivot-migrate
  test -x bin/tokenpivot
  test -f web/dist/index.html
  git diff --exit-code -- go.mod go.sum web/package.json web/package-lock.json
)
```

期望：Go 独立构建发生在前端安装之前并成功；npm 按已提交锁文件安装；独立前端及内嵌构建均成功，输出路径清楚，依赖声明/校验文件没有变化。`npm ci` 会替换已有 node_modules，只在新环境/已知可替换的依赖目录执行；不使用 `npm install`、force 或 legacy-peer-deps 补救验收。锁文件不匹配或完整性验证失败应停止，不静默重写锁文件。

### B. 基础检查集合（不需要 PG）

```sh
(
  set -eu
  unset TOKENPIVOT_DATABASE_URL TOKENPIVOT_MIGRATION_DATABASE_URL
  unset TOKENPIVOT_TEST_DATABASE_URL TOKENPIVOT_TEST_APP_DATABASE_URL
  unset TOKENPIVOT_DEV_API_TARGET
  make check
  git diff --exit-code -- go.mod go.sum web/package.json web/package-lock.json
)
```

期望：format、Go vet、前端 lint/typecheck、普通与内嵌 Go race 测试、真实代理测试、独立与内嵌构建全部执行并成功；无连接变量也不访问 PG。dist/bin 可以更新，但源码/依赖文件不被悄悄修复。检查日志必须可辨认执行阶段，任何失败使集合非零；不要求构建产物在失败时仍完整。

### C. 本地 Go + Vite 同源开发（显式需要专用本地 PG）

先按根 README 的专用本地准备步骤创建角色、数据库/schema，用凭证工具分别向迁移会话和服务会话安全注入对应变量；不在命令中展开连接值。使用 A 已构建的迁移二进制：

```sh
# 迁移会话：仅安全注入 TOKENPIVOT_MIGRATION_DATABASE_URL
# 仅在已明确准备的本地开发库执行，不是只读步骤
./bin/tokenpivot-migrate up
```

确认迁移退出 0 后，在单独的服务会话启动，不向服务进程注入迁移账号：

```sh
# 终端一：仅安全注入 TOKENPIVOT_DATABASE_URL
TOKENPIVOT_HTTP_ADDR=127.0.0.1:8080 make dev-go
```

```sh
# 终端二：只向 Vite 提供公开的上游地址
TOKENPIVOT_DEV_API_TARGET=http://127.0.0.1:8080 make dev-web
```

```sh
# 终端三：HTTP 层验收，不做 UI 自动化
(
  set -eu
  curl --fail --show-error --silent http://127.0.0.1:8080/healthz
  curl --fail --show-error --silent http://127.0.0.1:8080/readyz
  curl --fail --show-error --silent http://127.0.0.1:5173/ > /dev/null
  test "$(curl --show-error --silent --output /dev/null --write-out '%{http_code}' http://127.0.0.1:5173/api/probe)" = 404
)
```

期望：两个探针 200 且 GET 为 `ok` 换行；前端入口 200；`/api/probe` 当前为后端 404，不是 HTML 200，更不是业务成功。真实代理测试补充证明前缀边界、方法/状态/头保留及断连行为。Go 停止时 Vite 的 API 请求必须失败，不能回退页面。5173 被占用时 Vite 退出失败；Go 缺连接或无法连库时不监听；错误不回显配置值。不为这些验证停止共享数据库或未知服务。

### D. 内嵌二进制运行

先停止 C 的手工进程；保持服务连接安全注入，迁移已成功。将二进制复制到不含仓库/dist 的新临时目录，前台启动（无需 Node）：

```sh
(
  set -eu
  runtime_dir="$(mktemp -d)"
  cp bin/tokenpivot "$runtime_dir/tokenpivot"
  cd "$runtime_dir"
  TOKENPIVOT_HTTP_ADDR=127.0.0.1:18080 ./tokenpivot
)
```

另一终端执行：

```sh
(
  set -eu
  curl --fail --show-error --silent http://127.0.0.1:18080/readyz
  curl --fail --show-error --silent http://127.0.0.1:18080/ > /dev/null
  test "$(curl --show-error --silent --output /dev/null --write-out '%{http_code}' http://127.0.0.1:18080/api/probe)" = 404
)
```

期望分别为 200、200、404；真实 HTML/JS/CSS 由既有内嵌测试验证。可在无 Node 的运行环境重复此步骤。缺 dist 的原始 `go build -tags=uiembed` 必须编译失败，不能制作占位产物；正常 `make build` 会先生成真实 dist。构建与运行都不自动迁移。临时目录验收后按现有回收站规则人工处理，不新增 clean 目标。

### E. PG 联合集成检查与可诊断失败

先结束手工验收进程，按根 README 的专用 `_test` 库、双角色和密码认证说明准备环境，并独占该测试库。安全注入两个 TEST 连接变量，不能用开发/业务连接代替：

```sh
make test-integration
```

期望：真实执行联合标签测试而非 cached/skip；验证单连接迁移、事务/锁、最小权限、进程信号与真实断连恢复。测试仅操作本次随机 schema，已有无关对象不变；清理错误或归属不符必须失败并报告待人工处理的 schema，不扫描清库。

缺少配置的失败可直接复制验证，且不会接触数据库：

```sh
(
  unset TOKENPIVOT_TEST_DATABASE_URL TOKENPIVOT_TEST_APP_DATABASE_URL
  if make test-integration; then
    printf '%s\n' '错误：缺少测试配置却返回成功' >&2
    exit 1
  fi
)
```

此缺配置案例须看到 Go 集成用例实际执行并报 `explicit integration database variables required` 才算通过；前端构建等更早阶段失败不能替代配置校验。

还须在专用验收环境分别验证：仅一项 TEST 变量、非回环 IP/非 `_test` 库名、相同或越权角色、连接拒绝、认证错误。均运行同一目标并期望非零安全诊断；URL 安全校验应先于连接，真实网络/权限错误由既有测试处理。不得为构造失败指向业务库或改坏共享 PG 配置。

### F. 失败传播与非改写检查

| 场景（仅在一次性验收 checkout 构造） | 命令及期望 |
|---|---|
| 在已覆盖路径放一个合成的未格式化 Go 文件；运行前记录文件校验值。 | `make format-check` 非零并列出该文件；运行后校验值相同。纠正文件后成功。前端格式错误同理，Prettier 不改写。 |
| 合成 TS 类型错误或触发已有 ESLint 规则。 | 分别运行现有 `npm --prefix web run typecheck` / `npm --prefix web run lint`；`make check` 也非零，不绕过错误继续宣告成功。 |
| 在一次性 checkout 让包定义与锁文件不一致。 | `npm --prefix web ci` 失败，锁文件不被更新；不把这种试验带回工作分支。 |
| 在一次性 checkout 尚未生成 dist 时直接执行内嵌编译。 | `GOTOOLCHAIN=local go build -mod=readonly -tags=uiembed -o /tmp/tokenpivot-t051-missing-dist ./cmd/tokenpivot` 非零；随后 `make build` 可按顺序生成产物。 |
| 使用 `make -j check` / `make -j build`。 | 单个组合目标内仍先完成 web 再编译内嵌 Go；不承诺两个独立终端同时写同一产物安全。 |

只构造合成错误，不读取或复用真实配置文件；验证后按回收站规则移走合成文件，不直接删除、不覆盖原有未提交文件。

## 风险、待确认与回退

- 无需用户重新决定技术选型；本计划无影响范围的未决选择。是否实施、何时允许连接专用验收 PG，仍由主代理/用户另行授权。
- TODO(待确认，实施验收)：本轮没有安装依赖、启动服务、构建或运行测试。现有固定工具在新环境的可获得性、锁文件完整安装、全部既有检查能否通过，须通过 A–F 实测，不能把本文当作已通过报告。[go.sum](/Users/lxp/code-space/gitspace/TokenPivot/go.sum) 本轮只确认文件存在，未核对全部内容。
- race 缺 C 工具链、无可用回环监听权限、PG 认证命中 trust 或测试库未独占，都会导致相关检查失败；文档应定位真实前提，不跳过 race/PG 用例伪装成功。当前平台说明以 macOS/Linux 本地流程为边界，不新增 Windows 信号兼容方案。
- 构建不证明已迁移，readyz 不检查迁移版本，200 占位页不证明登录/业务可用。构建失败时旧产物可能仍在，不承诺原子产物发布。
- 本任务没有数据库格式/运行时变更。回退仅还原本任务的五个文件改动；不自动 down、不删除数据库/已有产物，不动本来未跟踪的文档。源文件或工具若出现范围外问题，停止并交主代理决定，不暗改版本或标准。

## 依据

### 仓库依据（2026-10-27，已读取；仅确认现状，不代表运行验收）

| 依据 | 采纳/排除 |
|---|---|
| [T-049 前置计划](/Users/lxp/code-space/gitspace/TokenPivot/docs/plans/2026-10-27-go-service-skeleton.md)、[T-050 前置计划](/Users/lxp/code-space/gitspace/TokenPivot/docs/plans/2026-10-27-database-bootstrap.md)、[T-031 前置计划](/Users/lxp/code-space/gitspace/TokenPivot/docs/plans/2026-10-27-frontend-scaffold-same-origin.md) | 继承已确认工具与职责；“当时未实施”及当时排除 Makefile 的描述不用于否认本条目范围。 |
| [架构:23–33](/Users/lxp/code-space/gitspace/TokenPivot/docs/architecture.md#L23-L33) | Go 单体 + PG、独立 web、开发代理、内嵌运行不需 Node、生产页面/API 同源；不增加 Redis/跨源方案。 |
| [根说明:7–63](/Users/lxp/code-space/gitspace/TokenPivot/README.md#L7-L63)、[配置:20–53](/Users/lxp/code-space/gitspace/TokenPivot/internal/config/config.go#L20-L53)、[迁移入口:36–55](/Users/lxp/code-space/gitspace/TokenPivot/cmd/tokenpivot-migrate/main.go#L36-L55) | 复用版本、手工 PG 准备、环境加载和独立迁移；status 可能初始化历史表，不视为绝对只读。 |
| [bootstrap:1–6](/Users/lxp/code-space/gitspace/TokenPivot/db/migrations/00001_bootstrap.sql#L1-L6)、[根说明:65–88](/Users/lxp/code-space/gitspace/TokenPivot/README.md#L65-L88) | 当前只有工具初始化，不承诺业务表、认证或 readyz 迁移校验。 |
| [web 包脚本:10–16](/Users/lxp/code-space/gitspace/TokenPivot/web/package.json#L10-L16)、[TS strict 范围](/Users/lxp/code-space/gitspace/TokenPivot/web/tsconfig.json#L1-L17)、[ESLint](/Users/lxp/code-space/gitspace/TokenPivot/web/eslint.config.js#L1-L20)、[Prettier 排除](/Users/lxp/code-space/gitspace/TokenPivot/web/.prettierignore#L1-L3) | 直接复用，不引入第二套格式/测试/类型配置。Go 文件不交给 Prettier。 |
| [web 专项说明:25–58](/Users/lxp/code-space/gitspace/TokenPivot/web/README.md#L25-L58)、[代理测试:57–188](/Users/lxp/code-space/gitspace/TokenPivot/web/test/proxy.test.mjs#L57-L188)、[内嵌测试:14–74](/Users/lxp/code-space/gitspace/TokenPivot/web/embed_test.go#L14-L74) | 复用代理边界及真实产物测试，不新增 UI 自动化、假业务 API 或前端测试框架。 |
| [数据库测试:18–115](/Users/lxp/code-space/gitspace/TokenPivot/internal/database/database_integration_test.go#L18-L115)、[迁移测试:29–126](/Users/lxp/code-space/gitspace/TokenPivot/cmd/tokenpivot-migrate/main_integration_test.go#L29-L126)、[服务测试:97–194](/Users/lxp/code-space/gitspace/TokenPivot/cmd/tokenpivot/main_integration_test.go#L97-L194) | 三处安全前提及随机 schema 清理已存在；只接入命令，修正文档过宽描述，不重构测试辅助函数。 |

### 外部依据

预算：只查“安装/模块文件保持”和“格式检查失败语义”两个问题，各最多两个一手来源，已停止；无新架构/依赖选择，因此不做无关社区/仓库横向调查。官方 Go GitHub 源码仅作为固定版本工具行为证据，不复制实现。

| 事实与来源 | 日期 / 核实状态 | 本计划采用方式 |
|---|---|---|
| [npm v11 的 npm ci](https://docs.npmjs.com/cli/v11/commands/npm-ci)：要求已有锁文件；定义不一致退出错误；不写包定义/锁文件，会替换已有 node_modules。 | 抓取 2026-10-27；官方文档已核实，固定 npm 的本地运行待实施验收。 | 首次安装用 ci；不以 install/冲突绕过参数修补锁文件。 |
| [Go Modules Reference](https://go.dev/ref/mod)：`-mod=readonly` 在 go.mod 需修改时失败；go mod download 预取已声明模块；go mod verify 验证缓存是否变化，不能代替全部模块/校验文件不变的断言。 | 抓取 2026-10-27；官方说明已核实，本仓模块实际安装未执行。 | 显式只读构建参数，加干净环境依赖文件 diff 验收；不把该参数描述成 go.sum 绝不会变化。 |
| [Go go1.27.3 gofmt 源码](https://github.com/golang/go/blob/go1.27.3/src/cmd/gofmt/gofmt.go#L272-L296)（[实际抓取的原始源码](https://raw.githubusercontent.com/golang/go/go1.27.3/src/cmd/gofmt/gofmt.go)）：`-l` 在差异时列文件，写入由 `-w` 控制，不能仅凭 `-l` 无命令错误认定格式通过。 | 抓取 2026-10-27；固定 tag 源码已核实，本仓格式目标未实现。 | shell 明确检查列表是否为空，保留工具错误，不写回。 |
| [Prettier CLI](https://prettier.io/docs/cli#exit-codes)：`--check` 只检查；格式不符退出 1，工具异常退出 2。 | 抓取 2026-10-27；官方说明已核实，固定 Prettier 的本地运行待实施验收。 | 复用已有 format:check 脚本并原样传播失败，不增加 --write 或额外插件。 |

获取说明：官方页面首次 web_fetch 因本机解析到非公网地址被拒；使用系统代理 curl 抓取同一来源成功，四次 curl 的退出码均为 0。未进行外部仓库克隆或代码引入。

### 本次实际读取与自检边界

仓库内容读取仅包括上述三份前置计划、架构、根 README/Go 模块/忽略规则、web 的包定义和锁文件头部/TS/Vite/ESLint/Prettier 排除/README、配置与两个 main、三个 integration 测试（服务与迁移测试只读相关片段及定向搜索）、bootstrap SQL、web 的 proxy.test.mjs/embed/noembed/embed_test。另加载指定 skills 及其直接工具说明；[go.sum](/Users/lxp/code-space/gitspace/TokenPivot/go.sum)、Makefile、根示例和 scripts 仅定向确认存在性。未读真实环境文件、凭证、无关文档或业务数据，未全项目扫描。

本轮唯一源文件写入为本文；未修改 TODO、未提交、未切分支/建立 worktree，未安装依赖、启动服务或执行数据库写入验证。主代理评审后仅提交本文，并负责将 T-051 置 planned，保留 `架构: docs/architecture.md`，追加本文 plan 引用；这不授权实施。无需更新 AGENTS.md：本轮仅形成计划，实际命令尚未落地，且不在授权写入范围。
