#!/usr/bin/env bash
# Linux/Docker 离线冒烟测试；仅使用测试凭据，容器 --network none 不访问真实上游。
set -euo pipefail

image="${1:?用法: bash scripts/test-paas-image.sh IMAGE}"
name="vertex-paas-smoke-$$-${RANDOM}"
bad_name="${name}-invalid"
tmp="$(mktemp -d)"
cleanup() {
    docker rm -f "$name" "$bad_name" >/dev/null 2>&1 || true
    rm -rf "$tmp"
}
trap cleanup EXIT

fail() {
    echo "[Smoke] $*" >&2
    exit 1
}

hash="$(docker run --rm --network none "$image" rules-hash)"
[[ "$hash" =~ ^[0-9a-f]{16}$ ]] || fail "rules-hash 输出格式错误"
docker run --rm --network none "$image" rules > "$tmp/rules.txt"
[[ -s "$tmp/rules.txt" ]] || fail "rules 输出为空"
docker run --rm --network none "$image" version > /dev/null

# 自动提供哈希仅用于无外网测试容器；真实部署必须由部署者先阅读规则并确认。
password='ci-password-with-"quote"-and-\slash'
key='sk-0123456789abcdef0123456789abcdef'
credentials=(-e "VPROXY_ADMIN_PASSWORD=$password" -e "VPROXY_API_KEY=$key")
valid=(-e "VPROXY_ACCEPT_RULES_HASH=$hash" "${credentials[@]}")

expect_failure() {
    local status=0
    timeout 25s docker run --name "$bad_name" --network none "$@" "$image" \
        > "$tmp/invalid.log" 2>&1 || status=$?
    docker rm -f "$bad_name" >/dev/null 2>&1 || true
    # 超时、Docker 引擎错误不算应用按预期拒绝配置。
    [[ "$status" -eq 1 ]] || fail "无效配置应以 1 退出，实际为 $status"
}

expect_failure "${credentials[@]}"
expect_failure -e VPROXY_ACCEPT_RULES_HASH=wrong "${credentials[@]}"
expect_failure -e "VPROXY_ACCEPT_RULES_HASH=$hash" -e "VPROXY_API_KEY=$key"
expect_failure -e "VPROXY_ACCEPT_RULES_HASH=$hash" -e "VPROXY_ADMIN_PASSWORD=$password"
expect_failure "${valid[@]}" -e PORT=0
expect_failure "${valid[@]}" -e PORT=65536
expect_failure "${valid[@]}" -e 'VPROXY_CONFIG_JSON={"max_concurrent":"bad"}'
expect_failure "${valid[@]}" -e 'VPROXY_CONFIG_JSON={"unknown_field":1}'
expect_failure "${valid[@]}" -e VPROXY_BOOTSTRAP_NODES=not-a-node
expect_failure "${valid[@]}" -e VPROXY_BOOTSTRAP_NODES=

# 非默认端口、特殊字符密码、JSON 覆盖和节点初始化一起验证。
docker run -d --name "$name" --network none "${valid[@]}" \
    -e PORT=18080 \
    -e 'VPROXY_CONFIG_JSON={"max_concurrent":2,"port_api":9999,"admin_password":"ignored"}' \
    -e 'VPROXY_BOOTSTRAP_NODES=socks5://ci:node-private-value@127.0.0.1:1080' \
    "$image" > /dev/null

wait_healthy() {
    local ready=false
    for _ in {1..60}; do
        if docker exec "$name" wget -q -T 2 -O - http://127.0.0.1:18080/health \
            > "$tmp/health.json" 2>/dev/null; then
            ready=true
            break
        fi
        [[ "$(docker inspect -f '{{.State.Running}}' "$name")" == true ]] || break
        sleep 1
    done
    if [[ "$ready" != true ]]; then
        # 这里只有公开的 CI 测试凭据，不读取真实部署的容器日志。
        docker logs "$name" >&2
        fail "服务未在 PORT 指定端口就绪"
    fi
    python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["status"] == "healthy"; assert d["api_keys_loaded"] == 1' \
        < "$tmp/health.json"
}

wait_healthy
docker exec "$name" sh -c 'test "$(id -u)" = 10001 && test -f /app/config/data.db'
docker exec "$name" sh -c 'test "$(stat -c %a /app/config/api_keys.txt)" = 600'

docker exec "$name" cat /app/config/config.json > "$tmp/config.json"
python3 - "$tmp/config.json" "$password" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    cfg = json.load(f)
assert cfg["port_api"] == 18080
assert cfg["admin_password"] == sys.argv[2]
assert cfg["max_concurrent"] == 2
assert cfg["parallel_pool_size"] == 2
assert cfg["recaptcha_token_prefetch"] is False
PY

status=0
docker exec "$name" wget -S -T 3 -O /dev/null http://127.0.0.1:18080/v1/models \
    > "$tmp/unauthorized.log" 2>&1 || status=$?
[[ "$status" -ne 0 ]] || fail "业务接口意外免鉴权"
grep -q 'HTTP/1.1 401' "$tmp/unauthorized.log" || fail "缺失密钥未返回 401"

docker exec "$name" wget -q -T 3 --header="Authorization: Bearer $key" \
    -O - http://127.0.0.1:18080/v1/models > "$tmp/models.json"
python3 -c 'import json,sys; d=json.load(sys.stdin); assert isinstance(d["data"], list)' < "$tmp/models.json"

docker logs "$name" > "$tmp/runtime.log" 2>&1
grep -q '\[Bootstrap\].*出口节点初始化完成' "$tmp/runtime.log" || fail "节点初始化未执行"
for secret in "$password" "$key" 'node-private-value'; do
    if grep -Fq "$secret" "$tmp/runtime.log"; then
        fail "启动日志泄漏测试凭据"
    fi
done

# 仅改测试容器的文件；下一次启动必须恢复环境变量指定的密钥。
docker exec "$name" sh -c 'printf "stale:sk-stale-key:smoke test\n" > /app/config/api_keys.txt'
# 验证 SIGTERM 直接到达 Go，且同一容器再次启动会重新生成密钥文件。
docker stop -t 35 "$name" > /dev/null
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$name")" == 0 ]] || fail "首次关闭不是正常退出"
docker start "$name" > /dev/null
wait_healthy
docker exec "$name" wget -q -T 3 --header="Authorization: Bearer $key" \
    -O /dev/null http://127.0.0.1:18080/v1/models
docker stop -t 35 "$name" > /dev/null
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$name")" == 0 ]] || fail "重启后关闭不是正常退出"
echo "[Smoke] PaaS 镜像离线冒烟测试通过（未验证 Google 上游）"
