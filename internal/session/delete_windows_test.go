// SPDX-License-Identifier: MIT

//go:build windows

package session

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// A junction is announced with CHILD_REPARSE and never scanned: refused.
func TestDeleteJunctionRefused(t *testing.T) {
	tree := delTree(t)
	link := filepath.Join(tree, "jn")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, filepath.Join(tree, "other")).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	id, ok := ids["jn"]
	if !ok {
		t.Fatal("junction not announced")
	}
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(id)}})
	st := v.expectStatus(proto.DelFailed)
	if st.Code != "nothing_deletable" || len(st.Results) != 1 || st.Results[0].Code != "mount" {
		t.Fatalf("%+v", st)
	}
	if !exists(link) || !exists(filepath.Join(tree, "other", "x.txt")) {
		t.Fatal("touched")
	}
}
