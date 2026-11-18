// SPDX-License-Identifier: MIT

//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// A folder the scan could not read is refused (its contents were never seen).
func TestDeleteReadDeniedRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	tree := delTree(t)
	locked := filepath.Join(tree, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	id, ok := ids["locked"]
	if !ok {
		t.Fatal("locked not announced")
	}
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(id)}})
	st := v.expectStatus(proto.DelFailed)
	if st.Code != "nothing_deletable" || len(st.Results) != 1 || st.Results[0].Code != "permission" {
		t.Fatalf("%+v", st)
	}
	os.Chmod(locked, 0o755)
	if !exists(filepath.Join(locked, "inner")) {
		t.Fatal("touched")
	}
}
