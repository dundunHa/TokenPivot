#!/usr/bin/env bash
# 无秘密的 compose 分阶段解析测试：输出一律丢弃，不打印插值结果。
set -euo pipefail
cd "$(dirname "$0")/.."
export TOKENPIVOT_SOURCE_COMMIT=0000000000000000000000000000000000000000
export TOKENPIVOT_PG_PASSWORD_FILE=/nonexistent/synthetic-password-file
unset TOKENPIVOT_DATABASE_URL TOKENPIVOT_MIGRATION_DATABASE_URL
c() { docker compose "$@" >/dev/null; }
c config db
TOKENPIVOT_DATABASE_URL=synthetic c config app
TOKENPIVOT_MIGRATION_DATABASE_URL=synthetic c --profile migration config migrate
if (unset TOKENPIVOT_SOURCE_COMMIT; c config db) 2>/dev/null; then echo "SOURCE_COMMIT 缺失应失败" >&2; exit 1; fi
echo ok
