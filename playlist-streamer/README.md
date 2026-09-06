# Playlist Streamer

Streams YouTube, SMB, Plex, local, HTTP, and Real-Debrid media to Owncast via RTMP. Supports playlists, optional cron scheduling, looping, and a web dashboard for managing playback.

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

streamer:
  plex_cache_enabled: true
  plex_cache_dir: "data/tmp/plex-cache"
  plex_cache_max_gb: 20
  plex_cache_min_free_gb: 10
```

Treat the Plex token like a password and keep it out of Git. The dashboard's
Plex tab organizes media as:

- Movies → movie title → media file
- TV Shows → show → season → episode and media file

Use the filter to search titles or filenames, then select `+` beside a file to
add its stable `plex://server/rating-key` URL to the current playlist. Plex
tokens and direct media URLs are never stored in playlist files.

When the Plex cache is enabled, media is downloaded completely before playback.
Interrupted downloads remain as `.partial` files and resume on the next attempt.
Older files are removed least-recently-used while preserving both the configured
cache limit and minimum free disk space. Cache filenames are opaque and never
contain Plex URLs or tokens.

Plex and local-file playlists are normalized to a consistent 720p video and
audio format with fixed two-second keyframes, then published through one
FFmpeg/RTMP connection. This applies to single-item playlists as well. Natural
item transitions therefore remain inside the same Owncast live session, and an
Owncast output configured for passthrough can segment the stream cleanly.
Manual skip/jump operations still restart the publisher at the requested item.

Plex must report media-part URLs that are readable from the streaming server.
The dashboard can still list cached Plex metadata when the underlying library
storage is unavailable, so successful browsing alone does not guarantee that a
title can be streamed.

### SMB media libraries

The Couch defaults use the existing read-only mounts at
`/mnt/owncast-media/movies` and `/mnt/owncast-media/tv`. For another server or
different mount points, give each mount a short name in `config.local.yaml`:

```yaml
smb:
  shares:
    - name: movies
      path: "/mnt/owncast-media/movies"
    - name: tv
      path: "/mnt/owncast-media/tv"
```

SMB credentials remain in the operating system's protected mount configuration;
they are never exposed to playlist-streamer or stored in playlists. The Trees
tab browses one directory at a time and adds logical URLs such as
`smb://tv/Show/Season%2001/Episode%2001.mkv`.

With remote caching enabled, the first SMB item is copied to local disk before
playback. Later items are prefetched in playlist order while the current item is
streaming. Partial copies resume after a connection interruption. This avoids
streaming directly from an SMB mount while also avoiding a manual upload of the
entire playlist before it starts.

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
- **SMB**: Implemented through read-only operating-system mounts and local cache prefetch.
- **Kick / Twitch**: Stub implementations return `ErrNotImplemented`. Add real support later using each platform's VOD API.

## Stream Key Security

Store `config.local.yaml` with restricted permissions and never commit it to version control.
