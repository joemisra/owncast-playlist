# couch-layers

Canvas compositing system for couch.dsp.coffee. Layers WebGL shader effects over the Owncast video player, synced across all viewers in real time.

## Architecture

```
Browser (each viewer)
  ├── couch-layers.js        ← WebGL rendering, chat bridge, SSE client
  ├── Owncast chat WebSocket ← detects ! commands
  └── SSE (/layers-api/events) ← receives canonical state

Layer Server (Go, port 9100)
  ├── /layers-api/state      ← GET current state, POST commands
  ├── /layers-api/events     ← SSE stream (pushes state to all clients)
  └── allowlist.json         ← who can run commands

Caddy
  └── reverse_proxy /layers-api/* → localhost:9100
```

When an allowed user types a `!` command in chat, every viewer's chat bridge detects it, but only the first POST is processed (deduplication by message ID). The server updates the canonical state and pushes it to all viewers via SSE.

## Chat Commands

All commands are prefixed with `!`. Only users in the allowlist can execute them.

### Presets (quickstart)

Load a full layer stack in one command:

```
!preset crt           CRT scanlines + barrel distortion
!preset vaporwave     Color grading + chromatic aberration
!preset glitchcore    Glitch blocks + pixelation
!preset vhs_tape      VHS tracking + noise
!preset nightvision   Green-tinted high-contrast
!preset clean         Clear all layers (empty stack)
```

### Layer Management

```
!layer add shader crt           Add a CRT effect layer
!layer add shader chromatic     Add chromatic aberration
!layer add shader glitch        Add a glitch effect
!layer rm 0                     Remove the first layer (0-indexed)
!clear                          Remove all layers
!layers off                     Hide all layers (keeps them loaded)
!layers on                      Show all layers
```

### Tweaking Parameters

Adjust a specific parameter on a layer by effect name:

```
!fx crt scanlineIntensity 0.8   Stronger scanlines (default 0.5)
!fx crt curvature 0.5           More barrel distortion (default 0.3)
!fx chromatic amount 5.0        Heavier RGB split (default 2.0)
!fx chromatic angle 1.57        Vertical split (default 0.0, radians)
!fx glitch intensity 0.9        More glitch (default 0.5)
!fx glitch blockSize 32         Larger glitch blocks (default 16)
!fx vhs tracking 0.9            Worse tracking (default 0.5)
!fx vhs noise 0.6               More noise (default 0.3)
!fx colorgrade hue 180          Shift hue (0-360)
!fx colorgrade saturation 2.0   Boost saturation (default 1.0)
!fx colorgrade brightness 0.1   Brighten (default 0)
!fx colorgrade contrast 1.5     Increase contrast (default 1.0)
!fx pixelate pixelSize 8        Larger pixels (default 4)
!fx invert amount 0.5           Partial inversion (default 1.0)
```

### Blend Modes & Opacity

```
!blend 0 screen                 Set layer 0 to screen blend
!blend 0 multiply               Multiply blend
!blend 0 overlay                Overlay blend
!blend 0 normal                 Back to normal
!opacity 0 0.5                  Set layer 0 to 50% opacity
!opacity 0 1.0                  Back to full opacity
```

Available blend modes: `normal`, `multiply`, `screen`, `overlay`, `darken`, `lighten`, `color-dodge`, `color-burn`, `hard-light`, `soft-light`, `difference`, `exclusion`, `hue`, `saturation`, `color`, `luminosity`.

### Info Commands (local only, no allowlist needed)

```
!effects                        List available shader effects in HUD
```

## Available Effects

| Effect | Parameters | Description |
|---|---|---|
| `passthrough` | (none) | No-op, useful as a base layer |
| `crt` | `scanlineIntensity`, `curvature` | CRT monitor simulation |
| `chromatic` | `amount`, `angle` | RGB channel separation |
| `colorgrade` | `hue`, `saturation`, `brightness`, `contrast` | Color correction |
| `glitch` | `intensity`, `blockSize` | Digital glitch blocks |
| `vhs` | `tracking`, `noise` | VHS tape distortion |
| `pixelate` | `pixelSize` | Pixelation / mosaic |
| `invert` | `amount` | Color inversion (0-1) |

