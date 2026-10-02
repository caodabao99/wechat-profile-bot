# 构建阶段
# go.mod 声明 go 1.25.1，基础镜像必须 >= 1.25，否则 build 直接报
# "go.mod requires go >= 1.25.1 (running go 1.24.x)"
FROM golang:1.25-alpine AS builder

WORKDIR /app

# 安装依赖（如果需要 CGO 则加 gcc/musl-dev，但 glebarez/sqlite 是纯 Go 的，不需要）
COPY go.mod go.sum ./
RUN go mod download

# go:embed 需要 static/ 目录（网页管理界面），缺了会报 pattern static: no matching files found
COPY *.go ./
COPY static ./static
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o wechat-profile-bot .

# 运行阶段
FROM alpine:3.20

# su-exec 用于入口脚本把进程降权到非 root（比 gosu 小得多）
RUN apk --no-cache add ca-certificates tzdata su-exec

# 镜像自带时区，docker run 直接启动时也不会退回 UTC（时间戳偏 8 小时）
ENV TZ=Asia/Shanghai

WORKDIR /app

# 从构建阶段复制二进制
COPY --from=builder /app/wechat-profile-bot .

# 入口脚本：对齐 /config 属主后降权运行。
# 用 RUN chmod 而不是 COPY --chmod，避免依赖 BuildKit。
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh

# 降权使用的 UID/GID，默认 1000；请与宿主机上运行 docker 的用户对齐
ENV PUID=1000
ENV PGID=1000

# 配置和数据目录（挂载卷持久化）
# 程序通过检测 /config 目录是否存在来决定数据位置：存在则使用
# /config/config.json、/config/wechat-profile-bot.db、/config/ilink_credentials.json，
# 不读取任何环境变量，因此这里无需（也不应）设置 CONFIG_PATH。
VOLUME ["/config"]

EXPOSE 17965

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
