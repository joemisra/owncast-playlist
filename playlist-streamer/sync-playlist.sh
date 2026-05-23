#!/bin/bash
# sync-playlist.sh — Scan videos folder, update the playlist via API
# Usage: ./sync-playlist.sh
#
# SFTP files into /opt/owncast/playlist-streamer/videos/ then run this.
# The stream switches to the new playlist immediately.

set -e

VIDEOS_DIR="/opt/owncast/playlist-streamer/videos"
API="http://localhost:9090/api"

# Find video files, sorted by name
FILES=()
while IFS= read -r -d '' f; do
    FILES+=("$f")
done < <(find "$VIDEOS_DIR" -maxdepth 1 -type f \( -iname "*.mp4" -o -iname "*.mkv" -o -iname "*.mov" -o -iname "*.avi" -o -iname "*.webm" \) -print0 | sort -z)

if [ ${#FILES[@]} -eq 0 ]; then
    echo "No video files found in $VIDEOS_DIR"
    exit 1
fi

echo "Found ${#FILES[@]} video(s):"
for f in "${FILES[@]}"; do
    echo "  $(basename "$f")"
done

# Build JSON
JSON='{"videos":['
FIRST=true
for f in "${FILES[@]}"; do
    if [ "$FIRST" = true ]; then
        FIRST=false
    else
        JSON+=','
    fi
    JSON+='{"url":"'"$f"'","provider":"local"}'
done
JSON+=']}'

# Replace playlist
curl -s -X POST "$API/playlist" \
    -H 'Content-Type: application/json' \
    -d "$JSON" > /dev/null

# Save to disk
curl -s -X POST "$API/playlist/save" > /dev/null

echo ""
echo "Playlist updated. Status:"
curl -s "$API/status" | python3 -m json.tool 2>/dev/null || curl -s "$API/status"
