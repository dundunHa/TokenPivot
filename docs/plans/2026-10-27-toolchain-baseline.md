# T-052：统一工具链版本基线（Go / Node / npm / PostgreSQL）

> 日期：2026-10-27，Asia/Shanghai。状态：**版本决策已确认，规划定稿；已按用户 2026-10-27 指示取消 Node LTS 前置门槛，改为以本机当前实测版本为基线，可实施。**
>
> 主代理负责计划审阅/提交、实施授权及 TODO 更新。本子代理仅写本计划，不修改配置、业务代码、待办、历史计划或 git 状态。

## 1. 目标与非目标

统一工具声明和当前使用说明：Go **1.26.4**、Node **26**（`engines.node: >=26.0.0`）、npm **随 Node 26 分发，本机 11.20.0**、PostgreSQL **18.4**。这三组版本选择均已由用户确认，不再作为待确认问题。

**核心需求是把声明的工具依赖版本对齐到本机当前实际环境**，既不是升级，也不是等待未来发布。

**实施前置检查（2026-10-27 用户调整）**：原计划要求「官方把 v26 某一补丁标为 LTS 后才动手」。用户已明确：核心需求是把工具依赖版本降到当前环境的版本，可以直接调整计划执行。因此**取消官方 LTS 标记这一门槛**，改为以本机实测版本为准，四项同时满足才动手：

- `GOTOOLCHAIN=local go version` 为 1.26.4；
- `node --version` 主版本为 26（本轮实测 v26.11.1）；
- `npm --version` 与同一 Node 安装随包（本轮实测 11.20.0）；
- `psql --version` 与 `pg_config --version` 为 18.4。

任一不符就停止回报，不安装、不自动升级、不切换版本。官方发布状态只作记录，不再是前置条件。LTS 标记属于「某一次发布」的元数据，不随日期自动出现在已装二进制上；本机现有 v26.11.1 是 Current 发布，更换二进制不在本任务范围。[E1、E2]

