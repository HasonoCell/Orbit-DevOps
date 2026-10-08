FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS build
WORKDIR /src
ARG GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH=amd64
# 一个不可变制品内保留独立入口；进程权限、资源和伸缩在部署层分开，而不是一个进程运行全部职责。
RUN set -e; for component in api release-worker build-worker pipeline-worker gateway-worker identity-admin migrate; do \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' \
      -o /out/orbit-devops-$component ./cmd/orbit-devops-$component; \
    done

# 静态 Go 可执行文件无需发行版、Shell 或包管理器；仍携带可信 CA 供 GitHub/OIDC HTTPS 使用。
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
WORKDIR /tmp
ENTRYPOINT ["/usr/local/bin/orbit-devops-api"]
