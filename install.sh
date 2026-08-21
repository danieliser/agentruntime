#!/bin/bash
set -euo pipefail

# agentruntime install.sh
# Installs the agentd binary and creates a launchd/systemd service

usage() {
    cat << 'EOF'
agentruntime installer

Usage:
  install.sh [OPTIONS]

Options:
  --system              Install to /usr/local/bin and create system-level service
  --port PORT           Configure service port (default: 8090)
  --no-credential-sync  Disable credential sync from Keychain
  --docker-default      Set docker as default runtime (instead of local)
  --help                Show this message

Example:
  install.sh --port 8090 --credential-sync
  install.sh --system --docker-default

EOF
    exit 0
}

# Defaults
SYSTEM_INSTALL=false
PORT=8090
CREDENTIAL_SYNC=true
DOCKER_DEFAULT=false
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Parse flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        --system)
            SYSTEM_INSTALL=true
            shift
            ;;
        --port)
            PORT="$2"
            shift 2
            ;;
        --no-credential-sync)
            CREDENTIAL_SYNC=false
            shift
            ;;
        --docker-default)
            DOCKER_DEFAULT=true
            shift
            ;;
        --help)
            usage
            ;;
        *)
            echo "Unknown option: $1"
            usage
            ;;
    esac
done

# Validate port
if ! [[ "$PORT" =~ ^[0-9]+$ ]] || [ "$PORT" -lt 1024 ] || [ "$PORT" -gt 65535 ]; then
    echo "error: invalid port $PORT (must be 1024-65535)"
    exit 1
fi

# Detect OS
OS=""
if [[ "$OSTYPE" == "darwin"* ]]; then
    OS="macos"
elif [[ "$OSTYPE" == "linux"* ]]; then
    OS="linux"
else
    echo "error: unsupported OS: $OSTYPE"
    exit 1
fi

# Determine install paths
if [ "$SYSTEM_INSTALL" = true ]; then
    BIN_DIR="/usr/local/bin"
    if [[ "$OS" == "macos" ]]; then
        SERVICE_PLIST="/Library/LaunchDaemons/com.agentruntime.agentd.plist"
    else
        SERVICE_UNIT="/etc/systemd/system/agentruntime.service"
    fi
    echo "system install: binaries to $BIN_DIR, service files to system location"
else
    BIN_DIR="$HOME/.local/bin"
    if [[ "$OS" == "macos" ]]; then
        SERVICE_PLIST="$HOME/Library/LaunchAgents/com.agentruntime.agentd.plist"
    else
        SERVICE_UNIT="$HOME/.config/systemd/user/agentruntime.service"
    fi
    echo "user install: binaries to $BIN_DIR, service files to user location"
fi

# Check for Go
if ! command -v go &> /dev/null; then
    echo "error: Go is required but not installed"
    exit 1
fi

GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
echo "using Go $GO_VERSION"

AGENTD_VERSION="${AGENTD_VERSION:-$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$SCRIPT_DIR/packages/agentd-py/pyproject.toml" | head -1)}"
AGENTD_COMMIT="${AGENTD_COMMIT:-$(git -C "$SCRIPT_DIR" rev-parse HEAD)}"
if [[ ! "$AGENTD_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || [[ ! "$AGENTD_COMMIT" =~ ^[a-f0-9]{40,64}$ ]]; then
    echo "error: release identity must be an exact semantic version and commit"
    exit 1
fi

# The installed daemon advertises Docker to qualified callers. Refuse a build
# whose exact release-stamped agent or proxy image is absent or mismatched.
if ! command -v docker &> /dev/null; then
    echo "error: Docker is required for release-stamped AgentD runtime images"
    exit 1
fi
for image in "agentruntime-agent:${AGENTD_VERSION}" "agentruntime-agent-codex:${AGENTD_VERSION}" "agentruntime-agent-claude:${AGENTD_VERSION}" "agentruntime-proxy:${AGENTD_VERSION}"; do
    if ! docker image inspect "$image" &> /dev/null; then
        echo "error: required runtime image $image is absent; run ./docker/build.sh all first"
        exit 1
    fi
    image_version="$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image")"
    image_commit="$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$image")"
    if [ "$image_version" != "$AGENTD_VERSION" ] || [ "$image_commit" != "$AGENTD_COMMIT" ]; then
        echo "error: runtime image $image stamp does not match ${AGENTD_VERSION}@${AGENTD_COMMIT}"
        exit 1
    fi
done

# Create bin directory if it doesn't exist
mkdir -p "$BIN_DIR"

# Build agentd
echo "building agentd..."
cd "$SCRIPT_DIR"
if ! go build -trimpath \
    -ldflags="-X github.com/danieliser/agentruntime/pkg/buildinfo.Version=${AGENTD_VERSION} -X github.com/danieliser/agentruntime/pkg/buildinfo.Commit=${AGENTD_COMMIT}" \
    -o "$BIN_DIR/agentd" ./cmd/agentd; then
    echo "error: failed to build agentd"
    exit 1
