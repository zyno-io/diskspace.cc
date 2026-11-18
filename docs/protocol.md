# diskspace.cc wire protocol (v1)

Agent (Go) and viewer (browser) talk end to end through an untrusted relay that only copies binary WebSocket messages. Everything below is implemented in `internal/session/crypto`, `internal/proto`, and `web/src/protocol`. Test vectors: `testdata/vectors/*.json` (regenerate with `go test ./internal/... -run TestVectors -update`; TS tests verify them byte for byte).

Notation: `‖` concatenation; `L(x)` = ASCII `"diskspace.cc v1 " + x`; `uv` = unsigned LEB128 (values up to 2^53-1 on the TS side); `u64be` big-endian.

## 1. Link secret

- `sid`: 16 random bytes, URL path, base64url without padding (22 chars).
- `K`: 32 random bytes, URL fragment, base64url (43 chars). Never sent to the relay.
- `https://diskspace.cc/s/<sid>#<K>`. The viewer moves K to sessionStorage and strips the fragment.
- The relay-served-JS caveat applies: a malicious relay could serve modified viewer code. Local mode (agent serves the SPA on 127.0.0.1) mitigates this.

## 2. Handshake

`k_auth = HKDF-SHA256(ikm=K, salt=L("auth"), info=sid, 32)`

| Msg | Layout (82 bytes) |
| --- | --- |
| M1 VIEWER_HELLO | `[0]=0xD1 [1]=0x01 [2:34]=pv (X25519 pub) [34:50]=nv (random) [50:82]=tv` |
| M2 AGENT_HELLO | `[0]=0xD2 [1]=0x01 [2:34]=pa [34:50]=na [50:82]=ta` |

- `tv = HMAC-SHA256(k_auth, L("vh") ‖ sid ‖ M1[0:50])`
- `ta = HMAC-SHA256(k_auth, L("ah") ‖ sid ‖ M1 ‖ M2[0:50])`
- Agent checks in order: length 82 and type 0xD1, version, `tv` (constant time). Failure closes the socket without doing any DH or allocating state: malformed or bad MAC → 4401, unknown version → 4400. Agents rate-limit to 20 handshake attempts per minute.
- Agent generates a fresh X25519 key per connection. Viewer verifies `ta`, else shows "Key mismatch — stale link or tampering".
- `dh = X25519(own, peer)`; all-zero output aborts.
- `TH = SHA-256(L("th") ‖ sid ‖ M1 ‖ M2)`
- `k_a2v ‖ k_v2a = HKDF-SHA256(ikm=dh, salt=K, info=L("keys") ‖ TH, 64)`
- `code = u32be(HKDF-SHA256(ikm=dh, salt=K, info=L("code") ‖ TH, 4)) mod 10^6`, shown as `"123 456"` on both sides.
- Viewer frame #0 is CLIENT_HELLO under `k_v2a`. The agent waits up to 10 s; if its `proto` differs from the agent's application protocol version the agent closes with 4400; otherwise, only after decrypting it, the agent marks the viewer active (superseding any older viewer) and sends STREAM_INIT, AGENT_INFO, then the scan replay.
- Viewer treats successful decryption and parsing of AGENT_INFO as key confirmation and shows "End-to-end encrypted · code 123 456".

## 3. Frames

One WebSocket binary message = one AEAD frame: `AES-256-GCM(key_dir, nonce, aad, plaintext)` (16-byte tag appended).

- nonce = `00 00 00 00 ‖ u64be(seq)`, seq implicit, per direction, starts at 0, never on the wire. Reaching 2^32 is an error: close and re-handshake.
- aad = `L("a2v")` for agent→viewer, `L("v2a")` for viewer→agent (direction separation defeats reflection).
- Any drop, duplicate, reorder, inject or reflect fails GCM → close 4402. Reconnect is a full new handshake (new keys, counters and zstd context); the agent replays its scan log.
- Plaintext = `[codec u8][body]`. Codec 0: raw. Codec 1: zstd (a2v only). Body = `[msgType u8][payload]`, at most 1 MiB. v2a is always codec 0 and small.

### Compression (codec 1)

Bodies under 512 B are sent as codec 0. Larger bodies are codec 1:

- `caps.zstdStream = true`: one zstd stream per connection (encoder: SpeedDefault, 4 MiB window, concurrency 1, no CRC). Each message is written then flushed (non-final block); the produced bytes are the codec-1 payload. The viewer pushes each payload to a streaming decoder and expects one message (1 B to 1 MiB) per push.
- The first codec-1 frame is always STREAM_INIT (`0x00` + 32 random bytes, compressed). The viewer's streaming decoder buffers the first ~18 bytes of a frame before emitting anything, so a tiny first real message would stall; STREAM_INIT primes it and is discarded.
- `caps.zstdStream = false`: every codec-1 payload is an independent zstd frame.
- Viewer validation: the first frame header must have no dictionary, no skippable frame, window ≤ 8 MiB; each push must yield 1 B to 1 MiB of output, else tear down.

