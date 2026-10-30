# TokenPivot 部署文档：bf 单机 Docker Compose

本文只描述已获批准的 `tokenpivot-bf` 部署：BF 本机（hostname `DESKTOP-C28DFS7`）上用 Docker Compose 运行 TokenPivot 单体镜像（Go 服务 + 内嵌 React 页面）和独立 PostgreSQL 18.6。来源计划见 [2026-10-29-bf-docker-deployment.md](plans/2026-10-29-bf-docker-deployment.md)，入口文件为 [Dockerfile](../Dockerfile)、[.dockerignore](../.dockerignore)、[compose.yaml](../compose.yaml)。

本文不授权任何执行；每次变更仍由主代理按执行 ID 授权并核对。本文不含真实密码、连接串或令牌。

## 1. 目标、环境与边界

| 项 | 值 |
|---|---|
| 项目名 | `tokenpivot-bf`（独立网络 `tokenpivot-bf-backend`，独立卷 `tokenpivot-bf-pgdata-18`） |
| 访问地址 | 唯一浏览器 Origin `http://DESKTOP-C28DFS7:18080`，端口绑定 `0.0.0.0:18080:18080` |
| 来源范围 | 不限来源网段。只能声称“绑定所有主机接口”，不能声称仅局域网、已验证公网可达或已实现来源隔离 |
| 数据库 | PostgreSQL 18.6，容器内主机名 `db`，数据库 `tokenpivot`，schema `tokenpivot`；不发布宿主机端口 |
| 内部网络 | `192.168.240.0/24`，网关 `192.168.240.1`；与其他项目网络、卷、容器不复用 |
| 旧资源 | 全部旧容器、卷、网络、镜像保持原状，不停止、不重建、不修改；本部署不是其清理前置步骤 |

固定版本引用（不要改成 `latest`）：Node `node:26.11.1-bookworm@sha256:253729d3f2b77dbab6a5694aa20da4b484bc759bfb7004aa2e0b683c390e825e`、Go `golang:1.26.4-bookworm@sha256:5a94593d87a066df5abb02969be911524963f53908292aa5a1a6096fc019012a`、PostgreSQL `postgres:18.6-bookworm@sha256:41a369edc91d9bdd7a58be6bc71ea0f8f7a3f49e82818a9f0829021ea7840b7c`。

已批准的窄例外：只有本套单机、专用 Compose 网络内部允许数据库连接使用 `sslmode=disable`，前提是数据库不发布宿主机端口、其他项目不加入该网络、凭证不跨主机传输。其他远程数据库连接仍要求 `sslmode=verify-full` 和可信 CA，不因“同机”说法降级。

HTTP 的已知风险：唯一 Origin 是明文 HTTP，凭证、会话 Cookie 和页面内容可在链路上被窃听或篡改；`TOKENPIVOT_ALLOW_INSECURE_HTTP=true` 只放开“非回环 http Origin”这一条限制，不改变监听范围、不开放注册，也不是 HTTPS 的安全等价物。会话 Cookie 按主机而非端口区分，同主机其他端口也可能收到。

不在本文范围：HTTPS/反向代理、CI/CD、镜像仓库、自动重启策略、定时备份、监控日志平台、PgBouncer、Redis；旧资源处置；数据库迁移回滚或删卷。

## 2. 前置条件与凭证引用

工具与版本（执行前重新核对，预检值为 Docker `29.1.3`、Compose `2.40.3`）：

- BF 本机：Docker 与 `docker compose`（v2）、`curl`、`psql` 可用；构建机另需 `git`。
- 构建镜像所需的 Go 1.26.4 / Node 26 由 Dockerfile 固定镜像提供，不在宿主机安装 Node；宿主源码树只用于 `docker build`。
- 端口 18080 空闲；`DESKTOP-C28DFS7` 解析到当前 BF；`192.168.240.0/24` 未被占用。任何一项不符即停止，不换端口、不换网段、不加第二个 Origin。

