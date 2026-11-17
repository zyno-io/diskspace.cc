// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	wassets "github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	diskspace "github.com/zyno-io/diskspace.cc"
)

type noRuntime struct{}

func (noRuntime) DesktopIPC() []byte       { return nil }
func (noRuntime) WebsocketIPC() []byte     { return nil }
func (noRuntime) RuntimeDesktopJS() []byte { return nil }

// The window's start page must be the launcher with the Wails runtime
// injected (Wails only injects into "/" and index.html).
func TestStartPageIsLauncherWithRuntime(t *testing.T) {
	srv, err := wassets.NewAssetServer("", assetserver.Options{Assets: launcherFS(diskspace.WebFS())}, false, nil, noRuntime{})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	for _, want := range []string{"/wails/runtime.js", "/wails/ipc.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("start page lacks %s", want)
		}
	}
	js := regexp.MustCompile(`/assets/launcher-[^"]+\.js`).FindString(body)
	if js == "" {
		t.Fatalf("start page is not the launcher:\n%s", body)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, js, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Errorf("GET %s = %d %q", js, rec.Code, rec.Header().Get("Content-Type"))
	}
}