## 4. Flow control

The agent starts with an implicit 8 MiB credit window measured in ciphertext bytes. Only NODES frames are charged. The viewer accumulates ciphertext bytes of NODES batches its UI has finished applying and sends `CREDIT {n}` each time at least 1 MiB is pending. PROGRESS is droppable when the agent is out of credit. The worker clamps reported bytes to what the current connection delivered.

## 5. Close codes

| Code | Meaning | Viewer state |
| --- | --- | --- |
| 4400 | unknown handshake version, or CLIENT_HELLO `proto` ≠ agent's | error: version ("update diskspace") |
| 4401 | malformed or unauthenticated handshake (key mismatch) | error: key-mismatch |
| 4402 | protocol or AEAD failure | error: protocol |
| 4403 | agent token mismatch (relay) | error: protocol |
| 4404 | unknown session | error: no-session |
| 4408 | session TTL expired | closed: expired |
| 4409 | sid claimed by another token (agent side) | n/a |
| 1000 | clean close | closed |
| other (1006 …) | network drop | reconnecting with exponential backoff (0.5 s to 15 s) |

After `BYE {reason:"superseded"}` the viewer enters `superseded` and does not reconnect until `takeOver()`.

## 6. Messages

Type bytes (`internal/proto/types.go`, `web/src/protocol/types.ts`). JSON payloads use the field names in `types.ts`; fields typed `| null` are always present.

