#!/bin/bash
cd /opt/owncast
ARCH=$(uname -m)
# map common arches
case "$ARCH" in
  x86_64) OC_ARCH="linux-amd64";;
  aarch64) OC_ARCH="linux-arm64";;
  armv7l) OC_ARCH="linux-armv7";;
  *) echo "Unknown arch: $ARCH"; exit 1;;
esac

LATEST=$(curl -s https://api.github.com/repos/owncast/owncast/releases/latest \
  | grep -oE "https.*${OC_ARCH}\.zip" | head -n1)
sudo -u owncast bash -lc "curl -L \"$LATEST\" -o owncast.zip"
sudo -u owncast bash -lc "rm -rf app && mkdir app && cd app && unzip ../owncast.zip && mv owncast ../ && cd .. && rm -rf app owncast.zip"
sudo -u owncast bash -lc "./owncast --help"   # quick sanity check (will print help)
