# diskspace.cc

See what's filling a server's disk, from your browser.

Run one command on the box, over SSH or at a console. It prints a secure link. Open the link in a browser on your laptop, pick a folder, and watch a treemap of the server's disk paint itself as the scan streams in. The link is end-to-end encrypted: the relay in the middle never sees a single file name.

```sh
ssh web-01
curl -fsSL https://diskspace.cc/run | sh -s -- /
# open the printed link on your own machine
```

The same command works on a Linux or macOS machine in front of you; leave off the path to pick a folder in the browser.

```powershell
# Windows: open PowerShell, then paste
irm https://diskspace.cc/run.ps1 | iex
```

On Windows, paste the command into a PowerShell window. Microsoft Defender blocks the same command when it's launched as a one-liner from Run or `cmd` (`powershell -c "irm … | iex"`), because that's how "ClickFix" malware is delivered.

Prefer not to use a terminal on your own computer? Download the desktop app for [macOS](https://diskspace.cc/dl/gui/mac) or [Windows](https://diskspace.cc/dl/gui/windows).

Want to see it first? [Try the demo](https://diskspace.cc/demo) with a made-up server.

## What you get

- **Built for machines with no GUI.** The server runs one small binary; the treemap draws in a browser on any other device.
- **A link, not an install.** The bootstrap script downloads the latest release for your OS/arch, verifies its SHA-256 against the release's `checksums.txt`, runs it from a temp dir and cleans up afterwards.
- **You choose the privilege level.** If you aren't an administrator, diskspace asks whether to restart elevated (`sudo` right in your SSH session, UAC, or the macOS admin prompt) or to continue with just your own files. Enter means "no". On macOS it also tells you whether Full Disk Access is granted and can open that setting.
- **Pick what to scan.** Any mounted volume, the folder you started it from, your home folder, or any other folder.
- **Results stream in.** The treemap and list paint within a fraction of a second and fill in as the scan runs; nothing waits for the full scan.
- **Clean up, with your say-so.** Right-click (or long-press) a folder or file to queue it for deletion, review the queue, then move it to the Trash / Recycle Bin or delete it permanently. Every request is approved on the scanned computer: type `y` in the terminal diskspace runs in, or answer a native dialog in the desktop app. The prompt shows the paths and sizes diskspace measured itself and the browser's verification code. A browser can ask; it can't approve. Without a terminal (e.g. `--quiet` under a service manager) deleting is off.
- **Pause and resume.** Pause stops the scanner from opening new folders (reads in progress finish) and keeps the results so far on screen. Resume continues; Stop ends the scan. A paused scan stays paused across browser reloads, and the elapsed time leaves out the pause.
- **Update notice.** The viewer, the desktop app and the CLI tell you when a newer stable release exists. The relay answers `GET /v1/latest` with `{"version":"0.1.2"}`. A viewer served by the agent itself (`--no-relay`) has no relay to ask and shows no banner.
- **Gentle on production.** The scanner runs at low CPU and disk priority by default (`--priority normal` to go full speed), and the agent exits after 30 minutes without a browser (`--idle`).
- **Fast.** A parallel, work-stealing scanner using the cheapest metadata calls each OS offers. On an M1 Mac it walks 1.1M entries in ~12 s, against ~105 s for `du -sk` on the same tree.
- **Accurate.** Allocated (on-disk) and apparent sizes, hardlinks counted once, mount points and firmlinks not double-counted, junctions/symlinks never followed, unreadable folders reported instead of silently skipped.

## Remote servers

- **Open the link on your laptop, not the server.** The link works from any device that can reach diskspace.cc. On a terminal that can show it, the CLI also prints a QR code for your phone.
- **Elevation over SSH.** Run it as a regular user and say yes to the sudo prompt, or pass `--elevate`. Elevated, anyone with the link can see the names of every file on the box; share it accordingly.
- **Deletes are approved in the server's terminal.** Keep the SSH session open while you clean up; for a long session, run it inside `tmux` or `screen`. Started without a controlling terminal (a systemd unit, cron, `setsid`), deleting is off. Don't background it with `&` or `nohup`: it keeps the terminal but can't read your answer.
- **It cleans up after itself.** It exits when you press Ctrl-C, or after 30 minutes with no browser connected (`--idle 2h`, or `--idle 0` to never exit).
- **Keeping the box off diskspace.cc.** Self-host the relay (below), which also serves the bootstrap script: `curl -fsSL https://disk.example.com/run | sh -s -- --relay https://disk.example.com /`. Or run with `--no-relay`, which serves the viewer on `127.0.0.1:<port>` on the server; forward the same port number (`ssh -L <port>:127.0.0.1:<port> web-01`) and open the printed link on your laptop.

## For AI agents

When a user wants to see what is filling a disk, an agent can run `curl -fsSL https://diskspace.cc/run | sh -s -- --quiet --no-elevate /` in the background on the machine and hand the user the printed link. The link contains the decryption key: share it only with that user. Started without a controlling terminal, deleting is off; a user who wants to clean up should run the command in their own SSH session, since deletion prompts belong to them. Never answer one on the user's behalf. diskspace has no machine-readable output; for numbers of your own, use `du`. A summary for agents is published at [diskspace.cc/llms.txt](https://diskspace.cc/llms.txt).

## CLI

```
diskspace [flags] [path]

  path              scan this folder as soon as a browser connects
  --root DIR        restrict scanning and browsing to DIR
  --elevate         restart as administrator without asking
  --no-elevate      never offer to elevate
  --local           also print a 127.0.0.1 link served by this process
  --no-relay        local link only (no internet needed)
  --open            open the link in your browser
  --idle 30m        exit after this long without a browser (0 = never)
  --relay URL       use a self-hosted relay
  --threads N       scanner workers (default: automatic)
  --priority P      scanner CPU and disk priority: low (default) or normal
```

## How it works

```
 scanned machine                    relay (diskspace.cc)                 your browser
┌─────────────────┐   wss    ┌────────────────────────────┐   wss   ┌──────────────────────┐
│ diskspace agent │◀────────▶│ pairs by session id,       │◀───────▶│ viewer (Web Worker)  │
│  scanner        │ opaque   │ copies binary frames       │ opaque  │  X25519 + AES-GCM    │
│  E2E session    │ frames   │ verbatim; never has a key  │ frames  │  zstd stream decode  │
└─────────────────┘          └────────────────────────────┘         └──────────────────────┘
        link:  https://diskspace.cc/s/<session id>#<32-byte secret K>
                                     └── sent to relay ──┘└─ fragment: never leaves the browser
```

1. **Key in the fragment.** The agent generates a session id and a 32-byte secret `K`. Browsers never send the `#fragment` to servers, so the relay learns only the session id. The viewer moves `K` into session storage and strips it from the address bar.
2. **Authenticated Diffie–Hellman.** Each browser connection runs a fresh ephemeral X25519 exchange. Both hello messages carry an HMAC keyed from `K`, so a relay that substitutes its own public keys is detected. Session keys are derived with HKDF from the DH secret, using `K` as salt and the full handshake transcript as info. Each side shows the same 6-digit verification code. Ephemeral keys give forward secrecy: leaking `K` later does not decrypt recorded traffic.
3. **Encrypted frames.** Every WebSocket message is AES-256-GCM with per-direction keys and an implicit sequence number as nonce. Any dropped, replayed, reordered, reflected or modified frame fails authentication and ends the connection.
4. **Compressed, then encrypted.** Scan results travel as a compact binary format (LEB128 varints; each folder name sent once). That format runs through one streaming zstd context per connection, so repeated names compress across messages. The browser decodes them incrementally with bounded memory.
5. **Streaming scan.** The scanner emits a record per folder: its own size totals, its subfolders (announced before they are scanned) and its largest files. A batcher flushes every 100 ms (30 ms for the first paint), and the browser adds each record's size to all ancestors as it arrives. Credit-based flow control keeps a slow tab from building up memory; the scanner never waits on the network. A reconnecting tab replays the scan log and catches up.

Full byte-level spec: [docs/protocol.md](docs/protocol.md). Design overview: [docs/architecture.md](docs/architecture.md). Threat model: [SECURITY.md](SECURITY.md).

## Repository layout

| Path | What |
|---|---|
| `cmd/diskspace` | CLI (also runs headless as `diskspace agent --ipc …` for the GUI) |
| `cmd/diskspace-relay` | relay server: rendezvous, static site, install scripts, release redirects |
| `gui/` | macOS/Windows desktop app (Wails v2 shell over the same core) |
| `internal/scan` | parallel scanner, per-OS stat/volume code, path guard |
| `internal/session` | agent: relay client, local server, per-viewer E2E connection, scan log |
| `internal/session/crypto`, `internal/proto` | handshake, frame cipher, wire codec, zstd stream |
| `internal/relay` | relay implementation + bootstrap/release handlers |
| `internal/elevate`, `internal/ipc` | elevation prompts/relaunch, authenticated parent↔child IPC |
| `web/` | landing page, viewer and GUI launcher (Vite + Preact + Canvas2D) |

## Building from source

Requires Go 1.27+ and Node 24+.

```sh
npm --prefix web ci && npm --prefix web run build   # web assets embedded into the Go binaries
go build ./cmd/diskspace ./cmd/diskspace-relay
go test ./...                                       # Go tests (scans synthetic temp trees only)
npm --prefix web test                               # viewer + protocol tests, incl. Go↔TS vectors
```

Run everything locally without the internet:

```sh
./diskspace --no-relay ~/Projects      # serves the viewer itself on 127.0.0.1
```

Desktop app: `scripts/package-mac.sh` (universal `.app`, `diskspace-desktop_macos_universal.{dmg,zip}`) and `scripts/package-win.sh` (Windows `Diskspace.exe`, cross-compiled without CGO; zipped as `diskspace-desktop_windows_<arch>.zip` by the release workflow).

### Self-hosting the relay

```sh
diskspace-relay --listen :8080 --public-url https://disk.example.com
diskspace --relay https://disk.example.com
```

The relay keeps no state beyond in-memory session pairing. Put it behind TLS: browsers allow WebCrypto only on `https://` pages or on `127.0.0.1`. Use `--trust-proxy` when it sits behind a reverse proxy.

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yml`: goreleaser builds the CLI archives, packages and relay
image; the macOS app is built on a macOS runner; Windows binaries are cross-built. Everything is signed (Apple
Developer ID + notarization via `rcodesign`, Authenticode via Azure Artifact Signing), `checksums.txt` is
regenerated over the signed files, and the release is published only after all assets are uploaded. The
workflow needs the `release` environment secrets `APPLE_CERTIFICATE_P12`, `APPLE_CERTIFICATE_PASSWORD`,
`APP_STORE_CONNECT_API_KEY`, `AZURE_TENANT_ID`, `AZURE_CLIENT_ID` and `AZURE_CLIENT_SECRET`, and fails
without them. A tag whose release is already published is rejected (use a new tag, or convert the release back to a draft first). Local builds (`scripts/package-mac.sh`, `scripts/package-win.sh`) require the same credentials
for macOS; Windows packaging only builds the unsigned `.exe`.

## Credits

The relay, bootstrap scripts, release resolver and parts of the crypto framing are adapted from [sp2p](https://github.com/zyno-io/sp2p) (MIT). See [NOTICE](NOTICE). SquirrelDisk inspired the product idea; no code from it is used.

## License

[MIT](LICENSE) © Signal24 LLC dba Zyno Consulting
