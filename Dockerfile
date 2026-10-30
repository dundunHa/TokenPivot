# TokenPivot 单体镜像（linux/amd64）：Go 服务 + 内嵌 React，外加独立迁移二进制。
# 构建输入全部固定到官方镜像的 linux/amd64 清单摘要，不使用 latest。
# 构建命令（先确认工作树干净且 HEAD 与 SOURCE_COMMIT 一致）：
#   docker build --platform linux/amd64 --build-arg SOURCE_COMMIT=<40位提交> -t tokenpivot:<40位提交> .

ARG NODE_IMAGE=node:26.11.1-bookworm@sha256:253729d3f2b77dbab6a5694aa20da4b484bc759bfb7004aa2e0b683c390e825e
ARG GO_IMAGE=golang:1.26.4-bookworm@sha256:5a94593d87a066df5abb02969be911524963f53908292aa5a1a6096fc019012a

FROM --platform=linux/amd64 ${NODE_IMAGE} AS node

FROM --platform=linux/amd64 ${GO_IMAGE} AS build
# 只借用 Node/npm 安装；其余仍用 Go 构建器，以便直接运行现有 make build。
COPY --from=node /usr/local/ /usr/local/
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64
WORKDIR /src

# 构建开始即校验提交必须是 40 位十六进制，避免用短哈希或分支名打标签。
ARG SOURCE_COMMIT
RUN printf '%s' "${SOURCE_COMMIT}" | grep -Eq '^[0-9a-f]{40}$' || { echo 'SOURCE_COMMIT must be a 40-hex commit' >&2; exit 1; }

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY web/package.json web/package-lock.json ./web/
RUN npm --prefix web ci

COPY . .
RUN go version && node --version && npm --version \
    && make build \
    && test -x bin/tokenpivot && test -x bin/tokenpivot-migrate \
    && test -s /usr/share/zoneinfo/Australia/Lord_Howe \
    && test -s /etc/ssl/certs/ca-certificates.crt

FROM scratch
ARG SOURCE_COMMIT
LABEL org.opencontainers.image.revision="${SOURCE_COMMIT}"
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /src/bin/tokenpivot /tokenpivot
COPY --from=build /src/bin/tokenpivot-migrate /tokenpivot-migrate
USER 65532:65532
EXPOSE 18080
ENTRYPOINT ["/tokenpivot"]
