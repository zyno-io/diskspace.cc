# Security

## Reporting a vulnerability

Please report security issues privately via GitHub Security Advisories on
[zyno-io/diskspace.cc](https://github.com/zyno-io/diskspace.cc/security/advisories/new)
rather than opening a public issue. We aim to respond within a few business days.

## What diskspace exposes

The agent answers **metadata only**: folder and file names, sizes, counts, volume names and
usage, and its hostname, OS and user name. It never reads or sends file contents and never
executes anything. The only thing it changes on disk is what you approve for deletion (below).
With `--root DIR`, scanning, browsing and deleting are confined to `DIR` (symlinks are resolved
before the check).

### Deleting needs approval on the scanned computer

A viewer can ask the agent to move scanned folders and files to the Trash / Recycle Bin, or to
delete them permanently. **Nothing is deleted unless the person at the scanned computer approves
that request there**, through a channel web content can't drive:

- **CLI:** a prompt in the terminal diskspace runs in (`/dev/tty`, or `CONIN$` on Windows; with
  UAC elevation, the original console window). Enter, `n`, Ctrl-D and no answer within 2 minutes
  all decline. A permanent deletion over 100 GB or 100,000 files also needs `delete` typed out.
  Lines typed before the prompt could be read are ignored.
- **Desktop app:** a native dialog (the app's own "Open here" viewer gets the same dialog; the
  page has no way to answer it). Large permanent deletions get a second "can't be undone" dialog.
  An approval given within a second of a dialog appearing (a stray keystroke) is asked again.
- **No terminal** (for example `--quiet` under a service manager): deletion is unavailable.

The viewer never confirms on its own; its deletion queue is only a review step. The terminal
prompt lists every item; the dialog lists the 15 largest and the folder of every item, so a
request can't tuck an extra item out of sight. What the prompt shows is resolved and measured by
the agent: the viewer names scanned entries only by scan id
(plus a file name inside a scanned folder), never by path or size. Names are shown with control,
bidi and invisible characters escaped, so a file name can't fake prompt lines or reorder a path.
The prompt also shows the requesting browser's verification code and whether it connected through
the relay. Compare the code before you approve.

Limits: one request waits for approval at a time; after a refusal (declined, unanswered or
withdrawn) no new request is accepted for 30 seconds; after 3 declined or unanswered requests
deletion is off until diskspace restarts. Deleting is only possible once the scan has finished or
been stopped, and a new scan can't start while a deletion runs.

After approval the removal engine re-checks every entry: it is opened relative to its pinned
parent without following links and must still be the entry that was scanned (device/inode or file
ID), inside the scan root and `--root`. It refuses mount points, reparse points, the scan root and
protected folders (`/`, system folders, every home folder and the standard folders directly in a
home, volume roots, and their ancestors; their contents can be deleted), never leaves the
target's filesystem, and moves to the Trash with no-replace renames. The CLI prints a line for
every requested item of an approved request; the app shows a summary notice.

Anyone holding the full link, including the `#` part, can see those names while the agent is
running. If the agent runs as administrator, that covers every user's files. Treat the link like a
password, and stop diskspace (Ctrl-C, or quit the app) when you're done. Sessions also end after
`--idle` (default 30 minutes) without a browser, and after 24 hours regardless.

## Threat model

| Party | Can | Cannot |
|---|---|---|
| Relay operator / network attacker | See session ids, IPs, timing and encrypted message sizes; drop or delay traffic | Read or alter payloads; impersonate either side (hellos are HMAC'd with a key derived from the fragment secret `K`, which never reaches the relay); decrypt recorded sessions later (ephemeral X25519 per connection) |
| Someone with only the session id | Open a WebSocket to the relay | Complete the handshake (needs `K`) or disrupt an authenticated viewer (a new viewer supersedes the old one only after proving it knows `K`) |
| Someone with the full link | Browse metadata as above; *ask* to delete scanned entries | Delete anything without approval on the scanned computer; name a path or size the agent didn't resolve and measure itself; ask again within 30 s of a refusal, or after 3 |
| A malicious browser extension, compromised relay JavaScript, or a hostile script in the viewer's tab | Everything the link holder can, including sending deletion requests as you | Approve them: approval happens in the terminal or a native dialog on the scanned computer, which shows the agent-resolved paths and the requesting connection's code |
| Other local users (agent elevated) | — | Hijack the GUI↔agent IPC channel. It is loopback only, uses a single-use 32-byte token passed via a 0600 file in a 0700 temp dir, and accepts exactly one connection |
| Websites in your browser (local mode) | — | Talk to the agent's 127.0.0.1 server via DNS rebinding or cross-origin requests (Host and Origin are checked; the WebSocket still requires `K`) |

The protocol is specified in [docs/protocol.md](docs/protocol.md). Go and TypeScript share test
vectors, and the test suite covers tampering cases: bit flips, substituted keys, replayed hellos,
dropped/duplicated/reordered/reflected frames, wrong `K`, and version downgrade.

### Caveat: the relay serves the viewer code

When you open an `https://diskspace.cc/s/…` link, the viewer JavaScript comes from the relay.
A malicious or compromised relay operator could serve JavaScript that reads `K` from the fragment.
The cryptography protects you from the *transport*, not from the code you load. Such code could
also send deletion requests, but not approve them: read the prompt on the scanned computer before
you answer `y`. Mitigations:

- `diskspace --no-relay` (or `--local`) serves the viewer from the agent itself at
  `http://127.0.0.1:<port>/…`. Nothing leaves your machine.
- The desktop app's "Open here" uses that local viewer.
- Self-host the relay (`diskspace-relay`) from source you have reviewed.

### Compression and length

Payloads are compressed before encryption, so encrypted sizes reveal roughly how compressible your
directory listing is. File names are not chosen by a remote attacker, so the CRIME/BREACH class of
attacks does not apply. Size metadata is not considered secret from the relay.

## Install scripts

`/run` and `/run.ps1` download the release archive for your platform. They accept only URLs under
`https://github.com/zyno-io/diskspace.cc/releases/download/<tag>/` and verify the archive's SHA-256
against that release's `checksums.txt` before extracting or running anything. They run the binary
from a temporary directory that is removed when it exits. If you'd rather not pipe a script into a
shell, download the archive and checksums from the GitHub release yourself.

## Release signing

Releases are built and signed by `.github/workflows/release.yml` from a `v*` tag, with secrets held in the
`release` GitHub environment (restricted to `main` and `v*` tags). The workflow refuses to run without them,
so unsigned binaries are never published.

- **macOS:** the `diskspace` binaries in the darwin archives are signed (Developer ID, hardened runtime) and
  notarized with `rcodesign`. A bare binary cannot carry a stapled ticket, so Gatekeeper checks it online.
  `Diskspace.app` is signed, notarized and stapled; the `diskspace-desktop_macos_universal.{dmg,zip}` are built from the stapled app and the
  `.dmg` is itself signed, notarized and stapled.
- **Windows:** `diskspace.exe` and `Diskspace.exe` (amd64, arm64) are Authenticode-signed with Azure Artifact
  Signing, and each signature is verified before the archives are rebuilt.
- **Integrity:** `checksums.txt` is generated after all signing, covers every release asset, and is uploaded
  last. Build provenance is attested with GitHub artifact attestations. The release stays a draft until every
  asset is uploaded.

Microsoft Defender flags download-and-run PowerShell passed on a command line
(`powershell -c "irm … | iex"`, as typed into Run or `cmd`) as ClickFix malware and kills it.
Pasting `irm https://diskspace.cc/run.ps1 | iex` into an open PowerShell window is not flagged
(tested with Windows 11 and Windows PowerShell 5.1). Keep instructions in that form.
