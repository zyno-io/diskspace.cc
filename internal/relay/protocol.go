// SPDX-License-Identifier: MIT

// Package relay implements the diskspace.cc rendezvous relay: an untrusted
// WebSocket forwarder that pairs an agent (the CLI on a user's machine) with a
// browser viewer and copies opaque, end-to-end encrypted binary messages
// between them. It also serves the built SPA.
//
// # Agent flow
//
// The agent picks sid (16 random bytes, base64url, 22 chars) and a relay token
// T (32 random bytes). The relay stores only sha256(T).
//
//  1. Control channel: dial wss://host/v1/agent?sid=<sid> with header
//     "Authorization: Bearer <base64url T>" (the token is never accepted in the
//     URL). The relay replies with text {"t":"registered","ttl":<seconds left>}.
//     Re-dialing the same sid with the same token within the 60s grace window
//     (or after a relay restart) re-claims the session; a different token is
//     closed with CloseSIDClaimed (4409) and the agent must rotate its link.
//     A missing/malformed token is closed with CloseBadToken (4403).
//  2. Keep reading the control channel (it must be read to answer relay pings,
//     which arrive every 20s). On {"t":"viewer","cid":C} a viewer is waiting.
//  3. Within 10s dial wss://host/v1/agent/data?sid=<sid>&cid=<C> with the same
//     Bearer header. The relay splices that socket to the viewer: binary
//     messages are copied verbatim in both directions. Agent->viewer messages
//     may be up to 1 MiB+4 KiB; viewer->agent up to 64 KiB. Text frames or
//     oversize messages close both sides. Unknown cid/sid is CloseUnknownSID
//     (4404); wrong token is 4403; more than 2 spliced pairs is 4429.
//  4. {"t":"viewer_gone","cid":C} is sent when that viewer disconnects (or
//     never got paired); the agent should drop any state for C. Closing the
//     data socket with a 4xxx code closes the viewer with the same code,
//     otherwise the viewer sees 1011.
//  5. On exit, close the control channel with 1000 to delete the session
//     immediately. Any other drop keeps it for 60s. Sessions end after 24h
//     (CloseTTL, 4408).
//
// The relay does not enforce a single active viewer; the agent decides who is
// active (up to the 2-pair cap) and closes superseded data sockets.
package relay

import "encoding/json"

// Close codes used by the relay.
const (
	CloseBadToken   = 4403 // missing/incorrect agent token
	CloseUnknownSID = 4404 // unknown sid (or cid, or session gone)
	CloseTTL        = 4408 // session absolute TTL reached
	CloseSIDClaimed = 4409 // sid already claimed by a different token
	CloseRateLimit  = 4429 // rate/concurrency limit hit
	CloseTooBig     = 1009 // oversize message
)

// Control message type tags (server -> agent).
const (
	MsgRegistered = "registered"
	MsgViewer     = "viewer"
	MsgViewerGone = "viewer_gone"
)

// ControlMsg is the server->agent control-channel JSON message.
type ControlMsg struct {
	T   string `json:"t"`
	TTL int    `json:"ttl,omitempty"` // registered: seconds until the 24h TTL
	CID string `json:"cid,omitempty"` // viewer, viewer_gone: opaque viewer id
}

// Registered is {"t":"registered","ttl":N}.
func Registered(ttlSeconds int) ControlMsg { return ControlMsg{T: MsgRegistered, TTL: ttlSeconds} }

// Viewer is {"t":"viewer","cid":C}.
func Viewer(cid string) ControlMsg { return ControlMsg{T: MsgViewer, CID: cid} }

// ViewerGone is {"t":"viewer_gone","cid":C}.
func ViewerGone(cid string) ControlMsg { return ControlMsg{T: MsgViewerGone, CID: cid} }

func (m ControlMsg) bytes() []byte {
	b, _ := json.Marshal(m)
	return b
}
