# AgentNexus 控制面镜像:多阶段构建,运行阶段只带静态二进制与 CA 证书。
# 数据库固定放在 /data(compose 里用命名卷),因此容器可无状态重建。
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/agentnexus ./cmd/agentnexus

FROM alpine:3.21

# 远端 MCP 后端可能是 https,运行阶段必须有根证书。
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 agentnexus \
    && mkdir -p /data \
    && chown agentnexus:agentnexus /data

COPY --from=build /out/agentnexus /usr/local/bin/agentnexus

USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/agentnexus"]
CMD ["-config", "/etc/agentnexus/config.yaml"]
