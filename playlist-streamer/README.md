# Playlist Streamer

Streams YouTube, Plex, local, HTTP, and Real-Debrid media to Owncast via RTMP. Supports playlists, optional cron scheduling, looping, and a web dashboard for managing playback.

## Prerequisites

- [Go](https://go.dev/dl/) 1.24+
- [yt-dlp](https://github.com/yt-dlp/yt-dlp) — **must be recent** (YouTube changes often break old versions)
- [Node.js](https://nodejs.org/) v20+ — required for YouTube's n-challenge (`sudo apt install nodejs`)
- [ffmpeg](https://ffmpeg.org/)
- Owncast running with a known stream key

### Updating yt-dlp

If your system yt-dlp is old (check with `yt-dlp --version`), use the bundled updater:

```bash
cd playlist-streamer
./update-ytdlp.sh
```

Then in `config.local.yaml`:
```yaml
streamer:
  ytdlp_path: "/opt/owncast/playlist-streamer/yt-dlp"  # or ./yt-dlp if run from that dir
```

## YouTube "bot" / sign-in

If YouTube blocks yt-dlp as a bot, use browser cookies:

1. **On a machine where you can log in to YouTube** (e.g. your desktop):
   - Install a cookies export extension ([Get cookies.txt](https://chromewebstore.google.com/detail/get-cookiestxt/bgaddhkoddajcdgocldbbfleckgcbcid) for Chrome)
   - Log in to YouTube, go to youtube.com
   - Export cookies as `cookies.txt` (Netscape format)
2. **Copy the file to your server**, e.g. `scp cookies.txt user@server:/opt/owncast/playlist-streamer/`
3. **In config.local.yaml**, set:
   ```yaml
   streamer:
     cookies_file: "/opt/owncast/playlist-streamer/cookies.txt"
   ```

**Security**: Treat `cookies.txt` like a password — restrict permissions (`chmod 600`) and don't commit to git.

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

### Plex

Add one or more Plex servers to `config.local.yaml`, or configure them from the
dashboard's Config tab:

```yaml
plex:
  servers:
    - name: home
      base_url: "http://plex.example:32400"
      token: "your-X-Plex-Token"
```

Treat the Plex token like a password and keep it out of Git. The dashboard's
Plex tab organizes media as:

- Movies → movie title → media file
- TV Shows → show → season → episode and media file

Use the filter to search titles or filenames, then select `+` beside a file to
add its stable `plex://server/rating-key` URL to the current playlist. Plex
tokens and direct media URLs are never stored in playlist files.

### Reverse proxy path

When the dashboard is mounted beneath a reverse-proxy path, pass that mount in
`X-Forwarded-Prefix` so unauthenticated redirects stay beneath the public path:

```caddyfile
handle_path /stream/* {
    reverse_proxy localhost:9090 {
        header_up X-Forwarded-Prefix /stream
    }
}
```

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
