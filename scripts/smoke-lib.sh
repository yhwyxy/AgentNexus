#!/usr/bin/env bash
# 冒烟脚本共享辅助函数,被 compose-e2e.sh / docker-provider-smoke.sh source。
# 调用方需先设置:SMOKE_BASE_URL(控制面地址)、SMOKE_API_KEY(admin 角色密钥)。
# 依赖:curl、awk、grep、sed(均为 POSIX 工具,不使用 python/jq)。

log() { printf '\n== %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

smoke_curl() { curl -sS --max-time 10 "$@"; }

wait_http() { # url timeout_seconds
	local url=$1 deadline=$((SECONDS + $2))
	while ((SECONDS < deadline)); do
		if curl -fsS --max-time 2 "$url" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	return 1
}

api_get() { # path
	smoke_curl "$SMOKE_BASE_URL$1" -H "Authorization: Bearer $SMOKE_API_KEY"
}

server_phase() { # id
	api_get "/api/v1/mcp-servers/$1" | grep -o '"phase":"[a-z]*"' | head -1 | cut -d'"' -f4
}

wait_phase() { # id expected_phase timeout_seconds
	local id=$1 want=$2 deadline=$((SECONDS + $3)) phase=""
	while ((SECONDS < deadline)); do
		phase=$(server_phase "$id" || true)
		if [ "$phase" = "$want" ]; then return 0; fi
		sleep 1
	done
	fail "server $id phase = ${phase:-<empty>}, want $want; status: $(api_get "/api/v1/mcp-servers/$id")"
}

register_server() { # json -> id (失败即退出)
	local body=$1 location
	location=$(smoke_curl -D- -o /dev/null -X POST "$SMOKE_BASE_URL/api/v1/mcp-servers" \
		-H "Authorization: Bearer $SMOKE_API_KEY" -H 'Content-Type: application/json' -d "$body" |
		tr -d '\r' | awk 'tolower($1)=="location:"{print $2}')
	[ -n "$location" ] || fail "registration returned no Location header"
	printf '%s' "${location##*/}"
}

register_server_status() { # json expected_status -> response body
	local body=$1 want=$2 out status
	out=$(mktemp)
	status=$(smoke_curl -o "$out" -w '%{http_code}' -X POST "$SMOKE_BASE_URL/api/v1/mcp-servers" \
		-H "Authorization: Bearer $SMOKE_API_KEY" -H 'Content-Type: application/json' -d "$body")
	[ "$status" = "$want" ] || fail "registration status = $status, want $want; body: $(cat "$out")"
	cat "$out"
	rm -f "$out"
}

# --- MCP over /mcp:initialize -> notifications/initialized -> tools/call ---

mcp_open_session() {
	smoke_curl -D- -o /dev/null -X POST "$SMOKE_BASE_URL/mcp" \
		-H "Authorization: Bearer $SMOKE_API_KEY" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agentnexus-smoke","version":"1.0"}}}' |
		tr -d '\r' | awk 'tolower($1)=="mcp-session-id:"{print $2}'
}

mcp_post() { # session_id body -> data payload (SSE)
	smoke_curl -X POST "$SMOKE_BASE_URL/mcp" \
		-H "Authorization: Bearer $SMOKE_API_KEY" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-H "Mcp-Session-Id: $1" \
		-d "$2" | grep '^data:' | sed 's/^data: //'
}

mcp_initialized() { # session_id
	smoke_curl -o /dev/null -X POST "$SMOKE_BASE_URL/mcp" \
		-H "Authorization: Bearer $SMOKE_API_KEY" \
		-H 'Content-Type: application/json' \
		-H 'Accept: application/json, text/event-stream' \
		-H "Mcp-Session-Id: $1" \
		-d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
}

mcp_call_tool() { # session_id tool_name arguments_json -> data payload
	mcp_post "$1" "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":$3}}"
}

expect_contains() { # haystack needle context
	case "$1" in
	*"$2"*) ;;
	*) fail "$3: expected to find $2 in: $1" ;;
	esac
}
