#!/bin/bash
set -e

# agentruntime uninstall.sh
# Removes agentd and agentruntime-sidecar binaries and service files

usage() {
    cat << 'EOF'
agentruntime uninstaller

Usage:
  uninstall.sh [OPTIONS]

Options:
  --system  Uninstall system-level service (requires sudo)
  --purge   Remove data directory without prompting
  --help    Show this message

EOF
    exit 0
}

# Defaults
SYSTEM_UNINSTALL=false
PURGE=false

# Parse flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        --system)
            SYSTEM_UNINSTALL=true
            shift
            ;;
        --purge)
            PURGE=true
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
if [ "$SYSTEM_UNINSTALL" = true ]; then
    BIN_DIR="/usr/local/bin"
    if [[ "$OS" == "macos" ]]; then
        SERVICE_PLIST="/Library/LaunchDaemons/com.agentruntime.agentd.plist"
    else
        SERVICE_UNIT="/etc/systemd/system/agentruntime.service"
    fi
else
    BIN_DIR="$HOME/.local/bin"
    if [[ "$OS" == "macos" ]]; then
        SERVICE_PLIST="$HOME/Library/LaunchAgents/com.agentruntime.agentd.plist"
    else
        SERVICE_UNIT="$HOME/.config/systemd/user/agentruntime.service"
    fi
fi

DATA_DIR="$HOME/.local/share/agentruntime"

echo "agentruntime uninstaller"
echo ""

# Stop and unload service
if [[ "$OS" == "macos" ]]; then
    if [ -f "$SERVICE_PLIST" ]; then
        echo "Stopping service..."
        if [ "$SYSTEM_UNINSTALL" = true ]; then
            sudo launchctl unload "$SERVICE_PLIST" 2>/dev/null || true
            echo "✓ unloaded system service"
        else
            launchctl unload "$SERVICE_PLIST" 2>/dev/null || true
            echo "✓ unloaded user service"
        fi
    fi
else
    # Linux systemd
    if [ "$SYSTEM_UNINSTALL" = true ]; then
        if sudo systemctl is-active --quiet agentruntime.service 2>/dev/null; then
            echo "Stopping service..."
            sudo systemctl stop agentruntime.service
            echo "✓ stopped system service"
        fi
        if sudo systemctl is-enabled --quiet agentruntime.service 2>/dev/null; then
            sudo systemctl disable agentruntime.service
            echo "✓ disabled system service"
        fi
    else
        if systemctl --user is-active --quiet agentruntime.service 2>/dev/null; then
            echo "Stopping service..."
            systemctl --user stop agentruntime.service
            echo "✓ stopped user service"
        fi
        if systemctl --user is-enabled --quiet agentruntime.service 2>/dev/null; then
            systemctl --user disable agentruntime.service
            echo "✓ disabled user service"
        fi
    fi
fi

# Remove service files
echo ""
echo "Removing service files..."
if [[ "$OS" == "macos" ]]; then
    if [ -f "$SERVICE_PLIST" ]; then
        if [ "$SYSTEM_UNINSTALL" = true ]; then
            sudo rm -f "$SERVICE_PLIST"
        else
            rm -f "$SERVICE_PLIST"
        fi
        echo "✓ removed $SERVICE_PLIST"
    fi
else
    if [ -f "$SERVICE_UNIT" ]; then
        if [ "$SYSTEM_UNINSTALL" = true ]; then
            sudo rm -f "$SERVICE_UNIT"
            sudo systemctl daemon-reload
        else
            rm -f "$SERVICE_UNIT"
            systemctl --user daemon-reload
        fi
        echo "✓ removed $SERVICE_UNIT"
    fi
fi

# Remove binaries
echo ""
echo "Removing binaries..."
if [ -f "$BIN_DIR/agentd" ]; then
    if [ "$SYSTEM_UNINSTALL" = true ]; then
        sudo rm -f "$BIN_DIR/agentd"
    else
        rm -f "$BIN_DIR/agentd"
    fi
    echo "✓ removed $BIN_DIR/agentd"
fi

if [ -f "$BIN_DIR/agentruntime-sidecar" ]; then
    if [ "$SYSTEM_UNINSTALL" = true ]; then
        sudo rm -f "$BIN_DIR/agentruntime-sidecar"
    else
        rm -f "$BIN_DIR/agentruntime-sidecar"
    fi
    echo "✓ removed $BIN_DIR/agentruntime-sidecar"
fi

# Handle data directory
echo ""
if [ -d "$DATA_DIR" ]; then
    if [ "$PURGE" = true ]; then
        rm -rf "$DATA_DIR"
        echo "✓ removed data directory: $DATA_DIR"
    else
        echo "Data directory exists: $DATA_DIR"
        read -p "Remove data directory? [y/N] " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            rm -rf "$DATA_DIR"
            echo "✓ removed data directory"
        else
            echo "⊘ kept data directory (use --purge to remove without prompting)"
        fi
    fi
fi

# Remove logs directory
if [[ "$OS" == "macos" ]]; then
    LOGS_DIR="$HOME/Library/Logs/agentruntime"
    if [ -d "$LOGS_DIR" ]; then
        if [ "$PURGE" = true ]; then
            rm -rf "$LOGS_DIR"
            echo "✓ removed logs directory: $LOGS_DIR"
        else
            read -p "Remove logs directory? [y/N] " -n 1 -r
            echo
            if [[ $REPLY =~ ^[Yy]$ ]]; then
                rm -rf "$LOGS_DIR"
                echo "✓ removed logs directory"
            else
                echo "⊘ kept logs directory"
            fi
        fi
    fi
fi

echo ""
echo "Uninstall complete!"