fi
if ! "$BIN_DIR/agentd" --require-build "${AGENTD_VERSION}@${AGENTD_COMMIT}"; then
    echo "error: installed AgentD binary identity verification failed"
    exit 1
fi
echo "✓ built agentd -> $BIN_DIR/agentd"

# Create data directory
DATA_DIR="$HOME/.agentd"
mkdir -p "$DATA_DIR"
echo "✓ created data directory: $DATA_DIR"

# Determine runtime
RUNTIME="local"
if [ "$DOCKER_DEFAULT" = true ]; then
    RUNTIME="docker"
fi

# Create service
if [[ "$OS" == "macos" ]]; then
    mkdir -p "$(dirname "$SERVICE_PLIST")"

    # Build command line arguments
    AGENTD_ARGS="$BIN_DIR/agentd --port $PORT --runtime $RUNTIME --data-dir $DATA_DIR"
    if [ "$CREDENTIAL_SYNC" = true ]; then
        AGENTD_ARGS="$AGENTD_ARGS --credential-sync"
    fi

    # Create launchd plist
    cat > "$SERVICE_PLIST" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.agentruntime.agentd</string>
    <key>Program</key>
    <string>$BIN_DIR/agentd</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN_DIR/agentd</string>
        <string>--port</string>
        <string>$PORT</string>
        <string>--runtime</string>
        <string>$RUNTIME</string>
        <string>--data-dir</string>
        <string>$DATA_DIR</string>
EOF

    if [ "$CREDENTIAL_SYNC" = true ]; then
        cat >> "$SERVICE_PLIST" << 'EOF'
        <string>--credential-sync</string>
EOF
    fi

    cat >> "$SERVICE_PLIST" << 'EOF'
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>~/Library/Logs/agentruntime/agentd.log</string>
    <key>StandardErrorPath</key>
    <string>~/Library/Logs/agentruntime/agentd.log</string>
    <key>WorkingDirectory</key>
    <string>~</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>~/.local/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
</dict>
</plist>
EOF

    # Expand ~ in plist — launchd doesn't expand tilde in any field.
    sed -i '' "s|~|$HOME|g" "$SERVICE_PLIST"

    echo "✓ created launchd plist: $SERVICE_PLIST"

    # launchd opens these paths itself. Pre-create them privately so neither
    # directory nor file ever inherits a broader default mode.
    install -d -m 700 "$HOME/Library/Logs/agentruntime"
    touch "$HOME/Library/Logs/agentruntime/agentd.log"
    chmod 600 "$HOME/Library/Logs/agentruntime/agentd.log"

    # Load the service
    if [ "$SYSTEM_INSTALL" = true ]; then
        sudo launchctl load "$SERVICE_PLIST"
        echo "✓ loaded system service with launchctl"
    else
        launchctl load "$SERVICE_PLIST"
        echo "✓ loaded user service with launchctl"
    fi

else
    # Linux systemd
    mkdir -p "$(dirname "$SERVICE_UNIT")"

    # Build systemd unit
    cat > "$SERVICE_UNIT" << EOF
[Unit]
Description=agentruntime daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN_DIR/agentd --port $PORT --runtime $RUNTIME --data-dir $DATA_DIR${CREDENTIAL_SYNC:+ --credential-sync}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
Environment=PATH=/usr/local/bin:/usr/bin:/bin
WorkingDirectory=$HOME

[Install]
WantedBy=default.target
EOF

    echo "✓ created systemd unit: $SERVICE_UNIT"

    # Enable and start the service
    if [ "$SYSTEM_INSTALL" = true ]; then
        sudo systemctl daemon-reload
        sudo systemctl enable agentruntime.service
        sudo systemctl start agentruntime.service
        echo "✓ enabled and started system service"
    else
        systemctl --user daemon-reload
        systemctl --user enable agentruntime.service
        systemctl --user start agentruntime.service
        echo "✓ enabled and started user service"
    fi
fi

echo ""
echo "Installation complete!"
echo ""
echo "agentd is now running on http://localhost:$PORT"
echo "  Runtime: $RUNTIME"
echo "  Data directory: $DATA_DIR"
echo "  Binaries: $BIN_DIR"
echo ""

if [[ "$OS" == "macos" ]]; then
    echo "To check status:"
    if [ "$SYSTEM_INSTALL" = true ]; then
        echo "  sudo launchctl list com.agentruntime.agentd"
        echo "  tail -f /Library/Logs/agentruntime/agentd.log"
    else
        echo "  launchctl list com.agentruntime.agentd"
        echo "  tail -f ~/Library/Logs/agentruntime/agentd.log"
    fi
else
    if [ "$SYSTEM_INSTALL" = true ]; then
        echo "To check status:"
        echo "  sudo systemctl status agentruntime.service"
        echo "  sudo journalctl -u agentruntime.service -f"
    else
        echo "To check status:"
        echo "  systemctl --user status agentruntime.service"
        echo "  journalctl --user -u agentruntime.service -f"
    fi
fi
