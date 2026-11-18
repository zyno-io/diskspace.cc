// SPDX-License-Identifier: MIT

package proto

import (
	"encoding/json"
	"errors"
)

// Message type bytes (mirror web/src/protocol/types.ts MSG).
const (
	// agent -> viewer
	MsgStreamInit   = 0x00
	MsgAgentInfo    = 0x01
	MsgRoots        = 0x02
	MsgBrowseResult = 0x03
	MsgPong         = 0x08
	MsgScanStarted  = 0x10
	MsgNodes        = 0x11
	MsgProgress     = 0x12
	MsgScanDone     = 0x13
	MsgScanEnded    = 0x14
	MsgDirFiles     = 0x15
	MsgReplayDone   = 0x16
	MsgScanState    = 0x17 // only sent to viewers that advertised caps.scanState
	MsgDeleteStatus = 0x18 // only sent to viewers that advertised caps.del
	MsgDeleted      = 0x19 // only sent to viewers that advertised caps.del
	MsgBye          = 0x7e
	MsgError        = 0x7f
	// viewer -> agent
	MsgClientHello  = 0x81
	MsgGetRoots     = 0x82
	MsgBrowse       = 0x83
	MsgScanStart    = 0x84
	MsgScanCancel   = 0x85
	MsgDirFilesReq  = 0x86
	MsgCredit       = 0x87
	MsgPing         = 0x88
	MsgScanPause    = 0x89 // only sent when AGENT_INFO.features.pause
	MsgDeleteReq    = 0x8a // only sent when AGENT_INFO.features.delete
	MsgDeleteCancel = 0x8b
)

// Codec bytes at the start of every plaintext frame.
const (
	CodecRaw  = 0
	CodecZstd = 1
)

// Limits.
const (
	MaxBody          = 1 << 20 // max message body (type byte + payload)
	MaxNameLen       = 4096
	MaxFramePlain    = MaxBody + 1
	MaxDirFilesLimit = 10000
	// MaxScanDirs bounds the directories one scan may announce; viewers refuse
	// to allocate more, so a DIR's total child count is capped by it too.
	MaxScanDirs   = 2_000_000
	MaxChildDirs  = MaxScanDirs
	InitialCredit = 8 << 20 // implicit initial credit window (ciphertext bytes)
)

// Reasons / codes.
const (
	ByeSuperseded = "superseded"
	ByeShutdown   = "shutdown"
	ByeIdle       = "idle"

	ErrCodeReplayUnavailable = "replay_unavailable"
)

type Cwd struct {
	Path string `json:"path"`
}

type ScanRef struct {
	ID     uint64 `json:"id"`
	State  string `json:"state"`            // running | done | canceled | error
	Paused bool   `json:"paused,omitempty"` // only for viewers that advertised caps.scanState
}

// Features lists the optional protocol extensions an agent supports. A viewer
// sends the matching v2a message only if the feature is advertised.
type Features struct {
	Pause  bool           `json:"pause,omitempty"`
	Delete *DeleteFeature `json:"delete,omitempty"`
}

// DeleteFeature is AGENT_INFO.features.delete: the agent accepts DELETE_REQ
// and DELETE_CANCEL. Every request is approved on the scanned computer.
type DeleteFeature struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`  // no_confirm | unsupported (when !Available)
	Confirm   string `json:"confirm,omitempty"` // terminal | app | console
	Trash     bool   `json:"trash"`             // a Trash / Recycle Bin can be used
	MaxItems  int    `json:"maxItems,omitempty"`
}

// Deleting is AGENT_INFO.deleting: the deletion job that is preparing,
// awaiting approval or running (any viewer's).
type Deleting struct {
	Job   uint64 `json:"job"`
	State string `json:"state"`
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

// ProtoVersion is the application protocol version. Agent and viewer must
// match exactly: the viewer is served by the relay and agents are separate
// binaries, so a mismatch fails with an "update diskspace" message instead of
// misparsing records.
const ProtoVersion = 1

// AgentInfo is the AGENT_INFO payload.
type AgentInfo struct {
	Proto        int       `json:"proto"`
	Version      string    `json:"version"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	Hostname     string    `json:"hostname"`
	User         string    `json:"user"`
	InvokingUser string    `json:"invokingUser"`
	Elevated     bool      `json:"elevated"`
	Elevation    string    `json:"elevation"` // none | sudo | uac | gui-admin | native
	Launcher     string    `json:"launcher"`  // cli | app (where the user sees the verification code)
	FDA          *string   `json:"fda"`       // granted | denied | null
	Root         *string   `json:"root"`
	Cwd          *Cwd      `json:"cwd"`
	Home         string    `json:"home"`
	AutoScan     *string   `json:"autoScan"`
	Volumes      []Volume  `json:"volumes"`
	Scan         *ScanRef  `json:"scan"`
	Features     Features  `json:"features"`
	Deleting     *Deleting `json:"deleting"`
}

