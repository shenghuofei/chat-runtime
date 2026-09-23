#!/bin/bash
#
# chat-runtime 服务启停脚本
#
# 用法:
#   ./scripts/service.sh start   启动服务（后台运行）
#   ./scripts/service.sh stop    停止服务（优雅关闭）
#   ./scripts/service.sh restart 重启服务
#   ./scripts/service.sh status  查看运行状态
#   ./scripts/service.sh logs    查看实时日志
#
# 配置（可通过环境变量覆盖）:
#   CHAT_RUNTIME_BIN      二进制路径，默认 ./chat-runtime
#   CHAT_RUNTIME_CONFIG   配置文件路径，默认 ./config.yml
#   CHAT_RUNTIME_PORT     监听端口，默认 8080
#   CHAT_RUNTIME_HOST     监听地址，默认 0.0.0.0
#   CHAT_RUNTIME_LOGDIR   日志目录，默认 ./logs
#   CHAT_RUNTIME_PIDFILE  PID 文件路径，默认 ./chat-runtime.pid

set -euo pipefail

# ---- 默认配置 ----
APP_NAME="chat-runtime"
BIN="${CHAT_RUNTIME_BIN:-./chat-runtime}"
CONFIG="${CHAT_RUNTIME_CONFIG:-./config.yml}"
PORT="${CHAT_RUNTIME_PORT:-8080}"
HOST="${CHAT_RUNTIME_HOST:-0.0.0.0}"
LOG_DIR="${CHAT_RUNTIME_LOGDIR:-./logs}"
PID_FILE="${CHAT_RUNTIME_PIDFILE:-./chat-runtime.pid}"
LOG_FILE="${LOG_DIR}/${APP_NAME}.log"

# ---- 颜色 ----
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# ---- 获取 PID ----
get_pid() {
    if [ -f "$PID_FILE" ]; then
        local pid
        pid=$(cat "$PID_FILE" 2>/dev/null)
        # 验证进程是否存活
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            echo "$pid"
            return 0
        fi
        # PID 文件存在但进程已死，清理
        rm -f "$PID_FILE"
    fi
    return 1
}

# ---- 启动 ----
do_start() {
    # 检查是否已在运行
    if pid=$(get_pid); then
        log_warn "${APP_NAME} 已在运行 (PID: ${pid})"
        return 0
    fi

    # 检查二进制存在
    if [ ! -x "$BIN" ]; then
        log_error "二进制文件不存在或不可执行: ${BIN}"
        log_error "请先执行: make build 或 GOOS=linux GOARCH=amd64 make build"
        return 1
    fi

    # 检查配置文件存在
    if [ ! -f "$CONFIG" ]; then
        log_error "配置文件不存在: ${CONFIG}"
        log_error "请先创建配置文件: cp examples/minimal.yml config.yml"
        return 1
    fi

    # 创建日志目录
    mkdir -p "$LOG_DIR"

    # 启动服务
    log_info "启动 ${APP_NAME}..."
    log_info "  二进制:   ${BIN}"
    log_info "  配置文件: ${CONFIG}"
    log_info "  监听:     ${HOST}:${PORT}"
    log_info "  日志:     ${LOG_FILE}"

    nohup "$BIN" serve \
        --config "$CONFIG" \
        --host "$HOST" \
        --port "$PORT" \
        >> "$LOG_FILE" 2>&1 &

    local pid=$!
    echo "$pid" > "$PID_FILE"

    # 等待 1 秒确认进程存活
    sleep 1
    if kill -0 "$pid" 2>/dev/null; then
        log_info "${APP_NAME} 启动成功 (PID: ${pid})"
        log_info "Web 界面: http://${HOST}:${PORT}"
    else
        log_error "${APP_NAME} 启动失败，请查看日志: ${LOG_FILE}"
        rm -f "$PID_FILE"
        # 显示最后几行日志帮助排查
        tail -20 "$LOG_FILE" 2>/dev/null
        return 1
    fi
}

