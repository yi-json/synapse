#!/bin/bash

# setup_rootfs.sh
# Creates a minimal root filesystem based on BusyBox for the Synapse/Carapace runtime.

set -e # Exit immediately if a command exits with a non-zero status.

ROOTFS_DIR="my-rootfs"

echo "🐳 [Synapse] Setting up Root Filesystem..."

# 1. Check for Docker
if ! command -v docker &> /dev/null; then
    echo "❌ Error: Docker is not installed."
    echo "   Please install it: sudo apt update && sudo apt install -y docker.io"
    exit 1
fi

# 2. Clean up previous build
if [ -d "$ROOTFS_DIR" ]; then
    echo "🧹 Cleaning up existing $ROOTFS_DIR..."
    sudo rm -rf "$ROOTFS_DIR"
fi

mkdir -p "$ROOTFS_DIR"

# 3. Create the Bundle using BusyBox
# We use BusyBox because it is static (no dependencies like /lib/ld-linux.so)
echo "📦 Pulling BusyBox image..."
sudo docker pull busybox:1.36 > /dev/null

echo "🔨 Exporting filesystem..."
CONTAINER_ID=$(sudo docker create busybox:1.36)

# Export tar and extract into directory
sudo docker export "$CONTAINER_ID" | tar -x -C "$ROOTFS_DIR"

# 4. Cleanup container
sudo docker rm "$CONTAINER_ID" > /dev/null

# 5. Verify
if [ -f "$ROOTFS_DIR/bin/sh" ]; then
    echo "✅ Success! RootFS created at: $(pwd)/$ROOTFS_DIR"
    echo "   Contains: $(ls $ROOTFS_DIR)"
else
    echo "❌ Error: Filesystem creation failed."
    exit 1
fi