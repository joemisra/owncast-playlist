#!/bin/bash
# Download latest yt-dlp to the playlist-streamer directory.
# Run this, then set ytdlp_path in config.local.yaml to ./yt-dlp (or full path).

set -e
cd "$(dirname "$0")"
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ASSET="yt-dlp_linux" ;;
  aarch64) ASSET="yt-dlp_linux_aarch64" ;;
  armv7l)  echo "armv7l: use yt-dlp_linux_armv7l.zip and extract manually"; exit 1 ;;
  *) echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

TAG=$(curl -sL https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest | grep '"tag_name":' | sed -E 's/.*"tag_name": "([^"]+)".*/\1/')
URL="https://github.com/yt-dlp/yt-dlp/releases/download/${TAG}/${ASSET}"

echo "Downloading yt-dlp ${TAG} ($ASSET)..."
curl -sL "$URL" -o yt-dlp
chmod +x yt-dlp
./yt-dlp --version
echo "Done. Set in config.local.yaml:"
echo "  ytdlp_path: \"$(pwd)/yt-dlp\""
