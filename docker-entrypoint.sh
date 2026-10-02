#!/bin/sh
# 容器入口：把 /config 的属主对齐到 PUID/PGID，然后降权运行，不再以 root 跑业务进程。
#
# 为什么需要 entrypoint，而不是在 Dockerfile 里直接写 USER 1000：
#   docker-compose 用的是 bind mount（./config:/config），这个目录在宿主机上的属主
#   取决于它是谁创建的（通常是 root）。如果镜像里直接 USER 1000，容器进程就写不进
#   数据库、日志和备份文件，服务根本起不来 —— 那就是「安全加固把功能搞坏了」。
#   所以这里先在 root 权限下 chown 对齐，再 exec 降权，两步都做完才既安全又可用。
#
# 可调环境变量：
#   PUID / PGID  容器进程使用的用户/组 ID，默认 1000。
#                请与宿主机上执行 docker 的用户保持一致（id -u / id -g 可查），
#                这样 ./config 里的文件在宿主机上也能正常读写。
#   PUID=0       逃生通道：显式要求以 root 运行，行为与旧版本一致。
set -eu

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

mkdir -p /config

# 显式要求 root 时不降权，保证老部署方式随时可以退回
if [ "$PUID" = "0" ]; then
	echo "[entrypoint] PUID=0，按要求以 root 运行" >&2
	exec /app/wechat-profile-bot "$@"
fi

# 镜像被改过、su-exec 不在时降级为 root，宁可不降权也不能让服务起不来
if ! command -v su-exec >/dev/null 2>&1; then
	echo "[entrypoint] 警告：镜像中未找到 su-exec，本次以 root 运行" >&2
	exec /app/wechat-profile-bot "$@"
fi

# 旧版本容器留下的文件属主是 root，一并改掉，否则降权后读不了也写不了
chown -R "$PUID:$PGID" /config
# /config 内是 config.json（模型 API Key）、微信登录凭据、2FA 密钥，收紧到仅属主可访问
chmod 700 /config

exec su-exec "$PUID:$PGID" /app/wechat-profile-bot "$@"
