# 工具链版本基线核实（2026-10-27）

本文件为 T-052「统一工具链版本基线」的规划前置记录。**只记已核实的事实与本机实测结论，不含变更建议**；是否切换基线由 T-052 规划决定。

## 一、仓库当前声明的基线

| 工具 | 声明版本 | 声明位置 |
|---|---|---|
| Go | 1.27.3 | `go.mod:3`、`README.md:7`、`README.md:37`、`Makefile:3`、`web/README.md:11` |
| Node | 24.22.1（engines `>=24.22.1 <25`） | `web/package.json:8`、`web/package-lock.json` root `packages[""].engines`、`README.md:7`、`README.md:187`、`web/README.md:11`、`Makefile:3` |
| npm | 11.19.1 | `web/package.json:6`（`packageManager`）、`README.md:7`、`Makefile:3`、`web/README.md:11` |
| PostgreSQL | 18.6（本地验收） | `README.md:37`、`docs/plans/2026-10-27-dev-build-basic-checks.md:94` |

这些基线来自官方发布元数据查询，不是来自 skill：
- Go：`go.dev/dl/?mode=json` 返回 `go1.27.3`、`stable=true`，采用明确补丁号；`docs/plans/2026-10-27-go-service-skeleton.md:34` 记录该决定，同文件 `:125` 明确写了「不能用技能里的版本文字代替发布来源」。
- Node / npm：`nodejs.org/dist/index.json` 选 Node 24 LTS 线、随包 npm 11.19.1，明确「不追逐 npm latest 12」；记录见 `docs/plans/2026-10-27-frontend-scaffold-same-origin.md:27` 与 `:214`。
- PostgreSQL：`docs/plans/2026-10-27-database-bootstrap.md:44`。

skill 侧只给大方向、不给补丁号：
- `~/.agents/skills/lang-go/SKILL.md:10`：「默认按最新稳定版编写（截至 2026-10：Go 1.27、pgx v5.11）；新项目创建时 Go 与依赖直接使用最新稳定版。项目 go.mod 版本更低时按项目版本」。
- `~/.agents/skills/lang-ts-react/SKILL.md`：通篇没有版本、LTS 或 engines 约定，Node 版本这条它管不着。

## 二、官方发布数据（抓取时间 2026-10-27 UTC）

### Go

来源 `https://go.dev/dl/?mode=json`，返回前两项：

| 版本 | stable | files |
|---|---|---|
| go1.27.3 | true | 45 |
| go1.26.10 | true | 45 |

`go1.27.3` 是当前最新稳定版；`1.26` 线已到 `1.26.10` 补丁级。

### Node.js

来源 `https://nodejs.org/dist/index.json`，各主版本最新一项：

| 主版本 | 最新版本 | lts | 发布日期 |
|---|---|---|---|
| v26 | v26.12.1 | false | 2026-10-21 |
| v25 | v25.9.0 | false | 2026-03-31 |
| v24 | v24.22.1 | "Krypton" | 2026-10-20 |
| v23 | v23.11.1 | false | 2025-05-14 |
| v22 | v22.23.4 | "Jod" | 2026-10-20 |

v26 线共 19 个发布，**其中标记 LTS 的数量为 0**。

来源 `https://raw.githubusercontent.com/nodejs/Release/main/schedule.json`（经 `curl` 获取）：

```
v24 -> {"start":"2025-05-06","lts":"2025-10-28","maintenance":"2026-10-20","end":"2028-04-30","codename":"Krypton"}
v25 -> {"start":"2025-10-15","maintenance":"2026-04-01","end":"2026-06-01"}
v26 -> {"start":"2026-05-05","lts":"2026-10-28","maintenance":"2027-10-20","end":"2029-04-30","codename":""}
```

据此：Node 24 已于 **2026-10-20** 进入 maintenance（早于本记录 7 天）；Node 26 计划 **2026-10-28** 转 LTS（本记录次日），当前仍为 Current。

> 注：`raw.githubusercontent.com` 经 `web_fetch` 被拒（域名解析到非公网地址），改用 `curl` 取得同一内容。

## 三、本机实际版本

| 工具 | 本机版本 | 路径 | 与声明基线的关系 |
|---|---|---|---|
| Go | 1.26.4 | `/opt/homebrew/bin/go` → `../Cellar/go/1.26.4/bin/go` | 比基线**旧**一个小版本 |
| Go（模块内） | 1.27.3 | `/Users/lxp/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.darwin-arm64/bin` | 与基线一致，由 `GOTOOLCHAIN=auto` 自动切换得到 |
| Node | v26.11.1 | fnm multishell 目录 | 比基线**新**，超出 `>=24.22.1 <25` 上界 |
| npm | 11.20.0 | 随 Node 26 分发 | 比基线新 |
| PostgreSQL | 18.4 | `psql --version` / `pg_config --version` | 比基线**旧**（基线 18.6） |

`go env GOTOOLCHAIN` 在本机为 `auto`。因此在模块内执行 `go version` 得到 1.27.3，在模块外（如 `/tmp`）得到 1.26.4。

## 四、本机实测结论（隔离副本中修改版本后复跑，未改动仓库）

做法：`git archive HEAD | tar -x` 到临时目录，在该副本内改版本后复跑，原 worktree 保持干净。

### Go 1.26.4（把 go.mod 改成 `go 1.26.4`）

| 检查 | 结果 |
|---|---|
| `go build -mod=mod ./...` | 通过 |
| `go vet -mod=readonly ./...` | 通过 |
| `go test -mod=readonly -race -count=1 ./...` | 全部包通过 |
| `go build -tags=uiembed`（真实 dist） | 通过 |
| `go test -tags=uiembed -race -count=1 ./...` | 全部包通过 |
| `go vet -tags=uiembed ./...` | 通过 |
| `go.sum` 是否漂移 | **完全一致**，无差异 |
| `gofmt -l` 输出与 1.27.3 对比 | **逐字相同**（两版均为空输出） |

反向确认：`go.mod` 保持 1.27.3、用机器 1.26.4 加 `GOTOOLCHAIN=local` 时直接拒绝构建，输出
`go: go.mod requires go >= 1.27.3 (running go 1.26.4; GOTOOLCHAIN=local)`。

### Node 26 / npm 11.20（把 engines 改成 `>=26.0.0`）

| 检查 | 结果 |
|---|---|
| `npm ci` | 通过，`added 174 packages`，**无 EBADENGINE 警告** |
| `npm run lint` | 通过（19 个文件，fail 0） |
| `npm run test` | 通过 |
| `npm run build` | 通过 |
| `package-lock.json` 变化 | `npm ci` **不会**改写 root engines；需 `npm install` 重算，重算后 diff 为 4 行，只涉及 root `packages[""].engines` |

依赖树里**没有**任何第三方包声明 Node 上界低于 26；唯一带 `<25` 上界的是本仓自己的 root package。

## 五、未验证项

- 未在 Node 26 上验证浏览器端行为、原生依赖（`fsevents` 等）的运行时表现；只跑了 lint/test/build。
- 未验证 PostgreSQL 18.4 与 18.6 的差异对本仓迁移与集成测试的影响；本机没有任何 TokenPivot 数据库凭证，未连接数据库。
- 未验证 Go 1.26.4 下 `GOTOOLCHAIN=auto` 与 `local` 在 CI 环境（无模块缓存）的行为差异；本机 1.27.3 来自既有模块缓存。
- 未尝试任何降级/升级安装动作，以上结论均限于「本机现有工具能否通过现有检查」。
