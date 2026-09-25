#!/usr/bin/env bash
# Compose 端到端冒烟:空环境起控制面 + demo-mcp(streamable_http 远端后端),
# 注册 -> 等 ready -> 经 /mcp 调用 demo.echo 断言 -> 收尾 down -v。
#
# 用法:scripts/compose-e2e.sh
# 需要:docker compose v2、可用的 daemon;需要能构建镜像(会编译 Go 代码)。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/smoke-lib.sh
. "$repo_root/scripts/smoke-lib.sh"

export AGENTNEXUS_SMOKE_KEY="${AGENTNEXUS_SMOKE_KEY:-$(openssl rand -hex 16)}"
SMOKE_BASE_URL="${SMOKE_BASE_URL:-http://127.0.0.1:8080}"
SMOKE_API_KEY="$AGENTNEXUS_SMOKE_KEY"

cleanup() {
	log "docker compose down -v"
	docker compose -f "$repo_root/docker-compose.yml" down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

cd "$repo_root"
log "docker compose up -d --build"
docker compose up -d --build

log "wait for /health/ready"
wait_http "$SMOKE_BASE_URL/health/ready" 180 || fail "control plane did not become ready"

log "register demo-mcp (streamable_http over the compose network)"
id=$(register_server '{"name":"demo-mcp","transport":"streamable_http","runtime":{"type":"remote","remote":{"endpoint":"http://demo-mcp:8080/mcp"}}}')
wait_phase "$id" ready 60
log "server $id is ready"

log "tools/list via /mcp"
session=$(mcp_open_session)
[ -n "$session" ] || fail "MCP initialize returned no session id"
mcp_initialized "$session"
listing=$(mcp_post "$session" '{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}')
expect_contains "$listing" 'demo-mcp.demo.echo' "aggregated catalog"

log "tools/call demo-mcp.demo.echo"
call=$(mcp_call_tool "$session" 'demo-mcp.demo.echo' '{"message":"compose-e2e"}')
expect_contains "$call" 'compose-e2e' "tools/call result"

log "compose e2e passed"
