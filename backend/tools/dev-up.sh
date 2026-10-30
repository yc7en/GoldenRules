#!/usr/bin/env bash
# 一键拉起本地联调环境：决策服务 + 管理平台，然后打印自测台地址。
#
# 为什么要这个脚本：跑通一次需要 7 个参数（事实目录、规则包、providers、
# reload-token、engine-url、engine-token、log-key），少一个就会在某一步
# 报一个看起来像 bug 的错误（比如没 --engine-url 时发布门禁 fail-closed 拦发布）。
# 把它们固定在一处，比写进 README 让人手抄可靠。
#
# 用法：
#   tools/dev-up.sh            # 前台跑，Ctrl-C 一起退出
#   PORT 冲突时：ENGINE_ADDR=127.0.0.1:19080 PLATFORM_ADDR=127.0.0.1:19081 tools/dev-up.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENGINE_ADDR="${ENGINE_ADDR:-127.0.0.1:18080}"
PLATFORM_ADDR="${PLATFORM_ADDR:-127.0.0.1:18081}"
TOKEN="${RELOAD_TOKEN:-devtoken}"
DATA_DIR="${DATA_DIR:-${TMPDIR:-/tmp}/gr-dev}"
LOG_KEY="${LOG_KEY:-00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff}"

mkdir -p "$DATA_DIR"

echo "== 构建 =="
cargo build --release --manifest-path "$ROOT/Cargo.toml" -p gr-server
(cd "$ROOT/platform" && go build -o "$DATA_DIR/platformd" ./cmd/platformd)

ENGINE="$ROOT/target/release/gr-server"
PLATFORM="$DATA_DIR/platformd"
[ -x "$ENGINE" ] || { echo "没找到 $ENGINE"; exit 1; }

cleanup() {
  # 只杀自己起的两个进程：按 PID，不用 pkill 按名字（会误伤同机上别人跑的同名进程）
  [ -n "${EPID:-}" ] && kill "$EPID" 2>/dev/null || true
  [ -n "${PPID_:-}" ] && kill "$PPID_" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "== 决策服务 $ENGINE_ADDR =="
"$ENGINE" \
  --addr "$ENGINE_ADDR" \
  --facts "$ROOT/fixtures/fact-catalog.json" \
  --package "$ROOT/fixtures/rule-package.json" \
  --providers "$ROOT/fixtures/providers.json" \
  --reload-token "$TOKEN" &
EPID=$!

echo "== 管理平台 $PLATFORM_ADDR =="
"$PLATFORM" \
  --addr "$PLATFORM_ADDR" \
  --data "$DATA_DIR/data" \
  --seed "$ROOT/fixtures" \
  --engine-url "http://$ENGINE_ADDR" \
  --engine-token "$TOKEN" \
  --log-key "$LOG_KEY" \
  --cors-origin '*' &
PPID_=$!

# 等两边都活再打印地址：先打印后起会让用户点开时看到「连不上」
for i in $(seq 1 40); do
  e=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://$ENGINE_ADDR/healthz" || true)
  p=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://$PLATFORM_ADDR/healthz" || true)
  [ "$e" = "200" ] && [ "$p" = "200" ] && break
  sleep 0.25
done

echo
echo "决策服务  http://$ENGINE_ADDR/healthz"
echo "管理平台  http://$PLATFORM_ADDR/healthz"
echo "自测台    file://$(cd "$ROOT" && pwd)/tools/console/index.html"
echo "BRMS 界面  cd ../web && npm run dev（默认 http://localhost:5173）"
echo
echo "自测台里把「平台地址」填成 http://$PLATFORM_ADDR，然后点「一键跑全链路」。"
echo "数据目录 $DATA_DIR（删掉它即可重置）。Ctrl-C 退出。"
echo

wait
