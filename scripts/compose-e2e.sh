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

log "management API: list / update / lifecycle"
listing=$(api_get /api/v1/mcp-servers)
expect_contains "$listing" '"count":1' "list count after registration"
expect_contains "$listing" "$id" "list contains the registered server"

# PUT:全量替换可变配置,revision 1 -> 2;stale revision 必须被拒。
revision=$(server_revision "$id")
updated=$(api_put "/api/v1/mcp-servers/$id" \
	"{\"revision\":$revision,\"displayName\":\"Demo MCP (updated)\",\"labels\":{\"tier\":\"smoke\"},\"runtime\":{\"type\":\"remote\",\"remote\":{\"endpoint\":\"http://demo-mcp:8080/mcp\"}},\"timeouts\":{\"connectSeconds\":7,\"listSeconds\":10,\"callSeconds\":60},\"limits\":{\"maxInFlight\":16}}" 202)
expect_contains "$updated" '"revision":2' "update bumps revision"
expect_contains "$updated" '"connectSeconds":7' "update applied the new timeout"
wait_phase "$id" ready 60

stale=$(api_put "/api/v1/mcp-servers/$id" \
	"{\"revision\":$revision,\"displayName\":\"Demo MCP (stale)\",\"runtime\":{\"type\":\"remote\",\"remote\":{\"endpoint\":\"http://demo-mcp:8080/mcp\"}}}" 409)
expect_contains "$stale" '"code":"conflict"' "stale revision rejected"

# :stop 是同步终态 —— 工具随即从聚合目录中消失。
api_action "$id" stop 200 | grep -q '"phase":"stopped"' || fail ":stop did not report phase=stopped"
stopped=$(mcp_post "$session" '{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}')
case "$stopped" in
*'demo-mcp.demo.echo'*) fail "tools/list still exposes tools of a stopped server" ;;
esac
log "stopped server is no longer routable"

api_action "$id" start 202 >/dev/null
wait_phase "$id" ready 60
call=$(mcp_call_tool "$session" 'demo-mcp.demo.echo' '{"message":"after-restart"}')
expect_contains "$call" 'after-restart' "tools/call after :start"

# disable 优先级高于 desiredState:禁用后 :start 必须被拒(409)。
api_action "$id" disable 200 | grep -q '"enabled":false' || fail ":disable did not report enabled=false"
disabled=$(api_action "$id" start 409)
expect_contains "$disabled" '"code":"conflict"' "start on a disabled server rejected"
api_action "$id" enable 202 >/dev/null
wait_phase "$id" ready 60

revision=$(server_revision "$id")
api_action "$id" restart 202 >/dev/null
wait_phase "$id" ready 60
[ "$(server_revision "$id")" = "$revision" ] || fail ":restart must not change revision"

log "compose e2e passed"