非目标：业务/API、数据库结构或迁移、数据目录、依赖升级、CI/部署、容器、版本管理器、新版本检查框架。数据库操作、全局工具安装、凭据读取均不在本任务授权范围内。历史计划不批量改写。[架构:23–33](</Users/lxp/code-space/gitspace/TokenPivot/docs/architecture.md#L23-L33>)中的部署边界不变。

## 2. 仓库与本机证据

### 仓库现状（本轮已读取）

| 位置 | 当前声明 / 行为 |
|---|---|
| [go.mod:3–18](</Users/lxp/code-space/gitspace/TokenPivot/go.mod#L3-L18>) | `go 1.27.3`；pgx `v5.11.0`、goose `v3.29.0`；间接依赖均有版本；无 `toolchain` 指令。 |
| [package.json:6–9](</Users/lxp/code-space/gitspace/TokenPivot/web/package.json#L6-L9>) | `packageManager: npm@11.19.1`；Node engines 为 `>=24.22.1 <25`。 |
| [package.json:30](</Users/lxp/code-space/gitspace/TokenPivot/web/package.json#L30>) | `@types/node: 24.19.2`，是编译时类型包，不是实际 Node 执行版本。 |
| [锁文件:1–33](</Users/lxp/code-space/gitspace/TokenPivot/web/package-lock.json#L1-L33>) | `lockfileVersion: 3`；根包 Node engines 同上。本轮只检查头部，不声称审查过完整依赖树。 |
| [Makefile:1–4](</Users/lxp/code-space/gitspace/TokenPivot/Makefile#L1-L4>)、[Makefile:22–86](</Users/lxp/code-space/gitspace/TokenPivot/Makefile#L22-L86>) | 注释要求 Go 1.27.3 / Node 24.22.1 / npm 11.19.1；Go recipe 已使用 `GOTOOLCHAIN=local`、`-mod=readonly`；普通检查与数据库集成检查分离。 |
| [根说明:7](</Users/lxp/code-space/gitspace/TokenPivot/README.md#L7>)、[根说明:37](</Users/lxp/code-space/gitspace/TokenPivot/README.md#L37>)、[根说明:187](</Users/lxp/code-space/gitspace/TokenPivot/README.md#L187>)、[前端说明:11](</Users/lxp/code-space/gitspace/TokenPivot/web/README.md#L11>) | 重复写有旧 Go/Node/npm 数字；根说明本地 PG 验收版本为 18.6。 |

常见版本声明文件的定名查找未命中 `.nvmrc`、`.node-version`、`.npmrc`、`.go-version`、`.tool-versions`、`mise.toml`、`.mise.toml`。现有包字段与说明已足够，本次不新增其中任何一个。

### 本机只读版本命令（2026-10-27）

| 命令 | 实测结果 | 证据边界 |
|---|---|---|
| `GOTOOLCHAIN=local go version` | `go version go1.26.4 darwin/arm64`，退出 0；命令位置 `/opt/homebrew/bin/go` | 本轮仅执行版本查询，无工具链下载。 |
| `node --version` | `v26.11.1`，退出 0 | 来自现有 fnm 会话目录；不要求其他人采用 fnm。官方该补丁发布记录为 Current，不能当作已发布 LTS。[E2] |
| `npm --version` | `11.20.0`，退出 0 | 与 Node 命令位于同一会话目录；这是当前本机值，不是未来版本固定要求。 |
| `psql --version` | `psql (PostgreSQL) 18.4`，退出 0；命令位置 `/opt/homebrew/opt/libpq/bin/psql` | 客户端版本，不能冒充实际服务端。 |
| `pg_config --version` / `pg_config --bindir` | `PostgreSQL 18.4` / `/opt/homebrew/Cellar/libpq/18.4/bin`，均退出 0 | libpq 安装证据，不是运行实例证据。 |
| `command -v postgres` | PATH 未找到，记录为 127 | 沿该缺失项定名查找 `/opt/homebrew/opt/postgresql*/bin/postgres` 未命中。不能据此断言整机没有服务端；本轮不连接数据库补证据。 |

Go 1.26.4 下 build/vet/test（含 `-tags=uiembed`）、gofmt、`go.sum` 无漂移与锁文件重算差异的原始记录见 [版本核实记录 §4](</Users/lxp/code-space/gitspace/TokenPivot/docs/research/2026-10-27-toolchain-version-baseline.md#L74-L102>)，那是在隔离副本里取得的。**本子代理未在仓库内重跑这些项目检查**，不能把它写成本轮规划新取得的执行日志。

本轮未运行安装、构建、测试或服务端验收。Go 官方模块代理已核实 pgx/goose 的最低 Go 分别为 1.25.0、1.26.0；完整依赖图与项目兼容性的最终验收仍用现有检查，而非仅凭两个模块头部。[E5、E6]

## 3. 已确认决策及声明职责

| 工具 | 最终决定 | 表达方式与理由 |
|---|---|---|
| Go | **1.26.4** | go 指令从 1.27.3 改为 1.26.4。**go 指令是最低要求，新机器使用 1.26.10 也满足，不把 1.26.10 写成目标。** 本机验收使用 Homebrew 1.26.4；保留现有 `GOTOOLCHAIN=local`，本机不再依赖模块缓存里的 1.27.3。不新增配置或指令。[E3] |
| Node | **26**；engines 为 **`>=26.0.0`** | 严格采用用户指定范围，不擅自追加 `<27` 或某个补丁下限。该范围也允许更高主版本，但本任务只按 Node 26 验收（LTS 时间门槛见第 1 节），不宣称已验证其他主版本。精确 LTS 补丁在实施时如实记录，不在今天预造。[E1、E2] |
| npm | **随 Node 26 分发（本机 11.20.0）** | **删除现有 `packageManager` 字段**：它原本单独选择精确 npm，而用户已明确不单独钉版本；不写不合法的“随包”版本字符串，也不另加 engines.npm、devEngines 或管理器。文档使用本行表述；11.20.0 是观测值，不是硬性补丁锁。 |
| PostgreSQL | **18.4** | 当前声明由 18.6 改为 18.4，按用户决定执行。文档明确本轮只实测客户端/开发库工具 18.4，**实际运行服务端仍未核实**。不因此连接、创建、替换或降级任何实例。 |

机器声明的职责：go 指令负责最低 Go/语言版本；包定义负责 Node 支持范围；锁文件负责依赖树与根包元数据，不负责锁 Node/npm 二进制。两份说明与 Makefile 注释是同步展示，不再承担独立的版本选择。人工验收比较这几处值即可，不增加第二份清单或解析框架。`engines` 默认是建议性字段，不承诺它会自动阻止错误环境。[E9]

### 历史计划与 Node 类型包

- **历史计划不改。** [数据库历史计划:44](</Users/lxp/code-space/gitspace/TokenPivot/docs/plans/2026-10-27-database-bootstrap.md#L44>)明确写的是当时的版本建议，其他段落也是当时的实施说明，不是当前持续维护的版本入口。本计划和现行说明取代其工具版本口径；不将历史文本当作现行验收依据。因此不修改历史计划，也不逐行替换其中的 18.6。根说明增加本计划链接即可解释来源变化。[Go 历史计划:34–35](</Users/lxp/code-space/gitspace/TokenPivot/docs/plans/2026-10-27-go-service-skeleton.md#L34-L35>)同理保留。[历史检查计划:94](</Users/lxp/code-space/gitspace/TokenPivot/docs/plans/2026-10-27-dev-build-basic-checks.md#L94>)写着「现有固定基线是 Go 1.27.3 / Node 24.22.1 / npm 11.19.1 / 本地 PG 18.6」，是三份历史计划里最像现行口径的一段，但它同样已归档；保留原文，由本计划取代其工具版本口径，根说明增加本计划链接即解释来源变化。
- **`@types/node` 不纳入本任务。** 它是编译时 API 类型，不是工具运行版本；本任务不增加 Node 26 API 或修改类型使用方式，为主版本数字一致而升级它会额外改变依赖图，超出本次工具声明统一。保留 **24.19.2**，明确这是与运行时主版本不一致的已知状态；交主代理记录后续 TODO“评估并对齐 Node 26 类型声明”，本子代理不新建 TODO。若现有类型检查因此失败，停止并报告具体错误，不静默升级类型包或其他依赖。

## 4. 最小改动清单

**单次实施只允许修改以下六个现有文件；除本计划外无新文件、新依赖或新配置。** 无 API、schema、数据流或迁移变化。

| 文件 | 具体改动 |
|---|---|
| [go.mod](</Users/lxp/code-space/gitspace/TokenPivot/go.mod>) | 仅第 3 行改为 `go 1.26.4`，所有 require 版本保持不变；不加 toolchain。 |
| [package.json](</Users/lxp/code-space/gitspace/TokenPivot/web/package.json>) | 删除 `packageManager: npm@11.19.1`；`engines.node` 改为 `>=26.0.0`。脚本及全部依赖不变，包括 `@types/node: 24.19.2`。 |
| [package-lock.json](</Users/lxp/code-space/gitspace/TokenPivot/web/package-lock.json>) | 用现有随包 npm 的 `npm install` 锁文件模式重算根包 engines 的相关四行元数据范围，与包定义一致；仍为 lockfileVersion 3。不手改依赖条目，不顺带升级任何直接或间接依赖。 |
| [Makefile](</Users/lxp/code-space/gitspace/TokenPivot/Makefile>) | 第 3 行 Go 改为 1.26.4，Node 改为 26，npm 改为随 Node 26 分发（本机 11.20.0）。所有 recipe、失败退出、`GOTOOLCHAIN=local`、`-mod=readonly` 与数据库边界不变；不新增版本检查目标。 |
| [根 README](</Users/lxp/code-space/gitspace/TokenPivot/README.md>) | 第 7/37 行 Go 改为 1.26.4；第 37 行 PG 改为 18.4；第 7/187 行 Node 改为 26、npm 改为随包表述。说明 Go 是最低要求，1.26.10 也满足但不是目标；本机不再依赖模块缓存里的 1.27.3。保留安装/启动/凭据边界，补一句 PG 18.4 仅有客户端证据及本计划链接，不宣称已测服务端。 |
| [前端 README](</Users/lxp/code-space/gitspace/TokenPivot/web/README.md>) | 第 11 行 Go 改为 1.26.4，Node 改为 26 / engines `>=26.0.0`，npm 改为随 Node 26 分发（本机 11.20.0）；说明不单独锁 npm。简短标注 Node 类型包仍为 24.19.2、后续单独评估，链接本计划。其余代理/构建语义不动。 |

[go.sum](</Users/lxp/code-space/gitspace/TokenPivot/go.sum>)不在改动范围。禁止以 tidy/get、删锁重装、force、legacy-peer-deps 或批量升级来通过检查。实际文件行号若因并行工作变化，以已核实字段定位，不扩大修改范围。

## 5. 实施顺序与停止条件

1. **环境准备（不属于六文件改动）。** 前端检查需要 `web/node_modules`，而本仓库当前没有。在改包定义之前执行一次 `npm --prefix web ci`：它只生成被 gitignore 的 `web/node_modules`，不改写 `web/package.json` 与 `web/package-lock.json`；先装再改可避免包定义与锁文件临时不一致而报错。网络或依赖不可用时直接报告环境阻塞，不进入后续步骤。
2. **确认本机版本，再改文件。** 执行第 6 节 A 的版本检查。只要 Go 不是 1.26.4、Node 主版本不是 26、npm 不是同一 Node 安装随包，或现有工具/依赖未就绪，就停止回报，不安装、不自动升级，也不在后台等待。官方 `lts` 字段只作记录，不再是门槛（2026-10-27 用户调整）。
3. 将 Go 最低要求与前端两项声明按第 4 节修改。保持所有依赖版本不变。
4. **实施中的锁文件生成步骤（不是验收安装步骤）**：用当前 Node 26 随包 npm 执行：

   ```sh
   npm --prefix web install --package-lock-only --ignore-scripts
   ```

   这是 `npm install` 的仅锁文件模式，只重算锁文件元数据，不安装依赖、不运行安装生命周期脚本。[E10] 审阅根包 engines 的四行相关差异；不能以“只有几行”代替内容检查。允许差异仅为根包 engines 与包定义同步，格式仍为 3；**任何依赖版本、resolved、integrity 或依赖关系变化都必须停止**，不得顺手接受 npm 重算造成的升级。
5. 同步 Makefile 注释、两份现行说明。历史计划不动。然后仅使用已具备的依赖执行下节验收；验收里不运行 npm ci、不安装工具、不下载缺失依赖。
6. 主代理用 `git status --porcelain` 确认只多出六文件与本计划（`docs/todo.md` 等本任务开始前已有的改动不算），审阅锁文件差异与真实检查结果，明确 PG 服务端未核实等未执行项，再按批准范围更新 TODO。不得将他人待办/归档修改带入本任务提交。

**数据库硬边界**：PG 18.6→18.4 的声明变更不授权任何数据目录升级、降级、复用、迁移、清库或删除，不替换运行实例二进制，不执行 dump/restore、initdb、pg_upgrade、迁移、服务启动或集成测试。官方说明同一主版本的小版本保持存储格式兼容，[E8] 因此这里是权限与数据安全限制，不是虚构“降级技术上必然需要迁移”。

其他停止条件：批准版本和实际命令不符；完整依赖最低 Go 高于 1.26.4；模块校验文件漂移；现有检查失败；需要改业务/测试源码、TS 配置或依赖才兼容；需触及六文件外内容；实施范围内存在他人未提交修改。均报告具体错误，不兜底切换工具或降低检查标准。

回退只撤销本次声明与说明改动，不安装旧工具、不操作数据库、不清理既有数据或产物。

## 6. 可复制验收（不连数据库，不安装工具）

以下供未来实施使用，本子代理未执行。工具与项目依赖必须事先就绪（`web/node_modules` 按第 5 节步骤 1 准备）；缺失即报告环境阻塞，不把它当成改动引入的失败。所有命令从仓库根执行；验收命令本身不含 npm ci/install、go mod download、服务启动或数据库连接。

### A. 实施前版本检查

运行：

```sh
GOTOOLCHAIN=local go version
node --version
npm --version
command -v node
command -v npm
psql --version
pg_config --version
```

预期：本机 Go 为 1.26.4；新机器 1.26.10 也满足最低要求，不因此改写目标。Node 主版本为 26（本轮实测 v26.11.1，不要求二进制自带 LTS 标记）；npm 为同一 Node 安装随包版本，本轮实测 11.20.0，不断言必须等于该补丁号。psql/pg_config 为 18.4，只计作客户端/开发库工具证据；不据此写服务端验收通过。任一前置不符立即停，不继续执行后面的修改/检查。

### B. 修改后声明一致性

```sh
node <<'NODE'
const assert = require('node:assert/strict');
const p = require('./web/package.json');
const l = require('./web/package-lock.json');
assert.equal(p.engines.node, '>=26.0.0');
assert.equal(Object.hasOwn(p, 'packageManager'), false);
assert.deepEqual(l.packages[''].engines, p.engines);
assert.equal(l.lockfileVersion, 3);
assert.equal(p.devDependencies['@types/node'], '24.19.2');
console.log('工具声明一致；Node 类型包按范围决定保持不变');
NODE
grep -x 'go 1.26.4' go.mod
make help
git diff --check -- go.mod web/package.json web/package-lock.json Makefile README.md web/README.md
git diff --exit-code -- go.sum
git diff -- go.mod web/package.json web/package-lock.json Makefile README.md web/README.md
```

预期：断言与命令退出 0；go 指令为 1.26.4；说明的 Go/Node/npm/PG 口径与第 3 节一致；锁文件仅根 engines 元数据变化；历史计划、依赖图和校验文件不变。帮助仍只打印清单。不要用「全仓不得出现 1.27.3」这类机械否定断言：README 会保留一句解释「本机不再依赖模块缓存里的 1.27.3」。

### C. 现有检查，无安装验收

```sh
before="$(git hash-object go.mod go.sum web/package.json web/package-lock.json)" &&
npm --prefix web ls --depth=0 &&
GOPROXY=off make check &&
after="$(git hash-object go.mod go.sum web/package.json web/package-lock.json)" &&
test "$before" = "$after"
```

预期：各阶段退出 0，普通与内嵌 Go 构建/race/vet、Go 格式、前端类型/格式/lint/测试通过；前后四文件哈希相同。`GOPROXY=off` 仅作用于这次验收命令，防止检查下载缺失模块，不写入配置；现有 Makefile 保留 `GOTOOLCHAIN=local`。模块缓存不全或 `web/node_modules` 未按步骤 1 准备会导致检查失败，此时报告环境阻塞，不借机安装。

普通检查不需要 PG，其中前端测试可能临时监听回环端口，不是持久服务或 UI 自动化。本任务**不执行 `make test-integration`**。上述命令不能证明真实 PG 服务端版本、运行状态或数据兼容性；这些明确保留为未验收项，不用版本声明修改冒充数据库验证。

## 7. 执行切分

**单块，不拆。** 一个 dev 串行维护第 4 节六文件，顺序为环境准备（一次 `npm ci`）→ LTS 前置检查 → Go/包声明 → npm 仅锁文件重算 → 两份说明/Makefile 注释 → 无安装验收。无需共享基础或并行分支。

该块独占前端包定义与锁文件；其他人不得用另一版 npm 重生成，主代理不在合并后另行批量重算。无迁移编号、注册表或生成代码。验收为第 6 节与锁文件仅元数据变化；PG 数据操作始终不在范围。

## 8. 依据

外部研究仅覆盖 Node 发布状态、Go 声明/发布与直接依赖最低要求、PG 版本政策、npm 字段及锁文件行为；每个子问题至多三个来源。没有新的架构或实现方案取舍，不做形式性 GitHub 模板调查、不复制参考实现。官方 Node Release 仓库只作日程证据。

web_fetch 对这些域名因非公网解析错误未取到正文，后用系统 curl 获取相同 URL，全部请求退出 0。“已核实”仅指官方文本/接口，不等于本机项目验收。用户最终选择优先于官方“当前补丁”建议，不据研究擅自改回已拍板版本。

| 编号 | 事实 / 采用或拒绝的内容 | 官方 URL | 日期 | 状态 |
|---|---|---|---|---|
| E1 | v26 的计划 LTS 日为 2026-10-28；采用人工前置检查，不把日程当完成。 | [Node 发布日程](<https://raw.githubusercontent.com/nodejs/Release/main/schedule.json>) | 抓取 2026-10-27 | 已核实日程；实际进入 LTS 尚未核实。 |
| E2 | 26.11.1 的发布记录是 Current，不能提前称作 LTS。 | [Node 26.11.1](<https://nodejs.org/en/blog/release/v26.11.1>) | 发布 2026-10-07；抓取 2026-10-27 | 已核实；未来 LTS 精确补丁未核实。 |
| E3 | go 指令是最低版本；必须不低于依赖要求；local 使用本地工具链，遇到更高最低要求会失败。保留已有用法，不新增工具链配置。 | [Go toolchains](<https://go.dev/doc/toolchain>) | 抓取 2026-10-27 | 已核实语义；项目检查结果区分交接报告与本轮执行。 |
| E4 | 发布接口返回稳定版本 go1.27.3、go1.26.10；用户选择的本机 1.26.4 不能称为当前补丁，也不因此把目标改成 1.26.10。 | [Go 发布接口](<https://go.dev/dl/?mode=json>) | 抓取 2026-10-27 | 已核实；未调查旧补丁漏修的具体缺陷/安全项。 |
| E5 | pgx v5.11.0 的 go 指令为 1.25.0。 | [固定模块声明](<https://proxy.golang.org/github.com/jackc/pgx/v5/@v/v5.11.0.mod>) | 抓取 2026-10-27 | 已核实；不是完整依赖图证明。 |
| E6 | goose v3.29.0 的 go 指令为 1.26.0，要求 pgx v5.11.0、x/sync v0.24.0。 | [固定模块声明](<https://proxy.golang.org/github.com/pressly/goose/v3/@v/v3.29.0.mod>) | 抓取 2026-10-27 | 已核实；最终采用现有项目检查验收。 |
| E7 | PG 官方当前 18 系列补丁为 18.6，建议使用当前补丁；本次仍按用户明确选择声明 18.4，不谎称它是当前补丁。 | [PG 版本政策](<https://www.postgresql.org/support/versioning/>) | 抓取 2026-10-27 | 已核实官方建议；本机实际服务端未核实。 |
| E8 | 同一 PG 主版本的小版本保持内部存储格式兼容；这不构成本任务的数据操作授权。 | [PG 18 升级说明](<https://www.postgresql.org/docs/18/upgrading.html>) | 抓取 2026-10-27 | 已核实通用说明；具体实例适用性未核实。 |
| E9 | engines 默认是建议字段；不据它宣称自动强制环境。用户不再单独锁 npm，因此移除精确 packageManager，不新增 devEngines。 | [npm v11 包字段](<https://docs.npmjs.com/cli/v11/configuring-npm/package-json/>) | 抓取 2026-10-27 | 已核实字段语义；具体随包 npm 的完整组合待验收。 |
| E10 | package-lock-only 只更新锁文件而不安装依赖；ignore-scripts 禁用安装生命周期脚本。仅用于实施中的元数据重算，不加入验收安装步骤。 | [npm v11 install](<https://docs.npmjs.com/cli/v11/commands/npm-install/>) | 抓取 2026-10-27 | 已核实选项；本轮未运行该命令。 |

## 9. 遗留事项与交接

- **无版本选择待用户确认。** 实施前须确认本机四项版本（Go 1.26.4 / Node 26 / npm 随包 / PG 18.4）与第 1 节一致（第 5 节步骤 2），并记录所用的精确 Node 补丁及随包 npm。官方 LTS 标记已按用户 2026-10-27 指示取消，不再作为前置门槛；没有自动等待或升级任务。
- 未核实：未来 Node LTS 发布事实，未来随包 npm 与项目的完整组合，PG 实际服务端及运行状态；本子代理未在仓库内重跑交接中报告通过的 Go 检查。正式验收按第 6 节记录实际结果，不把旧记录冒充本次执行。
- 已知不一致：`@types/node` 24.19.2 对运行时 Node 26；明确留给后续独立 TODO，不在本任务改依赖图。
- 给 doc-review 的最小材料：本计划与第 4 节六文件；锁文件先核头部和实际差异，外部事实按第 8 节 URL 核对即可。无需读待办、归档、凭据、完整环境或无关历史。重点检查已定版本、LTS 前置检查、npm 不单独锁、历史计划保留理由、类型包后续事项、PG 无数据操作以及验收无安装。
- 无需新增或更新项目规则文件：本轮只规划单次版本调整，规则和命令入口未改变；长期口径留在现行说明，不另建重复常驻规则。
