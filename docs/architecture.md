# Architecture

diskspace is one Go module with a shared core and three thin shells (CLI, desktop app, relay), plus a web viewer.

```
            ┌──────────── shared core (Go) ───────────┐
 CLI ──────▶│ internal/scan     parallel scanner       │
            │ internal/session  agent + E2E sessions   │──wss──▶ relay ──wss──▶ browser viewer
 GUI ─spawn▶│ internal/proto    wire codec + zstd      │
 (Wails)    │ internal/elevate  privilege prompts      │──http──▶ 127.0.0.1 viewer (local mode)
            └──────────────────────────────────────────┘
```

## Session and transport

- **Link:** `https://<relay>/s/<sid>#<K>`. `sid` is 16 random bytes and only routes. `K` is 32 random bytes and stays in the fragment. A third value, the agent token `T`, authenticates the agent to the relay; the relay stores only `sha256(T)`.
- **Relay:** the agent holds a control WebSocket. When a browser connects to `/v1/view`, the relay tells the agent, and the agent dials a dedicated data socket for that viewer. The relay then splices the two sockets and copies binary messages verbatim. It enforces message-size, rate, session-count, bandwidth and lifetime limits, and never holds a key.
- **Handshake** (per connection): VIEWER_HELLO and AGENT_HELLO each carry an ephemeral X25519 public key, a nonce and an HMAC keyed by `HKDF(K)`. Session keys are `HKDF(ikm = DH, salt = K, info = transcript hash)`. The viewer's first encrypted frame confirms its key; only then does the agent accept the viewer (and supersede any older one). The agent's first encrypted frame confirms the agent's key to the viewer.
- **Frames:** AES-256-GCM per direction, with nonce = implicit sequence number and the direction label as AAD. The plaintext is `[codec][type][payload]`. Agent→viewer bodies of 512 B and up go through one streaming zstd encoder per connection (flushed per message). The browser keeps one streaming decoder with a bounded window and bounded per-message output.
- **Flow control:** the viewer returns credit after the main thread has *applied* each batch. The agent stops sending scan data when credit runs out. The scanner keeps writing to an in-memory scan log in either case, so a slow browser never slows the scan.
- **Viewer pacing:** the worker keeps at most two decoded NODES batches in flight to the main thread (decoding only when it posts one) and holds the rest as raw bodies; each "applied" report releases one. PROGRESS is latest-wins and bypasses the queue; SCAN_DONE/ENDED/REPLAY_DONE stay ordered behind queued NODES, and a new SCAN_STARTED drops (and credits) queued NODES of older scans. On the main thread an ingest scheduler (`web/src/ui/ingest.ts`) applies batches in slices of an adaptive 2-8 ms frame budget (3 ms while the user is interacting, 25 ms slices while the tab is hidden), so rendering and input stay responsive. Credit is returned only once a batch is fully applied. No wire-format change.
- **Agent priority:** the agent runs at low CPU and disk priority by default (`--priority low`; `normal` opts out) so a big scan does not starve the desktop: nice 10 plus utility I/O tier on macOS, below-normal priority class on Windows, per-worker-thread nice and best-effort ioprio 7 on Linux. No cgo.
- **Local mode:** the agent can serve the same SPA and `/v1/view` on `127.0.0.1:<random>`, with Host/Origin checks and the same handshake. The desktop app's in-window viewer and `--no-relay` use it.

## Scanner (`internal/scan`)

- **Scheduling:** work-stealing deques, one per worker (clamped to 2×CPUs, between 8 and 32). Owners pop LIFO (depth-first, bounded memory); thieves steal the shallowest directories, which spreads early work across top-level folders so the treemap's proportions settle quickly.
- **Per-OS listing:** `openat` + `getdents` + `fstatat(AT_SYMLINK_NOFOLLOW)` on macOS/Linux. On Windows, `GetFileInformationByHandleEx(FileIdExtdDirectoryInfo)` returns sizes, attributes, reparse tags and file IDs for a whole directory in 64 KiB batches, with no per-file open.
- **Accounting:** allocated bytes (`st_blocks × 512` / AllocationSize) and apparent bytes. Hardlinks are counted once by (device, inode) or file ID. Other devices, Linux bind mounts and Windows name-surrogate reparse points are flagged, not descended. On macOS APFS, the Data volume's firmlinked roots are skipped so nothing is counted twice.
- **Ordering:** a directory's record (its own totals, its subdirectories with pre-assigned IDs, its 8 largest files ≥ 1 MiB) is emitted before its children are queued, so every record's parent is already known downstream.
- **Bounds:** past 2M announced directories or 512 levels deep, subtrees are reported as folded totals instead of individual nodes. A directory with a very large subdirectory list (tens of thousands of long names) is not folded: the DIR announces the total child count and carries the first ~256 KiB of names, and DIRMORE records carry the rest. The viewer reserves every child slot once, so node indices stay stable. The "N other files" bucket is the directory's own total minus its listed files. The viewer fetches a full file listing for a directory only when you expand it.

## Viewer (`web/`)

- **Worker:** a Web Worker owns the WebSocket, handshake, decryption, zstd and binary decoding. It posts decoded batches to the main thread.
- **Store:** a struct-of-arrays tree held in typed arrays. Each record adds its bytes to all its ancestors, so totals are always current while data is still arriving.
- **Treemap:** Canvas2D, squarified and nested, laid out only for the current zoom root. Parcels too small to draw collapse per folder, so drawing cost follows screen size, not tree size. While a scan runs it re-lays out every max(250 ms, 8× the last layout time) (at most every 2 s) and tweens parcels between layouts only while the main thread is idle and layout plus draw are cheap; zoom is a 250 ms crossfade. Labels choose the widest stretch of the Anybody variable font that fits.
- **List:** a virtualized tree list (pooled 28 px rows), synced with the treemap selection.

## Privilege elevation (`internal/elevate`)

- **When it asks:** before any network activity, and only if the process isn't already administrator/root. The prompt reads from `/dev/tty` / `CONIN$`, so it works under `curl | sh`. The default is to continue unelevated.
- **Unix:** `sudo -v`, then `exec sudo -- <self> …` (same PID, so the bootstrap cleanup still runs).
- **Windows:** `ShellExecuteExW("runas")` starts a hidden agent child. The original console renders its status over loopback IPC.
- **Desktop app:** the app spawns *itself* in `agent` mode, through `osascript … with administrator privileges` on macOS or UAC on Windows. Only the scanner process is elevated, never the UI.
- **Relaunch arguments:** the original working directory, home and relay are passed to the elevated child as flags, because `sudo` resets the environment and Windows starts elevated processes in System32.

## Release and install

- **Builds:** goreleaser builds static (`CGO_ENABLED=0`) CLI archives for darwin/linux/windows × amd64/arm64, plus `checksums.txt`, the relay archive and image, and deb/rpm/apk packages.
- **Desktop app:** built by separate jobs. macOS produces a universal `.app` (CGO, `lipo`), signed, notarized and stapled with `rcodesign`, then packed as `diskspace-desktop_macos_universal.dmg` (also signed/notarized/stapled) and `.zip`. Windows is cross-compiled without CGO as `.exe` zips and Authenticode-signed. Darwin CLI binaries are signed and notarized; `checksums.txt` is regenerated after signing and uploaded last. See `.github/workflows/release.yml`.
- **Install links:** the relay serves `/run` and `/run.ps1`. Each resolves the newest release that has an asset for the caller's platform, checks the URL prefix, and verifies the archive's SHA-256 before running it.