源码与镜像前提：

- `TOKENPIVOT_SOURCE_COMMIT` 必须是最终合入后的完整 40 位提交；构建前工作树干净且 `git rev-parse HEAD` 与之相等。不能拿旧基线提交冒充最终提交。
- 不从文档工作区或含未提交改动的工作区构建，不上传源码。
- 发布前检查在固定提交上实际通过：`make check`；并在独立专用 PostgreSQL 18.6 `_test` 库上执行 `make test-integration`（生产库不承担集成测试，缺测试环境则发布停止）。

凭证只以环境变量或受限文件引用，不写入仓库、命令参数、shell 历史或日志：

| 名称 | 用途 | 说明 |
|---|---|---|
| `TOKENPIVOT_SOURCE_COMMIT` | Compose 选择镜像 `tokenpivot:<提交>` | 公开值 |
| `TOKENPIVOT_DATABASE_URL` | app 进程连接，服务角色 `tokenpivot_app` | 完整 URL，密码由本地凭证工具注入；含 `host=db`、`search_path=tokenpivot`、`sslmode=disable` |
| `TOKENPIVOT_MIGRATION_DATABASE_URL` | 迁移进程连接，角色 `tokenpivot_owner` | 只注入迁移进程；不进入 app 容器，不回退到服务 URL |
| `TOKENPIVOT_PG_PASSWORD_FILE` | PostgreSQL 超级用户初始密码文件 | 固定为 `/root/.tokenpivot-bf/secrets/postgres-password`，权限 600；只引用路径，不读取或输出内容。 |

非敏感 URL 模板（`<密码安全注入>` 由凭证工具替换，勿手写真实值）：

```text
postgres://tokenpivot_owner@db:5432/tokenpivot?sslmode=disable&search_path=tokenpivot
postgres://tokenpivot_app@db:5432/tokenpivot?sslmode=disable&search_path=tokenpivot
```

密码文件与备份目录在使用前创建，权限仅限部署账号；密码文件内容不打印。备份目录固定为 `/root/.tokenpivot-bf/backups/`，目录权限 700。

## 3. 构建镜像并记录版本

在固定提交的干净工作树上执行（镜像名与提交绑定，不 push）：

```sh
git rev-parse HEAD   # 必须等于 TOKENPIVOT_SOURCE_COMMIT
docker build --platform linux/amd64 \
  --build-arg SOURCE_COMMIT=<完整40位提交> \
  -t tokenpivot:<完整40位提交> .
docker image inspect tokenpivot:<完整40位提交> \
  --format '{{.Id}} {{index .Config.Labels "org.opencontainers.image.revision"}}'
```

Dockerfile 内部会执行现有 `make build`，并检查两个二进制、时区数据与 CA 证书存在；缺一项即构建失败，不得换成 `latest` 兜底。

记录：完整提交、镜像 ID、构建退出码与时间。**最终提交与镜像 ID TODO(待确认)**（实施合入后填写）。

构建需要访问镜像仓库与依赖源；若构建机通过出网代理访问，这属于构建环境条件，由该环境提供，不写进 Dockerfile、镜像或本仓库，也不作为本项目的默认要求。

## 4. 创建新实例与初始化数据库

1. 变更前记录旧资源基线（只记非敏感元数据）：`docker ps --format '{{.Names}}\t{{.ID}}\t{{.Status}}'`、`docker volume ls -q`。发现本项目所需的名称、端口、挂载冲突时停止，不处置旧资源。
2. 创建受限密码文件（路径见 TODO），并确认其只对部署账号可读。
3. 分阶段注入变量：启动数据库只需 `TOKENPIVOT_SOURCE_COMMIT` 与 `TOKENPIVOT_PG_PASSWORD_FILE`，两个数据库 URL 此时可以不设置（compose 允许缺省，缺 URL 的 app / migrate 二进制启动即失败，不会带空 URL 运行）；迁移阶段再注入迁移 URL，应用阶段再注入服务 URL，彼此不混入。只启动数据库（不要运行不带服务名的 `docker compose up`，不要运行会打印插值后密钥的 `docker compose config`）：