// MarshalJSON guarantees "volumes" is an array, never null.
func (a AgentInfo) MarshalJSON() ([]byte, error) {
	type plain AgentInfo
	if a.Volumes == nil {
		a.Volumes = []Volume{}
	}
	return json.Marshal(plain(a))
}

type VolumeUsage struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
	Used  uint64 `json:"used"`
}

type ScanOptions struct {
	CrossMounts  bool   `json:"crossMounts,omitempty"`
	TopFiles     int    `json:"topFiles,omitempty"`
	MinFileBytes uint64 `json:"minFileBytes,omitempty"`
}

type ScanStarted struct {
	ScanID   uint64       `json:"scanId"`
	Path     string       `json:"path"`
	RootName string       `json:"rootName"`
	Volume   *VolumeUsage `json:"volume"`
	Opts     ScanOptions  `json:"opts"`
}

type Progress struct {
	ScanID      uint64 `json:"scanId"`
	Dirs        uint64 `json:"dirs"`
	Files       uint64 `json:"files"`
	Alloc       uint64 `json:"alloc"`
	Apparent    uint64 `json:"apparent"`
	Errors      uint64 `json:"errors"`
	ElapsedMs   int64  `json:"elapsedMs"`
	CurrentPath string `json:"currentPath,omitempty"`
}

type ScanTotals struct {
	Alloc    uint64 `json:"alloc"`
	Apparent uint64 `json:"apparent"`
	Files    uint64 `json:"files"`
	Dirs     uint64 `json:"dirs"`
}

type ScanDone struct {
	ScanID           uint64     `json:"scanId"`
	Totals           ScanTotals `json:"totals"`
	Denied           uint64     `json:"denied"`
	Errors           uint64     `json:"errors"`
	Vanished         uint64     `json:"vanished"`
	SkippedMounts    uint64     `json:"skippedMounts"`
	HardlinksDeduped uint64     `json:"hardlinksDeduped"`
	Folded           uint64     `json:"folded"`
	Ms               int64      `json:"ms"`
}

// ScanState is the SCAN_STATE payload (a2v 0x17): the scan's pause state and
// its elapsed time excluding paused time.
type ScanState struct {
	ScanID    uint64 `json:"scanId"`
	Paused    bool   `json:"paused"`
	ElapsedMs int64  `json:"elapsedMs"`
}

type ScanEnded struct {
	ScanID  uint64 `json:"scanId"`
	Reason  string `json:"reason"` // canceled | error
	Message string `json:"message,omitempty"`
}

type BrowseResult struct {
	ReqID  uint64   `json:"reqId"`
	Path   string   `json:"path"`
	Parent *string  `json:"parent"`
	Dirs   []string `json:"dirs"`
	Error  string   `json:"error,omitempty"`
}

// MarshalJSON guarantees "dirs" is an array, never null.
func (b BrowseResult) MarshalJSON() ([]byte, error) {
	type plain BrowseResult
	if b.Dirs == nil {
		b.Dirs = []string{}
	}
	return json.Marshal(plain(b))
}

