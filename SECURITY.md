# Security

## Reporting a vulnerability

Please report security issues privately via GitHub Security Advisories on
[zyno-io/diskspace.cc](https://github.com/zyno-io/diskspace.cc/security/advisories/new)
rather than opening a public issue. We aim to respond within a few business days.

## What diskspace exposes

The agent answers **metadata only**: folder and file names, sizes, counts, volume names and
usage, and its hostname, OS and user name. It never reads or sends file contents. It can't write,
delete or execute anything. With `--root DIR`, scanning and browsing are confined to `DIR`
(symlinks are resolved before the check).

Anyone holding the full link, including the `#` part, can see those names while the agent is
running. If the agent runs as administrator, that covers every user's files. Treat the link like a
password, and stop diskspace (Ctrl-C, or quit the app) when you're done. Sessions also end after
`--idle` (default 30 minutes) without a browser, and after 24 hours regardless.

## Threat model

| Party | Can | Cannot |
|---|---|---|
| Relay operator / network attacker | See session ids, IPs, timing and encrypted message sizes; drop or delay traffic | Read or alter payloads; impersonate either side (hellos are HMAC'd with a key derived from the fragment secret `K`, which never reaches the relay); decrypt recorded sessions later (ephemeral X25519 per connection) |
| Someone with only the session id | Open a WebSocket to the relay | Complete the handshake (needs `K`) or disrupt an authenticated viewer (a new viewer supersedes the old one only after proving it knows `K`) |
| Someone with the full link | Browse metadata as above | Anything beyond read-only metadata |
| Other local users (agent elevated) | — | Hijack the GUI↔agent IPC channel. It is loopback only, uses a single-use 32-byte token passed via a 0600 file in a 0700 temp dir, and accepts exactly one connection |
| Websites in your browser (local mode) | — | Talk to the agent's 127.0.0.1 server via DNS rebinding or cross-origin requests (Host and Origin are checked; the WebSocket still requires `K`) |

The protocol is specified in [docs/protocol.md](docs/protocol.md). Go and TypeScript share test
vectors, and the test suite covers tampering cases: bit flips, substituted keys, replayed hellos,
dropped/duplicated/reordered/reflected frames, wrong `K`, and version downgrade.

### Caveat: the relay serves the viewer code

When you open an `https://diskspace.cc/s/…` link, the viewer JavaScript comes from the relay.
A malicious or compromised relay operator could serve JavaScript that reads `K` from the fragment.
The cryptography protects you from the *transport*, not from the code you load. Mitigations:

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
