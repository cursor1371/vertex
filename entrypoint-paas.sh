#!/bin/sh
set -eu
umask 077

cd /app

# 查询命令不初始化文件、不启动 HTTP，也不要求填写部署凭据。
if [ "$#" -gt 0 ]; then
    case "$1" in
        rules|rules-hash|version|--version)
            if [ "$#" -ne 1 ]; then
                echo "[Entrypoint] 查询命令不接受额外参数" >&2
                exit 1
            fi
            exec /app/vproxy "$1"
            ;;
        *)
            echo "[Entrypoint] 默认启动无需参数；可用查询命令：rules / rules-hash / version" >&2
            exit 1
            ;;
    esac
fi

# PaaS 镜像固定运行目录；原有 Dockerfile/entrypoint.sh 不受影响。
export VPROXY_CONFIG=/app/config/config.json
export VPROXY_API_KEYS=/app/config/api_keys.txt
export VPROXY_MODELS=/app/config/models.json

# Go 负责严格类型校验与原子写入。失败时 set -e 阻止服务带默认配置启动。
/app/vproxy bootstrap-paas

echo "[Entrypoint] 初始化完成；配置、密钥与模型清单将在每次启动时重新生成"
# Go 成为 PID 1，直接接收平台 SIGTERM，沿用现有优雅关闭逻辑。
exec /app/vproxy