# ---- 停止 ----
do_stop() {
    local pid
    if ! pid=$(get_pid); then
        log_warn "${APP_NAME} 未在运行"
        return 0
    fi

    log_info "停止 ${APP_NAME} (PID: ${pid})..."

    # 先发 SIGTERM 让服务优雅关闭（会持久化会话、关闭连接）
    kill -TERM "$pid" 2>/dev/null

    # 等待进程退出（最多 30 秒）
    local count=0
    local max_wait=30
    while kill -0 "$pid" 2>/dev/null; do
        count=$((count + 1))
        if [ $count -ge $max_wait ]; then
            log_warn "等待超时，强制终止 (SIGKILL)..."
            kill -9 "$pid" 2>/dev/null
            sleep 1
            break
        fi
        sleep 1
    done

    rm -f "$PID_FILE"
    log_info "${APP_NAME} 已停止"
}

# ---- 重启 ----
do_restart() {
    do_stop
    sleep 1
    do_start
}

# ---- 状态 ----
do_status() {
    local pid
    if pid=$(get_pid); then
        log_info "${APP_NAME} 正在运行 (PID: ${pid})"
        # 显示进程资源占用
        if command -v ps &>/dev/null; then
            echo ""
            ps -p "$pid" -o pid,ppid,%cpu,%mem,rss,etime,command 2>/dev/null || true
        fi
        # 检查端口监听
        if command -v ss &>/dev/null; then
            echo ""
            echo "端口监听:"
            ss -tlnp 2>/dev/null | grep ":${PORT}" || echo "  (未检测到端口 ${PORT} 的监听)"
        elif command -v netstat &>/dev/null; then
            echo ""
            echo "端口监听:"
            netstat -tlnp 2>/dev/null | grep ":${PORT}" || echo "  (未检测到端口 ${PORT} 的监听)"
        fi
        return 0
    else
        log_warn "${APP_NAME} 未在运行"
        return 1
    fi
}

# ---- 日志 ----
do_logs() {
    if [ ! -f "$LOG_FILE" ]; then
        log_warn "日志文件不存在: ${LOG_FILE}"
        return 1
    fi
    log_info "实时日志（Ctrl+C 退出）: ${LOG_FILE}"
    tail -f "$LOG_FILE"
}

# ---- 帮助 ----
do_help() {
    echo "用法: $0 {start|stop|restart|status|logs}"
    echo ""
    echo "命令:"
    echo "  start    启动服务（后台运行）"
    echo "  stop     停止服务（优雅关闭，最多等 30 秒）"
    echo "  restart  重启服务"
    echo "  status   查看运行状态"
    echo "  logs     查看实时日志"
    echo ""
    echo "环境变量:"
    echo "  CHAT_RUNTIME_BIN      二进制路径      (默认 ./chat-runtime)"
    echo "  CHAT_RUNTIME_CONFIG   配置文件路径    (默认 ./config.yml)"
    echo "  CHAT_RUNTIME_PORT     监听端口        (默认 8080)"
    echo "  CHAT_RUNTIME_HOST     监听地址        (默认 0.0.0.0)"
    echo "  CHAT_RUNTIME_LOGDIR   日志目录        (默认 ./logs)"
    echo "  CHAT_RUNTIME_PIDFILE  PID 文件路径    (默认 ./chat-runtime.pid)"
    echo ""
    echo "示例:"
    echo "  $0 start                                    # 默认配置启动"
    echo "  CHAT_RUNTIME_PORT=9090 $0 start             # 指定端口"
    echo "  CHAT_RUNTIME_CONFIG=/etc/chat.yml $0 start  # 指定配置文件"
}

# ---- 入口 ----
case "${1:-}" in
    start)   do_start ;;
    stop)    do_stop ;;
    restart) do_restart ;;
    status)  do_status ;;
    logs)    do_logs ;;
    help|-h|--help) do_help ;;
    *)
        log_error "未知命令: ${1:-}"
        do_help
        exit 1
        ;;
esac