```sh
docker compose up -d db
docker compose exec db psql -U postgres -d tokenpivot -c 'SELECT version();'
```

`SELECT version()` 必须显示 PostgreSQL 18.6（镜像标签不能代替该结果）。数据库就绪前重复上面的 psql 检查，不要用 `docker compose up` 的重试代替。

4. 在 db 容器内以超级用户交互执行初始化（密码用 `\password` 交互输入，不写入命令参数）：

```sql
CREATE ROLE tokenpivot_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
CREATE ROLE tokenpivot_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
\password tokenpivot_owner
\password tokenpivot_app
REVOKE ALL ON DATABASE tokenpivot FROM PUBLIC;
GRANT CONNECT ON DATABASE tokenpivot TO tokenpivot_owner, tokenpivot_app;
\connect tokenpivot
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA tokenpivot AUTHORIZATION tokenpivot_owner;
GRANT USAGE ON SCHEMA tokenpivot TO tokenpivot_app;
```

`tokenpivot_app` 必须在迁移 `up` 之前存在：迁移逐个建表时会向这个固定角色授权，不预授未来表权限。owner 拥有 schema，app 只有 CONNECT/USAGE，迁移账号不是超级用户。

5. 记录本次创建步骤的实际结果（容器 ID、卷名、网络名、数据库版本），不要输出容器完整环境。

## 5. 独立迁移（唯一执行 ID）

迁移不在 `docker compose up` 的默认路径里（`migrate` 在 `migration` profile 中），必须显式执行一次。每次迁移分配唯一执行 ID 并写入一次性容器名：

```sh
docker compose --profile migration run \
  --name tokenpivot-bf-migrate-<唯一执行ID> migrate
```

- 保留该一次性容器供审计，**不使用 `--rm`**；记录执行 ID、开始/结束时间、退出码和日志中的版本行（`applied` 或 `no change`）。
- 成功后才允许创建/启动 app。重复 `up` 成功并记录 `no change`。
- 失败时 app 保持未启动。先核对原执行：`docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' tokenpivot-bf-migrate-<执行ID>` 只看状态与退出码，禁止不带 `--format` 输出完整环境；再在 owner 连接下核对迁移历史与对象（`status` 可能创建历史表，README 已说明；结果未明前不得再次 `up`）。不自动 `down-one`、reset、force、删库或删卷。
- 生产优先前向修复；迁移失败或提交结果未知时，先查原执行，不重复触发。

迁移后核对：实际迁移版本与固定源码一致；app 容器没有 `TOKENPIVOT_MIGRATION_DATABASE_URL`；数据库 host 连接规则为 SCRAM（compose 已设 `POSTGRES_HOST_AUTH_METHOD=scram-sha-256`），不使用 `trust`。

## 6. 启动应用与验收

```sh
docker compose up -d app
curl --fail --show-error --silent http://127.0.0.1:18080/healthz
curl --fail --show-error --silent http://127.0.0.1:18080/readyz
curl --fail --show-error --silent http://DESKTOP-C28DFS7:18080/healthz
```

`healthz` 只证明 HTTP 正常，`readyz` 才证明数据库 Ping 成功；`readyz` 不检查迁移版本或业务表。两者都 200 才算就绪，healthz 成功而 readyz 失败仍算未就绪。

验收项（全部记录退出码与实际输出，不记录任何 Cookie 或令牌）：

1. **实际版本**：确认运行中的应用就是固定镜像——

   ```sh
   docker image inspect tokenpivot:<完整40位提交> \
     --format '{{.Id}} {{index .Config.Labels "org.opencontainers.image.revision"}}'
   docker inspect --format '{{.Image}}' "$(docker compose ps -q app)"
   ```

   容器镜像 ID 与构建记录一致，标签 revision 等于完整提交。

