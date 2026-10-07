# 构建阶段：在容器内编译，本机不必装 Go —— 在 NAS 上直接 docker build 就行。
#
# go.mod 声明 go 1.25.1，基础镜像必须 >= 1.25，否则 build 直接报
# "go.mod requires go >= 1.25.1 (running go 1.24.x)"
FROM golang:1.25-alpine AS builder

# 支持产出多架构镜像（NAS 常见为 ARM64，如绿联/群晖 Arm 机型）。
# 本项目用纯 Go 的 SQLite（glebarez），CGO 关闭，因此交叉编译零障碍：
#   BuildKit：docker build --platform linux/arm64 -t wechat-profile-bot:arm64 .
#   老版 docker 不支持 --platform 自动传参时，显式给：--build-arg TARGETARCH=arm64
# 不设 TARGETARCH 时默认按 amd64 构建，保持与旧行为一致。
ARG TARGETARCH=amd64
ARG TARGETOS=linux

WORKDIR /app

# 国内网络直连 Go 模块镜像源（容器内一般没有宿主机的代理可用）；
# 海外构建环境可删掉这行或改回 proxy.golang.org
ENV GOPROXY=https://goproxy.cn,direct

# 只拷 go.mod/go.sum 先下载依赖：源码改动不会让这个缓存层失效，重建更快
COPY go.mod go.sum ./
RUN go mod download

# go:embed 需要的资源必须一并拷入，漏一个就会构建失败（都是踩过的坑）：
#   static/          网页管理界面（漏了报 pattern static: no matching files found）
#   prompts/         内置默认提示词模板（v5.1.0，漏了报 pattern prompts: ...）
#   tests/evaldata/  AI 评测黄金用例（v7.0.0，漏了报 pattern tests/evaldata/...）
COPY *.go ./
COPY static ./static
COPY prompts ./prompts
COPY tests/evaldata ./tests/evaldata

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o wechat-profile-bot .

# 运行阶段：最小镜像
FROM alpine:3.20

# su-exec 用于入口脚本把进程降权到非 root（比 gosu 小得多）；
# tzdata + TZ 保证容器内时间戳不退回 UTC（否则作息/统计会整体偏 8 小时）。
# 注意：healthcheck 用的 nc 由 alpine 自带的 busybox 提供，**不要往这里加 `nc` 包**
# （Alpine 无此包名，会让整个镜像构建直接失败）。
RUN apk --no-cache add ca-certificates tzdata su-exec

ENV TZ=Asia/Shanghai

WORKDIR /app

COPY --from=builder /app/wechat-profile-bot .

# 入口脚本：对齐 /config 属主后降权运行。
# 用 RUN chmod 而不是 COPY --chmod，避免依赖 BuildKit。
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh

# 降权使用的 UID/GID，默认 1000；NAS 上请对齐宿主机执行 docker 的用户（id -u / id -g）
ENV PUID=1000
ENV PGID=1000

# 数据与配置目录（卷挂载持久化）。程序检测到 /config 存在就把
# config.json / 数据库 / 凭据 / 日志都放在这里；不读取业务环境变量，
# 仅首次启动时用 WEPB_* 引导生成 config.json（之后以文件与网页设置为准）。
VOLUME ["/config"]

EXPOSE 17965

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
