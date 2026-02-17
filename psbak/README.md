# Playlist Streamer

Streams YouTube videos (and future Kick/Twitch) to Owncast via RTMP. Supports playlists, optional cron scheduling, and looping.

## Prerequisites

- [Go](https://go.dev/dl/) 1.21+
- [yt-dlp](https://github.com/yt-dlp/yt-dlp)
- [ffmpeg](https://ffmpeg.org/)
- Owncast running with a known stream key

## Build

```bash
cd playlist-streamer
go mod tidy
go build -o playlist-streamer .
```

Or: `make build`

## Configuration

1. Copy `config.yaml` to `config.local.yaml` (or use `-config`).
2. Set `owncast.stream_key` from your Owncast admin (`/admin` → Server → Stream Key).
3. Edit `playlists/default.yaml` with your YouTube video URLs.

## Usage

```bash
# Stream immediately (uses first playlist in playlists/)
./playlist-streamer -run

# Stream with specific playlist
./playlist-streamer -run -playlist default.yaml

# Run 24/7 (loops playlist when configured)
./playlist-streamer -daemon=continuous

# Run on playlist schedule (cron in playlist yaml)
./playlist-streamer -daemon=schedule
```

## Playlist Format

```yaml
playlists:
  - name: "My Playlist"
    videos:
      - url: "https://youtube.com/watch?v=..."
        provider: youtube
    schedule: "0 9 * * *"  # Optional cron (9:00 daily)
```

## Provider Stubs

- **YouTube**: Implemented via yt-dlp.
- **Kick / Twitch**: Stub implementations return `ErrNotImplemented`. Add real support later using each platform's VOD API.

## Stream Key Security

Store `config.local.yaml` with restricted permissions and never commit it to version control.
