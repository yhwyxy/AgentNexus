#!/usr/bin/env bash
# docker runtime 冒烟(宿主机直跑 + 本机 daemon),覆盖:
#   (a) stdio + docker:只读挂载生效 -> ready -> 经 /mcp 用 demo.readfile 读到挂载内容
#   (b) streamable_http + docker:端口发布 + 就绪探测 -> ready -> demo.echo
#   (c) 允许清单外挂载 / 容器内目标越界 -> 注册返回 400 invalid_argument
#   (d) SIGKILL 控制面后重启 -> 按标签接管既有容器(容器 ID 不变)
#   (e) SIGTERM -> 容器全部被回收,无残留 agentnexus-* 容器
#
# 用法:scripts/docker-provider-smoke.sh
# 需要:本机 daemon、docker CLI、Go 工具链。会构建并运行本地镜像 agentnexus-demo-mcp:local。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/smoke-lib.sh
. "$repo_root/scripts/smoke-lib.sh"

demo_image="agentnexus-demo-mcp:local"
work_dir="$(mktemp -d)"
mount_dir="$(mktemp -d)"
http_addr="127.0.0.1:18080"
SMOKE_BASE_URL="http://$http_addr"
export AGENTNEXUS_SMOKE_KEY="$(openssl rand -hex 16)"
SMOKE_API_KEY="$AGENTNEXUS_SMOKE_KEY"
docker_host="${DOCKER_HOST:-unix:///var/run/docker.sock}"
server_pid=""

printf 'mounted-payload\n' >"$mount_dir/probe.txt"

stop_server() { # signal
	if [ -n "$server_pid" ] && kill -0 "$server_pid" 2>/dev/null; then
		kill "-$1" "$server_pid" 2>/dev/null || true
		wait "$server_pid" 2>/dev/null || true
	fi
	server_pid=""
}

cleanup() {
	stop_server TERM
	docker ps -aq --filter 'name=agentnexus-' | xargs -r docker rm -f >/dev/null 2>&1 || true
	rm -rf "$work_dir" "$mount_dir"
}
trap cleanup EXIT

start_server() {
	"$repo_root/bin/agentnexus" -config "$work_dir/agentnexus.yaml" >"$work_dir/server.log" 2>&1 &
	server_pid=$!
	wait_http "$SMOKE_BASE_URL/health/ready" 30 || {
		cat "$work_dir/server.log" >&2
		fail "control plane did not become ready"
	}
}

container_ids() { # label selector value -> ids
	docker ps -aq --filter "label=agentnexus.server-id=$1"
}

log "check docker daemon"
docker info >/dev/null 2>&1 || fail "no docker daemon available"

log "build binaries and demo image"
cd "$repo_root"
go build -o bin/agentnexus ./cmd/agentnexus
go build -o bin/demo-mcp ./cmd/demo-mcp
docker build -q -f Dockerfile.demo-mcp -t "$demo_image" . >/dev/null

log "write smoke config to $work_dir/agentnexus.yaml"
cat >"$work_dir/agentnexus.yaml" <<YAML
server:
  httpAddress: "$http_addr"
  shutdownTimeout: 10s
database:
  path: "$work_dir/agentnexus.db"
runtime:
  reconcileInterval: 10s
  docker:
    host: "$docker_host"
    connectHost: 127.0.0.1
    publishHost: 127.0.0.1
    stopGrace: 2s
security:
  apiKeys:
    - name: smoke
      role: admin
      keyEnv: AGENTNEXUS_SMOKE_KEY
  hostAccess:
    mounts:
      - host: "$mount_dir"
        container: /workspace
        access: ro
YAML

start_server

log "(c) out-of-allowlist mount is rejected with 400"
body=$(register_server_status '{"name":"outside-root","transport":"stdio","runtime":{"type":"docker","docker":{"image":"'"$demo_image"'","command":["-transport","stdio"],"mounts":[{"source":"'"$work_dir"'","target":"/workspace","readOnly":true}]}}}' 400)
expect_contains "$body" 'invalid_argument' "out-of-allowlist mount error code"

log "(c) container target outside the allowed root is rejected with 400"
body=$(register_server_status '{"name":"outside-target","transport":"stdio","runtime":{"type":"docker","docker":{"image":"'"$demo_image"'","command":["-transport","stdio"],"mounts":[{"source":"'"$mount_dir"'","target":"/data","readOnly":true}]}}}' 400)
expect_contains "$body" 'invalid_argument' "outside-target mount error code"

log "(a) register stdio docker backend with a read-only bind mount"
stdio_id=$(register_server '{"name":"docker-stdio","transport":"stdio","runtime":{"type":"docker","docker":{"image":"'"$demo_image"'","command":["-transport","stdio"],"mounts":[{"source":"'"$mount_dir"'","target":"/workspace","readOnly":true}]}}}')
wait_phase "$stdio_id" ready 60
log "server $stdio_id is ready"

session=$(mcp_open_session)
[ -n "$session" ] || fail "MCP initialize returned no session id"
mcp_initialized "$session"
call=$(mcp_call_tool "$session" 'docker-stdio.demo.readfile' '{"path":"probe.txt"}')
expect_contains "$call" 'mounted-payload' "bind mount visibility through demo.readfile"

log "(b) register streamable_http docker backend with a published port"
http_id=$(register_server '{"name":"docker-http","transport":"streamable_http","runtime":{"type":"docker","docker":{"image":"'"$demo_image"'","command":["-transport","http","-addr","0.0.0.0:8080","-path","/mcp"],"port":8080,"endpointPath":"/mcp"}}}')
wait_phase "$http_id" ready 60
log "server $http_id is ready"

session=$(mcp_open_session)
[ -n "$session" ] || fail "MCP initialize returned no session id"
mcp_initialized "$session"
call=$(mcp_call_tool "$session" 'docker-http.demo.echo' '{"message":"docker-http-ok"}')
expect_contains "$call" 'docker-http-ok' "http docker backend tool call"

stdio_container=$(container_ids "$stdio_id")
stdio_container=$(printf '%s' "$stdio_container" | head -1)
[ -n "$stdio_container" ] || fail "no running container for server $stdio_id"
log "container for $stdio_id: $stdio_container"

log "(d) SIGKILL then restart: existing containers are adopted, not recreated"
stop_server KILL
start_server
wait_phase "$stdio_id" ready 60
adopted=$(container_ids "$stdio_id" | head -1)
[ "$adopted" = "$stdio_container" ] || fail "container changed after restart: $stdio_container -> ${adopted:-<none>}"

log "(e) SIGTERM reclaims every managed container"
stop_server TERM
deadline=$((SECONDS + 30))
while ((SECONDS < deadline)); do
	left=$(docker ps -aq --filter 'name=agentnexus-')
	[ -z "$left" ] && break
	sleep 1
done
left=$(docker ps -aq --filter 'name=agentnexus-')
[ -z "$left" ] || fail "containers still present after shutdown: $left"

log "docker provider smoke passed"