2. **健康与页面**：上面三个探针 200；HEAD 无正文、不支持的方法 405、`/healthz/` 与 `/readyz/` 404；页面与静态资源经唯一 Origin 可访问。

3. **错误 Origin 仍被拒绝**（不带真实凭证、不写库）：

   ```sh
   curl -i -X POST -H 'Origin: http://example.invalid' \
     --data '{}' http://127.0.0.1:18080/api/v1/auth/login
   ```

   预期 403。成功 Cookie 的属性只在用户做首次真实注册时观察，不为测试制造首个生产用户。

4. **端口与数据面**：`docker port "$(docker compose ps -q app)"` 显示 `0.0.0.0:18080`；`docker port "$(docker compose ps -q db)"` 为空；卷为 `tokenpivot-bf-pgdata-18` 且挂载到 `/var/lib/postgresql`，旧卷未被挂载。不要把“绑定所有接口”描述成私网绑定或来源隔离。

5. **旧资源未变**：与第 4 节基线比较容器名称/ID/状态和卷列表，8080/8081 原占用者保持原状；基线若有漂移，先记录并确认无本项目冲突。

6. **其他来源**：如另有客户端可用，从该客户端复核同一 Origin；不可用时明确记为未验证，不据此推断公网或局域网可达性。

## 7. 恢复与回滚限制

- 需要撤回时，经批准只停止本项目应用：`docker compose stop app`。数据库容器、应用容器、卷、日志和备份都保留，数据不删除。
- 重新启动需另行批准；回滚到旧镜像、重建容器、执行迁移回退、删除资源同样需另行批准。
- 数据库已迁移时，不承诺旧应用兼容；优先前向修复，不自动 down、不删库、不删卷。
- 禁止用 `docker compose down`、`docker compose rm`、`docker rm`、`docker prune`、`--rm` 绕过保留要求。

## 8. 初次验收后备份（路径待定）

用 owner 凭证执行自定义格式 `pg_dump`，输出到批准的仓库外受限目录（**路径 TODO(待确认)**）；业务备份含密码哈希与会话摘要，按敏感数据处理，不打印内容。记录 PG 版本、源码提交、迁移版本、时间和 SHA-256，并用 `pg_restore --list` 检查可读性。`pg_restore --list` 只证明归档可读，不替代恢复验证；恢复验证需另行批准的隔离空实例，不对现有对象使用覆盖或清理选项。后续每次迁移前另行备份与审批，本文不设定时任务。

## 9. 执行记录表（每次发布填写）

| 项 | 值 |
|---|---|
| 固定源码提交 | TODO(待确认) |
| 镜像 ID / revision 标签 | TODO(待确认) |
| 构建时间与退出码 | |
| 旧资源基线记录时间 | |
| DB 创建执行 ID / 时间 | |
| 迁移执行 ID / 时间 / 退出码 / 版本 | |
| app 启动时间 / 容器 ID | |
| healthz / readyz / Origin 403 / 版本核对结果 | |
| 旧资源对比结果 | |
| 未验证项 | 其他客户端可达性、防火墙/路由转发、实际暴露范围；按当时实际记录 |

## 10. 已知未核实事项

- 最终完整提交与镜像 ID：TODO(待确认)，发布执行时记录。
- 镜像摘要可拉取性、固定摘要构建、scratch 中两个二进制与 Compose profile 行为，须由实际构建和分阶段启动验收证明；未验证前不得声称已通过。
- Windows/Hyper-V 防火墙与路由器 18080 转发未纳入本方案；不新增来源限制，也不保证任何来源的可达或不可达。
- 内部 `sslmode=disable` 的批准范围仅限本套单机专用网络；不适用于其他远程连接。