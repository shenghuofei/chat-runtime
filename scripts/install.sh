#!/bin/bash
#
# 快速部署脚本：将 chat-runtime 安装为 systemd 服务
#
# 用法: sudo ./scripts/install.sh
#
# 前提: 已编译好 Linux 二进制（dist/chat-runtime-linux-amd64）

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

BINARY="dist/chat-runtime-linux-amd64"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/chat-runtime"
DATA_DIR="/var/lib/chat-runtime"
SERVICE_USER="chat-runtime"

log_info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# 检查 root 权限
if [ "$(id -u)" -ne 0 ]; then
    log_error "请使用 sudo 运行此脚本"
    exit 1
fi

# 检查二进制
if [ ! -f "$BINARY" ]; then
    log_error "未找到二进制文件: ${BINARY}"
    echo "请先编译: GOOS=linux GOARCH=amd64 make build-all"
    exit 1
fi

# 1. 创建运行用户
if ! id "$SERVICE_USER" &>/dev/null; then
    log_info "创建服务用户: ${SERVICE_USER}"
    useradd -r -s /sbin/nologin "$SERVICE_USER"
fi

# 2. 安装二进制
log_info "安装二进制到 ${INSTALL_DIR}/chat-runtime"
cp "$BINARY" "${INSTALL_DIR}/chat-runtime"
chmod 755 "${INSTALL_DIR}/chat-runtime"

# 3. 创建配置目录
log_info "创建配置目录: ${CONFIG_DIR}"
mkdir -p "$CONFIG_DIR"
if [ ! -f "${CONFIG_DIR}/config.yml" ]; then
    cp examples/minimal.yml "${CONFIG_DIR}/config.yml"
    log_info "已复制示例配置到 ${CONFIG_DIR}/config.yml（请修改 API Key）"
else
    log_info "配置文件已存在，跳过"
fi

# 4. 创建数据目录
log_info "创建数据目录: ${DATA_DIR}"
mkdir -p "$DATA_DIR"
chown "$SERVICE_USER":"$SERVICE_USER" "$DATA_DIR"

# 5. 安装 systemd 服务
log_info "安装 systemd 服务"
cp scripts/chat-runtime.service /etc/systemd/system/
systemctl daemon-reload

# 6. 提示
echo ""
log_info "安装完成！"
echo ""
echo "后续步骤:"
echo "  1. 编辑配置文件: vim ${CONFIG_DIR}/config.yml"
echo "  2. 设置 API Key（推荐使用 env 文件）:"
echo "     echo 'ARK_API_KEY=your-key' > ${CONFIG_DIR}/env"
echo "     echo 'ARK_MODEL=ep-xxxxx' >> ${CONFIG_DIR}/env"
echo "     # 然后取消 chat-runtime.service 中 EnvironmentFile 的注释"
echo "  3. 启动服务: systemctl start chat-runtime"
echo "  4. 设置开机自启: systemctl enable chat-runtime"
echo "  5. 查看日志: journalctl -u chat-runtime -f"
echo "  6. 查看状态: systemctl status chat-runtime"
