// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"testing"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// Repeated attach (SCAN_START spam from a viewer that does not read) must
// supersede the unsent SCAN_STARTED rather than grow the control queue, and
// must keep its position after earlier control messages.
func TestAttachSupersedesPendingStarted(t *testing.T) {
	s := newSender(nil, nil, nil)
	s.pushJSON(proto.MsgAgentInfo, proto.AgentInfo{})
	for i := 0; i < 5000; i++ {
		body := []byte{proto.MsgScanStarted, byte(i), byte(i >> 8)}
		s.attach(&scanRun{log: newScanLog(uint64(i), 1<<20), startedBody: body}, i%2 == 0)
	}
	s.mu.Lock()
	n := len(s.ctrl)
	s.mu.Unlock()
	if n > 3 {
		t.Fatalf("ctrl entries=%d after 5000 attaches, want <=3", n)
	}
	s.mu.Lock()
	first, last := s.ctrl[0], s.ctrl[len(s.ctrl)-1]
	s.mu.Unlock()
	if first[0] != proto.MsgAgentInfo {
		t.Errorf("AGENT_INFO no longer first")
	}
	if last[0] != proto.MsgScanStarted && last[0] != proto.MsgReplayDone {
		t.Errorf("unexpected tail %v", last[0])
	}
}

func TestWaitRoomBlocksUntilDrained(t *testing.T) {
	s := newSender(nil, nil, nil)
	for i := 0; i < ctrlSoftEntries; i++ {
		s.push([]byte{proto.MsgPong})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if s.waitRoom(ctx) {
		t.Fatal("waitRoom returned with a full queue")
	}
	s.pick()
	if !s.waitRoom(context.Background()) {
		t.Fatal("waitRoom did not return after room appeared")
	}
}
