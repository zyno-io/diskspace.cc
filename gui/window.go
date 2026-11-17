// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	diskspace "github.com/zyno-io/diskspace.cc"
)

const (
	launcherW = 480
	launcherH = 680
)

// petrol-0 (#0D2A31): the window shows this before the page paints.
var petrol = &options.RGBA{R: 0x0d, G: 0x2a, B: 0x31, A: 0xff}

func runGUI() error {
	a, err := newApp(version, defaultRelay)
	if err != nil {
		return err
	}
	opts := &options.App{
		Title:            "diskspace.cc",
		Width:            launcherW,
		Height:           launcherH,
		MinWidth:         420,
		MinHeight:        600,
		BackgroundColour: petrol,
		AssetServer:      &assetserver.Options{Assets: launcherFS(diskspace.WebFS())},
		OnStartup:        a.startup,
		OnShutdown:       a.shutdown,
		Bind:             []interface{}{a},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "cc.diskspace.app",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) {
				if ctx := a.wailsCtx(); ctx != nil {
					wruntime.WindowUnminimise(ctx)
					wruntime.Show(ctx)
				}
			},
		},
		Mac: &mac.Options{
			// A transparent, title-less bar that keeps content below it: the
			// window reads as one petrol plane, and the embedded viewer's top
			// row never sits under the traffic lights.
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                  true,
			},
			Appearance: mac.NSAppearanceNameDarkAqua,
			About: &mac.AboutInfo{
				Title:   "Diskspace",
				Message: "See what's filling your disk.\nVersion " + version + "\n\nMIT licensed · github.com/zyno-io/diskspace.cc",
			},
		},
		Windows: &windows.Options{
			Theme:                windows.Dark,
			DisablePinchZoom:     true,
			IsZoomControlEnabled: false,
			WebviewUserDataPath:  webviewDataDir(),
		},
	}
	if goruntime.GOOS == "darwin" {
		// Without a menu, Cmd+Q / Cmd+C / Cmd+V do nothing on macOS.
		m := menu.NewMenu()
		m.Append(menu.AppMenu())
		m.Append(menu.EditMenu())
		m.Append(menu.WindowMenu())
		opts.Menu = m
	}
	return wails.Run(opts)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Diskspace:", err)
	os.Exit(1)
}

// wailsCtx returns the Wails runtime context once the window started.
func (a *App) wailsCtx() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctx
}

// webviewDataDir keeps the WebView2 profile in Local AppData under a stable
// name; Wails' default is Roaming AppData named after the .exe file.
func webviewDataDir() string {
	if goruntime.GOOS != "windows" {
		return ""
	}
	base, err := os.UserCacheDir() // %LocalAppData% on Windows
	if err != nil {
		return ""
	}
	return filepath.Join(base, "diskspace.cc", "WebView2")
}