| Dir | Byte | Name | Payload |
| --- | --- | --- | --- |
| a2v | 0x00 | STREAM_INIT | 32 random bytes, codec 1, discarded |
| a2v | 0x01 | AGENT_INFO | JSON |
| a2v | 0x02 | ROOTS | reserved |
| a2v | 0x03 | BROWSE_RESULT | `{reqId,path,parent,dirs[],error?}` |
| a2v | 0x08 | PONG | none |
| a2v | 0x10 | SCAN_STARTED | `{scanId,path,rootName,volume,opts}` |
| a2v | 0x11 | NODES | binary |
| a2v | 0x12 | PROGRESS | JSON, ~4 Hz |
| a2v | 0x13 | SCAN_DONE | JSON |
| a2v | 0x14 | SCAN_ENDED | `{scanId,reason,message?}` |
| a2v | 0x15 | DIR_FILES | binary |
| a2v | 0x16 | REPLAY_DONE | none |
| a2v | 0x17 | SCAN_STATE | `{scanId,paused,elapsedMs}`; only to viewers with `caps.scanState` |
| a2v | 0x7E | BYE | `{reason}` (`superseded`, `shutdown`, `idle`) |
| a2v | 0x7F | ERROR | `{code,message}` (e.g. `replay_unavailable`) |
| v2a | 0x81 | CLIENT_HELLO | `{proto:1,ua,caps:{zstdStream,scanState?}}` (frame #0) |
| v2a | 0x82 | GET_ROOTS | none |
| v2a | 0x83 | BROWSE | `{reqId,path}` |
| v2a | 0x84 | SCAN_START | `{path,crossMounts:false,topFiles?,minFileBytes?}` |
| v2a | 0x85 | SCAN_CANCEL | `{scanId}` |
| v2a | 0x86 | DIR_FILES_REQ | `{reqId,scanId,dirId,limit<=10000}` |
| v2a | 0x87 | CREDIT | `{n}` |
| v2a | 0x88 | PING | none |
| v2a | 0x89 | SCAN_PAUSE | `{scanId,paused}`; an idempotent set, not a toggle; only if `AGENT_INFO.features.pause` |

Order on a fresh connection: STREAM_INIT, AGENT_INFO, then replay (SCAN_STARTED, SCAN_STATE if paused, every logged NODES, then SCAN_DONE or SCAN_ENDED if the scan had already ended), REPLAY_DONE, then live traffic. A canceled scan replays its NODES too, so a reconnect after Stop shows the partial tree. REPLAY_DONE follows the end message only when the scan had already ended at attach time; a scan still running at attach gets REPLAY_DONE as soon as the replay has delivered what was in the log then, and any end message comes later, live. A live viewer of a scan it stopped receives SCAN_ENDED promptly, without the NODES it has not yet been sent. An ended scan has no pause state: the agent sends no SCAN_STATE for it. The viewer rejects any other first messages. On reconnect the replay repeats from the start of the scan; consumers must treat a SCAN_STARTED for an already-known scanId as a restart.

## 7. NODES and DIR_FILES

```
NODES   := 0x11 scanId:uv count:uv record*
DIR     := 0x01 dirId:uv flags:uv ownAlloc:uv ownApparent:uv ownFiles:uv entryErrors:uv
           nTotal:uv [firstChildId:uv if nTotal>0] nInline:uv (name childFlags:uv)×nInline
           nFiles:uv (name alloc:uv apparent:uv fileFlags:uv)×nFiles
FOLD    := 0x02 targetId:uv alloc:uv apparent:uv files:uv dirs:uv
FOLDEND := 0x03 targetId:uv
DIRMORE := 0x04 dirId:uv firstChildId:uv n:uv (name childFlags:uv)×n        (n ≥ 1)
DIR_FILES := 0x15 reqId scanId dirId flags restCount restAlloc restApparent n:uv (name alloc apparent fileFlags)×n
name    := len:uv (≤ 4096) bytes
```

- Child i of a DIR has id `firstChildId + i`. Flagged children never get their own DIR record. Root has dirId 0.
- A DIR announces nTotal child dirs (ids `firstChildId .. firstChildId+nTotal-1`) and carries the first nInline ≤ nTotal of them. If nInline < nTotal, DIRMORE records for the same `dirId` follow, in order, carrying the rest. A huge subdirectory list is thus split into chunks of about 256 KiB of encoded names, all emitted before any record of those children, so no record approaches the 1 MiB message limit and nothing is folded for being too wide.
- DIRMORE `firstChildId` must equal the next unnamed slot (`firstChildId + names so far`), and the total named must never exceed nTotal. Viewers reserve all nTotal slots at DIR time; slots not yet named are placeholders (not listed, size 0) that keep the directory unsettled. A DIRMORE for a dir whose DIR has been applied but with a wrong `firstChildId`, or overflowing nTotal, is dropped. Records for an id that is unknown or still an unnamed placeholder (DIR, DIRMORE, FOLD, FOLDEND) are buffered by wire id and applied in arrival order once the id is named; any still buffered at SCAN_DONE are reported as unattached folders.
- nTotal is capped at 2,000,000 (the per-scan announced-directory limit); decoders reject more, and a viewer refuses DIRs that would take its cumulative announced total past the cap. An empty DIRMORE and nInline > nTotal are protocol errors.
- Flags: dir `READ_DENIED=1 READ_ERROR=2 VANISHED=4 FOLDED_CHILDREN=8 HAS_ENTRY_ERRORS=16`; child `OTHER_DEVICE=1 REPARSE=2 EXCLUDED=4 DUP_DIR=8 BIND_MOUNT=16`; file `MULTILINK=1 SYMLINK=2 SPARSE=4 CLOUD=8 SPECIAL=16 BAD_NAME=32`.
- Names are raw bytes (Linux) or WTF-8 (Windows). The viewer decodes UTF-8; if the bytes are invalid it substitutes U+FFFD and sets BAD_NAME (32) on the file or child (the decoder synthesizes bit 32 for children).
- Decoders are strict: counts must be plausible against the remaining bytes (minimum element size), names ≤ 4096, no trailing bytes, unknown record kinds rejected.
- Batching: the agent closes a NODES message at 64 KiB or 100 ms (first of a scan at 30 ms).
- `ownAlloc/ownApparent/ownFiles` cover all non-directory entries directly in the directory (after hardlink dedup) plus its own blocks; the viewer derives the "other files" bucket as own minus the listed files.

## 8. Extensions: capabilities, pause, latest release

The protocol version stays 1 and must match exactly. Optional features are negotiated instead, so a new viewer can meet an old agent and the other way round (the relay serves the viewer, agents are separate binaries):

- Viewer to agent: `CLIENT_HELLO.caps.scanState: true` says the viewer understands SCAN_STATE.
- Agent to viewer: `AGENT_INFO.features` lists what the agent accepts (`{pause: true}`); `AGENT_INFO.scan.paused` (omitted when false) reports a paused running scan.
- **Extension rule:** each side sends a message type introduced after v0.1.x only if the peer advertised it. Unknown JSON fields are ignored on both sides, but an unknown message type stays fatal (the agent closes with 4402, the viewer shows a protocol error). Never rely on the peer ignoring a type.

Pause: the viewer sends `SCAN_PAUSE {scanId, paused}`. The agent applies it to the current running scan (any other request changes nothing) and always answers the active viewer with `SCAN_STATE {scanId, paused, elapsedMs}` on the control queue, which is never credit-gated. `elapsedMs` and `PROGRESS.elapsedMs` / `SCAN_DONE.ms` leave out paused time, and no PROGRESS is sent while paused. A viewer that did not advertise `scanState` cannot see or end a pause, so attaching one to a paused scan resumes it.

Latest release: the relay serves `GET /v1/latest` as `{"version":"0.1.2"}` (the newest stable release; empty when unknown). It is plain HTTP on the viewer's own origin, outside the E2E channel, and carries nothing about the session. Viewers, the desktop app and the CLI compare it with `AGENT_INFO.version` (dev builds and prereleases of the same version are ignored). A viewer served by the agent itself has no relay, so no banner.