## Console API

Open browser dev console for direct access. These work locally for experimentation (not synced):

```js
couchLayers.layers.addShaderLayer('crt')     // add a layer locally
couchLayers.layers.clear()                    // clear local layers
couchLayers.hud.toggle()                      // toggle the HUD overlay
```

To send synced commands through the server (same as chat, requires allowlist):

```js
couchLayers.send('preset vaporwave')
couchLayers.send('fx crt scanlineIntensity 0.9')
couchLayers.send('layer add shader glitch')
couchLayers.send('clear')
```

## HUD

Hover over the video player and click the **HUD** button (top-left) to toggle the debug overlay. Shows FPS, active layers with their params/blend modes, and recent command log.

## Allowlist

Edit `/opt/owncast/layer-server/allowlist.json` to control who can run commands. No restart needed — the file is re-read on every command.

```json
[
  "joemisra",
  "friendname",
  "anotherfriend"
]
```

Matching is case-insensitive. You can use either Owncast display names or user IDs. If the file is missing or empty, all users are allowed.

## Recipes

**Dreamy nostalgia** — warm colors, soft CRT, chromatic fringe:
```
!preset clean
!layer add shader colorgrade
!fx colorgrade hue 20
!fx colorgrade saturation 1.3
!fx colorgrade contrast 1.1
!layer add shader crt
!fx crt scanlineIntensity 0.3
!fx crt curvature 0.15
!layer add shader chromatic
!fx chromatic amount 1.5
!opacity 2 0.4
```

**Surveillance cam** — green tint, pixelated, noisy:
```
!preset clean
!layer add shader colorgrade
!fx colorgrade hue 120
!fx colorgrade saturation 0.3
!fx colorgrade contrast 1.6
!layer add shader pixelate
!fx pixelate pixelSize 3
!layer add shader vhs
!fx vhs noise 0.5
!fx vhs tracking 0.2
```

**Subtle enhancement** — just a touch of color grading:
```
!preset clean
!layer add shader colorgrade
!fx colorgrade saturation 1.15
!fx colorgrade contrast 1.05
!fx colorgrade brightness 0.02
```

**Full chaos** — everything at once:
```
!preset glitchcore
!layer add shader vhs
!layer add shader chromatic
!fx chromatic amount 8
!fx glitch intensity 1.0
```

## Server Administration

```bash
# Check server status
systemctl status layer-server

# View logs
journalctl -u layer-server -f

# Restart after code changes
cd /opt/owncast/layer-server && go build -o layer-server . && systemctl restart layer-server

# Check current state
curl -s https://couch.dsp.coffee/layers-api/state | python3 -m json.tool

# Force a preset from the server (bypasses allowlist)
curl -s -X POST http://localhost:9100/layers-api/state \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"preset","args":["crt"]}'

# Clear all layers from the server
curl -s -X POST http://localhost:9100/layers-api/state \
  -H 'Content-Type: application/json' \
  -d '{"cmd":"clear","args":[]}'
```

Note: POSTing directly to `localhost:9100` bypasses the allowlist check since no `userName` is provided and the allowlist permits requests without user info. Use this for admin overrides.

## File Locations

```
/opt/owncast/owncast/data/public/couch-layers/couch-layers.js   Client-side code
/opt/owncast/layer-server/main.go                                State server source
/opt/owncast/layer-server/layer-server                           Compiled binary
/opt/owncast/layer-server/allowlist.json                         Allowed users
/etc/systemd/system/layer-server.service                         Systemd unit
/etc/caddy/Caddyfile                                             Proxy config
```
