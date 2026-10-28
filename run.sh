#!/usr/bin/env bash
# Ubuntu / Debian 一键跑起来：查依赖 → 编译 → 服务器和判机一起拉起，Ctrl-C 一起收干净。
#
#   ./run.sh                                  # 只给自己访问：http://127.0.0.1:8080
#   FASTOJ_ADDR=0.0.0.0:8080 ./run.sh         # 同网段都能访问（判机仍走 127.0.0.1）
#   JUDGE=0 ./run.sh                          # 只起服务器，判机在另一台机器上
#   FASTOJ_ADMIN_TOKEN=xxx FASTOJ_JUDGE_TOKEN=yyy ./run.sh   # 用固定令牌
#
# 其余参数原样传给服务器，比如 ./run.sh -no-register -v
set -eu
cd "$(dirname "$0")"

GO="${GO:-go}"
ADDR="${FASTOJ_ADDR:-127.0.0.1:8080}"
DATA="${FASTOJ_DATA:-data}"
JUDGE="${JUDGE:-1}"
JUDGE_NAME="${JUDGE_NAME:-$(hostname)}"

# apt 的 golang-go 装在 /usr/bin，官方压缩包习惯解压到 /usr/local/go：两个都认。
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
  PATH="/usr/local/go/bin:$PATH"
fi
if ! command -v "$GO" >/dev/null 2>&1; then
  echo "没有 go 编译器：sudo apt install golang-go，或把官方包解压到 /usr/local/go" >&2
  exit 1
fi

newtoken() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16
  else
    head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# 两个令牌空着服务器会拒绝启动。这里随机生成而不是写死一对固定值：脚本里的默认令牌
# 迟早跟着仓库传到网上，而随机值只在本次运行里有效，重启就换。
GENERATED=0
if [ -z "${FASTOJ_ADMIN_TOKEN:-}" ]; then FASTOJ_ADMIN_TOKEN="$(newtoken)"; GENERATED=1; fi
if [ -z "${FASTOJ_JUDGE_TOKEN:-}" ]; then FASTOJ_JUDGE_TOKEN="$(newtoken)"; GENERATED=1; fi
export FASTOJ_ADMIN_TOKEN FASTOJ_JUDGE_TOKEN

# 监听地址不等于能连上的地址：绑 0.0.0.0 或 :: 时，判机要用回环去连它。
HOST="${ADDR%:*}"
PORT="${ADDR##*:}"
case "$HOST" in
  ""|0.0.0.0|::|\[::\]) HOST=127.0.0.1 ;;
esac
SERVER_URL="http://$HOST:$PORT"

if command -v curl >/dev/null 2>&1 && curl -s -o /dev/null --max-time 2 "$SERVER_URL/healthz"; then
  echo "$SERVER_URL 上已经有一个 fast-oj 在跑：先停掉它，或者换个 FASTOJ_ADDR。" >&2
  exit 1
fi

if [ "$JUDGE" = 1 ] && ! command -v g++ >/dev/null 2>&1; then
  echo "没找到 g++，判机起不来（C/C++ 提交会一直排队）。先只起服务器：sudo apt install g++ 之后重跑。" >&2
  JUDGE=0
fi

echo "==> 编译（CGO 关掉，SQLite 用的是纯 Go 驱动）"
CGO_ENABLED=0 "$GO" build -o dist/ ./cmd/...

if [ "$JUDGE" = 1 ]; then
  echo "==> 起判机 $JUDGE_NAME → $SERVER_URL"
  # 令牌走上面 export 的环境变量，不写在命令行上：Linux 上 /proc/<pid>/cmdline 是所有
  # 本地账号可读的，而判机上跑的正是别人提交的代码。
  ./dist/fast-judge -server "$SERVER_URL" -name "$JUDGE_NAME" -v &
  JUDGE_PID=$!
  # 服务器退出（含 Ctrl-C）时别留一个孤儿判机在那儿空轮询。
  trap 'kill "$JUDGE_PID" 2>/dev/null || true' EXIT
fi

echo
echo "浏览器打开 $SERVER_URL"
if [ "$GENERATED" = 1 ]; then
  echo "  管理令牌（本次随机生成，用一次即作废）：$FASTOJ_ADMIN_TOKEN"
  echo "  判机令牌（本次随机生成）：$FASTOJ_JUDGE_TOKEN"
  echo "  判机令牌想固定下来就 export FASTOJ_JUDGE_TOKEN 再跑。管理令牌不建议固定：它"
  echo "  「一次即废」这一点就是防泄露的那道闸门；用完下一枚会打印在服务器日志的"
  echo "  one-time token rotated 那一行里。"
fi
echo "  全新库的第一件事：打开 $SERVER_URL/admin/claim，用管理令牌给 admin 账号设网页密码（这一枚设完就作废）。"
echo

./dist/fast-oj -addr "$ADDR" -data "$DATA" "$@"