type Bye struct {
	Reason string `json:"reason"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ---- viewer -> agent ----

type ClientCaps struct {
	ZstdStream bool `json:"zstdStream"`
	// ScanState: the viewer understands SCAN_STATE (0x17). The agent sends no
	// new a2v message type to a viewer that did not advertise it.
	ScanState bool `json:"scanState,omitempty"`
	// Del: the viewer understands DELETE_STATUS (0x18) and DELETED (0x19).
	Del bool `json:"del,omitempty"`
}

type ClientHello struct {
	Proto int        `json:"proto"`
	UA    string     `json:"ua"`
	Caps  ClientCaps `json:"caps"`
}

type ScanStart struct {
	Path         string `json:"path"`
	CrossMounts  bool   `json:"crossMounts"`
	TopFiles     int    `json:"topFiles,omitempty"`
	MinFileBytes uint64 `json:"minFileBytes,omitempty"`
}

type ScanCancel struct {
	ScanID uint64 `json:"scanId"`
}

// ScanPause is the SCAN_PAUSE payload (v2a 0x89): an idempotent set, not a toggle.
type ScanPause struct {
	ScanID uint64 `json:"scanId"`
	Paused bool   `json:"paused"`
}

// Deletion request limits.
const (
	MaxDeleteItems = 500
)

// Deletion modes and job states.
const (
	DeleteModeTrash     = "trash"
	DeleteModePermanent = "permanent"

	DelPreparing = "preparing"
	DelAwaiting  = "awaiting"
	DelRunning   = "running"
	DelDone      = "done"    // the job ran (results may hold failures)
	DelRefused   = "refused" // not approved (declined, timeout, canceled, limits, scanning)
	DelFailed    = "failed"  // never ran (invalid, busy, nothing deletable)
)

// DeleteItem is one DELETE_REQ item: a scanned directory by id, or a
// non-directory by its parent directory's id and its name.
type DeleteItem struct {
	Dir    *uint64 `json:"dir,omitempty"`
	Parent *uint64 `json:"parent,omitempty"`
	Name   *string `json:"name,omitempty"`
}

// DeleteReq is the DELETE_REQ payload (v2a 0x8A).
type DeleteReq struct {
	ReqID  uint64       `json:"reqId"`
	ScanID uint64       `json:"scanId"`
	Mode   string       `json:"mode"`
	Items  []DeleteItem `json:"items"`
}

// DeleteCancel is the DELETE_CANCEL payload (v2a 0x8B): before approval it
// withdraws the request; once running the job stops after the current item.
type DeleteCancel struct {
	Job uint64 `json:"job"`
}

// DeleteResult is one item's outcome; I indexes DeleteReq.Items.
type DeleteResult struct {
	I          int    `json:"i"`
	OK         bool   `json:"ok"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	FreedAlloc uint64 `json:"freedAlloc"`
	Files      uint64 `json:"files"`
	Dirs       uint64 `json:"dirs"`
}

// DeleteStatus is the DELETE_STATUS payload (a2v 0x18).
type DeleteStatus struct {
	Job        uint64         `json:"job"`
	ReqID      uint64         `json:"reqId"`
	State      string         `json:"state"`
	Where      string         `json:"where,omitempty"`
	Total      int            `json:"total"`
	Done       int            `json:"done"`
	FreedAlloc uint64         `json:"freedAlloc"`
	Current    string         `json:"current,omitempty"`
	Code       string         `json:"code,omitempty"`
	Message    string         `json:"message,omitempty"`
	Results    []DeleteResult `json:"results,omitempty"`
}

// DeletedItem is one tombstone: a directory (Dir set; Partial when only some
// of its contents went) or a non-directory (Parent + Name).
type DeletedItem struct {
	Dir           *uint64 `json:"dir,omitempty"`
	Parent        *uint64 `json:"parent,omitempty"`
	Name          *string `json:"name,omitempty"`
	Partial       bool    `json:"partial,omitempty"`
	FreedAlloc    uint64  `json:"freedAlloc"`
	FreedApparent uint64  `json:"freedApparent"`
	Files         *uint64 `json:"files,omitempty"`
	Dirs          *uint64 `json:"dirs,omitempty"`
}

// Deleted is the DELETED payload (a2v 0x19): tombstones for the scan's tree.
type Deleted struct {
	ScanID uint64        `json:"scanId"`
	Job    uint64        `json:"job"`
	Items  []DeletedItem `json:"items"`
}

type Browse struct {
	ReqID uint64 `json:"reqId"`
	Path  string `json:"path"`
}

type DirFilesReq struct {
	ReqID  uint64 `json:"reqId"`
	ScanID uint64 `json:"scanId"`
	DirID  uint64 `json:"dirId"`
	Limit  int    `json:"limit"`
}

type Credit struct {
	N uint64 `json:"n"`
}

// EncodeMsg builds a message body: [type][JSON payload]. A nil v yields a bare type byte.
func EncodeMsg(typ byte, v any) ([]byte, error) {
	if v == nil {
		return []byte{typ}, nil
	}
	j, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 1, 1+len(j))
	b[0] = typ
	return append(b, j...), nil
}

// DecodeMsg splits a body and unmarshals its JSON payload into v (if v != nil
// and a payload is present). It returns the type byte.
func DecodeMsg(body []byte, v any) (byte, error) {
	if len(body) == 0 {
		return 0, errors.New("proto: empty body")
	}
	if v != nil && len(body) > 1 {
		if err := json.Unmarshal(body[1:], v); err != nil {
			return body[0], err
		}
	}
	return body[0], nil
}
