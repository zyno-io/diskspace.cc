// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const ghPrefix = "https://github.com/zyno-io/diskspace.cc/releases/download/"

func mustBootstrap(t *testing.T, cfg BootstrapConfig) *Bootstrap {
	t.Helper()
	b, err := NewBootstrap(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func prodBootstrap(t *testing.T) *Bootstrap {
	return mustBootstrap(t, BootstrapConfig{BaseURL: "https://diskspace.cc"})
}

func writeFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func fakeArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\nprintf 'CLI_EXECUTED\\n'\nprintf 'CLI_ARG:<%s>\\n' \"$@\"\n[ \"$1\" != exit7 ] || exit 7\n")
	if err := tw.WriteHeader(&tar.Header{Name: "diskspace", Mode: 0o700, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// ---- validation & rendering ----

func TestNewBootstrapValidation(t *testing.T) {
	bad := []BootstrapConfig{
		{BaseURL: "https://diskspace.cc\"; rm -rf ~; echo \""},
		{BaseURL: "https://diskspace.cc/$(id)"},
		{BaseURL: "https://dis`id`.cc"},
		{BaseURL: "https://disk space.cc"},
		{BaseURL: "https://a.cc'x"},
		{BaseURL: "https://a.cc\\x"},
		{BaseURL: "https://a.cc;x"},
		{BaseURL: "https://a.cc|x"},
		{BaseURL: "https://a.cc/?q=1"},
		{BaseURL: "https://a.cc/#f"},
		{BaseURL: "https://user:pw@a.cc"},
		{BaseURL: "https://a.cc\nx"},
		{BaseURL: "ftp://a.cc"},
		{BaseURL: ""},
		{BaseURL: "http://diskspace.cc"}, // http requires Dev
		{BaseURL: "https://diskspace.cc", Repo: "a/b'c"},
		{BaseURL: "https://diskspace.cc", Repo: "no-slash"},
		{BaseURL: "https://diskspace.cc", DevBinaryDir: "/tmp"}, // dev dir requires Dev
	}
	for _, c := range bad {
		if _, err := NewBootstrap(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	good := []BootstrapConfig{
		{BaseURL: "https://diskspace.cc"},
		{BaseURL: "https://diskspace.cc/"},
		{BaseURL: "http://127.0.0.1:8080", Dev: true},
		{BaseURL: "http://localhost:8080", Dev: true},
		{BaseURL: "http://[::1]:8080", Dev: true},
		{BaseURL: "https://example.com/sub/path"},
	}
	for _, c := range good {
		if _, err := NewBootstrap(c); err != nil {
			t.Errorf("rejected %+v: %v", c, err)
		}
	}
}

func TestScriptRendering(t *testing.T) {
	b := prodBootstrap(t)
	for name, s := range map[string]string{"sh": b.sh, "ps": b.ps} {
		if strings.Contains(s, "{{") {
			t.Errorf("%s: unrendered placeholder", name)
		}
		if !strings.Contains(s, "https://diskspace.cc/dl/") {
			t.Errorf("%s: missing base URL", name)
		}
	}
	if !strings.HasPrefix(b.sh, "#!/bin/sh\n") ||
		!strings.Contains(b.sh, "PREFIX="+ghPrefix+"\n") ||
		!strings.Contains(b.sh, `"$DS_TMP/diskspace" "$@"`) ||
		strings.Contains(b.sh, "exec ") ||
		!strings.Contains(b.sh, "trap 'rm -rf") {
		t.Error("sh script missing required pieces")
	}
	for _, want := range []string{
		`https://github\.com/zyno-io/diskspace\.cc/releases/download/`,
		"Expand-Archive", "Get-FileHash", "& $bin @dsArgs", "} finally {", "} @args",
	} {
		if !strings.Contains(b.ps, want) {
			t.Errorf("ps script missing %q", want)
		}
	}
	// Dev: the trusted prefix is templated from server config.
	d := mustBootstrap(t, BootstrapConfig{BaseURL: "http://127.0.0.1:9", Dev: true, DevBinaryDir: t.TempDir()})
	if !strings.Contains(d.sh, "PREFIX=http://127.0.0.1:9/devrel/\n") || strings.Contains(d.sh, "github.com") {
		t.Error("dev sh prefix wrong")
	}
	if !strings.Contains(d.ps, `http://127\.0\.0\.1:9/devrel/`) {
		t.Error("dev ps prefix wrong")
	}
}

func TestRenderedScriptShellcheck(t *testing.T) {
	if os.Getenv("SHELLCHECK") != "1" {
		t.Skip("set SHELLCHECK=1 to run")
	}
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	f := filepath.Join(t.TempDir(), "run.sh")
	writeFixture(t, f, []byte(prodBootstrap(t).sh))
	if out, err := exec.Command(sc, "-s", "sh", f).CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}

// ---- routes ----

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func routedMux(b *Bootstrap) http.Handler {
	mux := http.NewServeMux()
	for p, h := range b.Routes() {
		mux.Handle(p, h)
	}
	return mux
}

func TestScriptRoutes(t *testing.T) {
	h := routedMux(prodBootstrap(t))
	if rec := get(h, "/run"); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "#!/bin/sh") || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("/run: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get(h, "/run.ps1"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Get-FileHash") {
		t.Fatalf("/run.ps1: %d", rec.Code)
	}
}

func TestDLRoutes(t *testing.T) {
	b := prodBootstrap(t)
	b.resolver = NewReleaseResolver(DefaultGitHubRepo)
	b.resolver.fetchedAt = time.Now()
	b.resolver.cache = map[string]resolvedAsset{}
	for _, n := range []string{
		"diskspace_linux_amd64.tar.gz", "diskspace_windows_arm64.zip", "diskspace_amd64.deb",
		"Diskspace_macos_universal.dmg", "Diskspace_windows_amd64.zip", "Diskspace_windows_arm64.zip", "checksums.txt",
	} {
		b.resolver.cache[n] = resolvedAsset{url: ghPrefix + "v1.2.3/" + n}
	}
	h := routedMux(b)
	redirects := map[string]string{
		"/dl/linux/amd64":                   "diskspace_linux_amd64.tar.gz",
		"/dl/windows/arm64":                 "diskspace_windows_arm64.zip",
		"/dl/gui/mac":                       "Diskspace_macos_universal.dmg",
		"/dl/gui/windows":                   "Diskspace_windows_amd64.zip",
		"/dl/gui/windows/amd64":             "Diskspace_windows_amd64.zip",
		"/dl/gui/windows/arm64":             "Diskspace_windows_arm64.zip",
		"/dl/diskspace_amd64.deb":           "diskspace_amd64.deb",
		"/dl/checksums.txt":                 "checksums.txt",
		"/dl/Diskspace_macos_universal.dmg": "Diskspace_macos_universal.dmg",
	}
	for path, asset := range redirects {
		rec := get(h, path)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != ghPrefix+"v1.2.3/"+asset {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
		rec = get(h, path+"?resolve=1")
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != ghPrefix+"v1.2.3/"+asset || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s resolve: %d %q", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{
		"/dl/plan9/amd64", "/dl/linux/386", "/dl/gui/linux", "/dl/gui/windows/386", "/dl/evil.sh",
		"/dl/diskspace_linux_amd64.tar.gz.sig", "/dl/%2e%2e/etc", "/dl/a/b/c/d", "/dl/", "/dl/sp2p_linux_amd64.tar.gz",
	} {
		if rec := get(h, path); rec.Code < 400 {
			t.Errorf("%s: unexpectedly %d", path, rec.Code)
		}
	}
	// Unlisted asset: redirect falls back to latest, but resolve refuses it.
	if rec := get(h, "/dl/darwin/arm64"); rec.Code != 302 || !strings.Contains(rec.Header().Get("Location"), "/releases/latest/download/") {
		t.Errorf("fallback redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get(h, "/dl/darwin/arm64?resolve=1"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("resolve of moving latest accepted: %d", rec.Code)
	}
}

func TestResolveRejectsUnsafeTargets(t *testing.T) {
	const asset = "diskspace_linux_amd64.tar.gz"
	for _, target := range []string{
		ghPrefix + "v0.4.0/" + asset, ghPrefix + "v0.1.1-rc.1/" + asset,
		"https://github.com/zyno-io/diskspace.cc/releases/latest/download/" + asset,
		"https://example.invalid/" + asset,
		"https://github.com/other/repo/releases/download/v1/" + asset,
		ghPrefix + "../" + asset, ghPrefix + "v1/other/" + asset, ghPrefix + "%2e%2e/" + asset, ghPrefix + "/" + asset,
	} {
		b := prodBootstrap(t)
		b.resolver = NewReleaseResolver(DefaultGitHubRepo)
		b.resolver.cache = map[string]resolvedAsset{asset: {url: target}}
		b.resolver.fetchedAt = time.Now()
		rec := get(routedMux(b), "/dl/linux/amd64?resolve=1")
		wantOK := target == ghPrefix+"v0.4.0/"+asset || target == ghPrefix+"v0.1.1-rc.1/"+asset
		if wantOK && (rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != target) {
			t.Errorf("%s: rejected: %d", target, rec.Code)
		}
		if !wantOK && rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: accepted: %d", target, rec.Code)
		}
	}
}

// ---- shell checksum behavior (against a fake downloader) ----

type bootstrapCase struct {
	skipChecksum        bool
	hashFailure         bool
	name, manifest, url string
	tamper, wantOK      bool
}

func bootstrapCases(asset, digest string) []bootstrapCase {
	url := ghPrefix + "v0.4.0/" + asset
	valid := digest + "  " + asset + "\n"
	return []bootstrapCase{
		{name: "valid", manifest: valid, url: url, wantOK: true},
		{name: "explicit-bypass-no-manifest", url: url, skipChecksum: true, wantOK: true},
		{name: "explicit-bypass-hash-failure", manifest: valid, url: url, skipChecksum: true, hashFailure: true, wantOK: true},
		{name: "explicit-bypass-wrong-host", url: strings.Replace(url, "github.com", "example.invalid", 1), skipChecksum: true},
		{name: "explicit-bypass-wrong-platform", url: strings.Replace(url, asset, "diskspace_wrong_platform.zip", 1), skipChecksum: true},
		{name: "crlf", manifest: strings.ReplaceAll(valid, "\n", "\r\n"), url: url, wantOK: true},
		{name: "hash-command-failure", manifest: valid, url: url, hashFailure: true},
		{name: "dot-prefix", manifest: strings.ToUpper(digest) + "  ./" + asset + "\n", url: url, wantOK: true},
		{name: "binary-marker", manifest: digest + " *./" + asset + "\n", url: url, wantOK: true},
		{name: "tampered", manifest: valid, url: url, tamper: true},
		{name: "missing-entry", manifest: digest + "  diskspace_wrong_platform.zip\n", url: url},
		{name: "missing-manifest", url: url},
		{name: "duplicate-entry", manifest: valid + valid, url: url},
		{name: "duplicate-alias", manifest: valid + digest + "  ./" + asset + "\n", url: url},
		{name: "malformed-digest", manifest: strings.Repeat("g", 64) + "  " + asset + "\n", url: url},
		{name: "short-digest", manifest: "abcd  " + asset + "\n", url: url},
		{name: "extra-field", manifest: strings.TrimSpace(valid) + " extra\n", url: url},
		{name: "wrong-host", manifest: valid, url: strings.Replace(url, "github.com", "example.invalid", 1)},
		{name: "wrong-repo", manifest: valid, url: strings.Replace(url, "zyno-io/diskspace.cc", "evil/repo", 1)},
		{name: "wrong-platform", manifest: valid, url: strings.Replace(url, asset, "diskspace_wrong_platform.zip", 1)},
		{name: "moving-latest", manifest: valid, url: "https://github.com/zyno-io/diskspace.cc/releases/latest/download/" + asset},
		{name: "traversal", manifest: valid, url: strings.Replace(url, "v0.4.0", "../other", 1)},
		{name: "dot-tag", manifest: valid, url: strings.Replace(url, "v0.4.0", "..", 1)},
		{name: "encoded-tag", manifest: valid, url: strings.Replace(url, "v0.4.0", "%2e%2e", 1)},
	}
}

func TestShellBootstrapChecksums(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell integration")
	}
	script := prodBootstrap(t).sh
	archive := fakeArchive(t)
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	asset := fmt.Sprintf("diskspace_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	for _, downloader := range []string{"curl", "wget"} {
		for _, hasher := range []string{"sha256sum", "shasum", "openssl"} {
			hashPath, err := exec.LookPath(hasher)
			if err != nil {
				continue
			}
			for _, tc := range bootstrapCases(asset, digest) {
				t.Run(downloader+"/"+hasher+"/"+tc.name, func(t *testing.T) {
					dir := t.TempDir()
					bin := filepath.Join(dir, "bin")
					if err := os.Mkdir(bin, 0o700); err != nil {
						t.Fatal(err)
					}
					for _, name := range []string{"uname", "tr", "mktemp", "rm", "cat", "awk", "tar", "gzip", "chmod", "cp"} {
						path, err := exec.LookPath(name)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
							t.Fatal(err)
						}
					}
					if !tc.skipChecksum || tc.hashFailure {
						if err := os.Symlink(hashPath, filepath.Join(bin, hasher)); err != nil {
							t.Fatal(err)
						}
					}
					if tc.hashFailure {
						_ = os.Remove(filepath.Join(bin, hasher))
						writeFixture(t, filepath.Join(bin, hasher), []byte("#!/bin/sh\necho '"+digest+"  archive'\nexit 1\n"))
					}
					writeFixture(t, filepath.Join(bin, downloader), []byte(`#!/bin/sh
set -e
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o|-O) out=$2; shift 2 ;;
        https://*) url=$1; shift ;;
        *) shift ;;
    esac
done
case "$url" in
    *'?resolve=1') cp "$FIXTURE/release-url" "$out" ;;
    */checksums.txt)
        [ "$EXPECT_SKIP_CHECKSUM" != 1 ] || { echo UNEXPECTED_CHECKSUM_DOWNLOAD >&2; exit 44; }
        [ "$url" = 'https://github.com/zyno-io/diskspace.cc/releases/download/v0.4.0/checksums.txt' ] || exit 43
        cp "$FIXTURE/checksums.txt" "$out" ;;
    *.tar.gz) cp "$FIXTURE/archive" "$out" ;;
    *) exit 42 ;;
esac
`))
					writeFixture(t, filepath.Join(dir, "release-url"), []byte(tc.url+"\n"))
					if tc.manifest != "" {
						writeFixture(t, filepath.Join(dir, "checksums.txt"), []byte(tc.manifest))
					}
					data := archive
					if tc.tamper {
						data = append(bytes.Clone(data), 'x')
					}
					writeFixture(t, filepath.Join(dir, "archive"), data)
					if !tc.wantOK {
						_ = os.Remove(filepath.Join(bin, "tar"))
						writeFixture(t, filepath.Join(bin, "tar"), []byte("#!/bin/sh\necho EXTRACTED_UNVERIFIED\nexit 99\n"))
					}
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()
					args := []string{"-s", "--"}
					skipExpected := "0"
					if tc.skipChecksum {
						args = append(args, "--insecure-skip-checksum")
						skipExpected = "1"
					}
					args = append(args, "argument with spaces", "-format", "json")
					cmd := exec.CommandContext(ctx, "/bin/sh", args...)
					cmd.Env = append(os.Environ(), "PATH="+bin, "FIXTURE="+dir, "EXPECT_SKIP_CHECKSUM="+skipExpected)
					cmd.Stdin = strings.NewReader(script)
					var stderr bytes.Buffer
					cmd.Stderr = &stderr
					stdout, err := cmd.Output()
					output := string(stdout) + stderr.String()
					if strings.Contains(stderr.String(), "WARNING: --insecure-skip-checksum") != tc.skipChecksum || strings.Contains(string(stdout), "WARNING:") {
						t.Fatalf("incorrect bypass warning: %s", output)
					}
					if tc.wantOK {
						wantArgs := "CLI_ARG:<argument with spaces>\nCLI_ARG:<-format>\nCLI_ARG:<json>\n"
						if err != nil || !strings.Contains(output, "CLI_EXECUTED") || !strings.Contains(string(stdout), wantArgs) || strings.Contains(string(stdout), "--insecure-skip-checksum") {
							t.Fatalf("bootstrap failed: %v %s", err, output)
						}
					} else if err == nil || strings.Contains(output, "EXTRACTED_UNVERIFIED") || strings.Contains(output, "CLI_EXECUTED") {
						t.Fatalf("unsafe bootstrap: %v %s", err, output)
					}
				})
			}
		}
	}
}

func TestShellBootstrapMissingHashTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell integration")
	}
	dir := t.TempDir()
	for _, name := range []string{"uname", "tr"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	script := prodBootstrap(t).sh
	for _, args := range [][]string{nil, {"file", "--insecure-skip-checksum"}, {"--insecure-skip-checksum=true"}} {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{"-s", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+dir)
		cmd.Stdin = strings.NewReader(script)
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "sha256sum, shasum, or openssl is required") || strings.Contains(string(output), "WARNING:") {
			t.Fatalf("did not fail closed: %v %s", err, output)
		}
	}
}

// ---- end-to-end against the real handlers in dev mode ----

func TestShellBootstrapEndToEndDev(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell integration")
	}
	for _, tool := range []string{"curl", "tar", "awk", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	devDir := t.TempDir()
	archive := fakeArchive(t)
	asset := fmt.Sprintf("diskspace_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	writeFixture(t, filepath.Join(devDir, asset), archive)

	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	defer srv.Close()
	b := mustBootstrap(t, BootstrapConfig{BaseURL: srv.URL, Dev: true, DevBinaryDir: devDir})
	srv.Config.Handler = routedMux(b)

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", append([]string{"-s", "--"}, args...)...)
		cmd.Stdin = strings.NewReader(get(srv.Config.Handler, "/run").Body.String())
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("a b", "exit0")
	if err != nil || !strings.Contains(out, "CLI_ARG:<a b>") {
		t.Fatalf("%v %s", err, out)
	}
	// The binary's exit status propagates.
	if _, err := run("exit7"); err == nil {
		t.Fatal("exit status swallowed")
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 7 {
		t.Fatalf("unexpected: %v", err)
	}
}

// ---- PowerShell ----

func psRun(t *testing.T, pwsh, platform, prelude, script, suffix, fixture, skipExpected string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", prelude+strings.TrimRight(script, "\n")+suffix)
	osValue := ""
	if platform == "windows" {
		osValue = "Windows_NT"
	}
	cmd.Env = append(os.Environ(), "OS="+osValue, "FIXTURE="+fixture, "EXPECT_SKIP_CHECKSUM="+skipExpected)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	return string(stdout), stderr.String(), err
}

var psCases = map[string]bool{
	"valid": true, "explicit-bypass-no-manifest": true, "explicit-bypass-wrong-host": true, "crlf": true,
	"hash-command-failure": true, "tampered": true, "missing-manifest": true, "duplicate-entry": true,
	"wrong-host": true, "wrong-repo": true, "moving-latest": true, "traversal": true,
}

func TestPowerShellBootstrapChecksums(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	script := prodBootstrap(t).ps
	archive := fakeArchive(t)
	digest := fmt.Sprintf("%x", sha256.Sum256(archive))
	platforms := []string{"windows"}
	if runtime.GOOS != "windows" {
		platforms = append(platforms, runtime.GOOS)
	}
	for _, platform := range platforms {
		ext := ".tar.gz"
		if platform == "windows" {
			ext = ".zip"
		}
		asset := fmt.Sprintf("diskspace_%s_%s%s", platform, runtime.GOARCH, ext)
		for _, tc := range bootstrapCases(asset, digest) {
			// pwsh startup is slow; the sh tests cover every case, so only
			// a representative subset runs here.
			if !psCases[tc.name] {
				continue
			}
			t.Run(platform+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				writeFixture(t, filepath.Join(dir, "release-url"), []byte(tc.url+"\n"))
				if tc.manifest != "" {
					writeFixture(t, filepath.Join(dir, "checksums.txt"), []byte(tc.manifest))
				}
				data := archive
				if tc.tamper {
					data = append(bytes.Clone(data), 'x')
				}
				writeFixture(t, filepath.Join(dir, "archive"), data)
				prelude := `function Invoke-WebRequest {
    param($Uri, $OutFile, $TimeoutSec, [switch]$UseBasicParsing)
    if ($Uri.EndsWith('?resolve=1')) { return @{ Content = [IO.File]::ReadAllText((Join-Path $env:FIXTURE 'release-url')) } }
    $source = if ($Uri.EndsWith('/checksums.txt')) {
        if ($env:EXPECT_SKIP_CHECKSUM -eq '1') { throw 'UNEXPECTED_CHECKSUM_DOWNLOAD' }
        if ($Uri -cne 'https://github.com/zyno-io/diskspace.cc/releases/download/v0.4.0/checksums.txt') { throw 'Checksum release drifted' }
        'checksums.txt'
    } else { 'archive' }
    Copy-Item -LiteralPath (Join-Path $env:FIXTURE $source) -Destination $OutFile
}
function tar { throw 'VERIFIED_EXTRACTION' }
function Expand-Archive { throw 'VERIFIED_EXTRACTION' }
`
				if tc.hashFailure {
					prelude += "function Get-FileHash { throw 'hash-command-failure' }\n"
				}
				suffix, skipExpected := "", "0"
				if tc.skipChecksum {
					suffix = " '--insecure-skip-checksum' 'argument with spaces'"
					skipExpected = "1"
				}
				stdout, stderr, err := psRun(t, pwsh, platform, prelude, script, suffix, dir, skipExpected)
				output := stdout + stderr
				if strings.Contains(stderr, "WARNING: --insecure-skip-checksum") != tc.skipChecksum {
					t.Fatalf("incorrect bypass warning: %s", output)
				}
				extracted := strings.Contains(output, "VERIFIED_EXTRACTION")
				if err == nil || extracted != tc.wantOK {
					t.Fatalf("unsafe or failed PowerShell bootstrap: %v %s", err, output)
				}
			})
		}
	}
}

func TestPowerShellRunsBinaryWithArgsAndCleansUp(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	asset := "diskspace_windows_" + map[string]string{"arm64": "arm64"}[runtime.GOARCH]
	if asset == "diskspace_windows_" {
		asset += "amd64"
	}
	asset += ".zip"
	prelude := `function Invoke-WebRequest {
    param($Uri, $OutFile, $TimeoutSec, [switch]$UseBasicParsing)
    if ($Uri.EndsWith('?resolve=1')) { return @{ Content = $env:RELEASE_URL } }
}
function Expand-Archive {}
function Join-Path {
    param($Path, $ChildPath)
    if ($ChildPath -eq 'diskspace.exe') { return 'Invoke-DSFixture' }
    Microsoft.PowerShell.Management\Join-Path $Path $ChildPath
}
function Invoke-DSFixture { foreach ($v in $args) { [Console]::Out.WriteLine("CLI_ARG:<$v>") } }
`
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	script := prodBootstrap(t).ps
	cmd := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command",
		prelude+strings.TrimRight(script, "\n")+" '--insecure-skip-checksum' 'argument with spaces' '-format' ''")
	cmd.Env = append(os.Environ(), "OS=Windows_NT", "RELEASE_URL="+ghPrefix+"v0.4.0/"+asset)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	stdout := strings.ReplaceAll(string(out), "\r\n", "\n")
	if err != nil || !strings.Contains(stdout, "CLI_ARG:<argument with spaces>\nCLI_ARG:<-format>\nCLI_ARG:<>\n") || strings.Contains(stdout, "--insecure-skip-checksum") {
		t.Fatalf("%v\n%s\n%s", err, stdout, stderr.String())
	}
}

func TestPowerShellRejectsForeignReleaseURL(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	script := prodBootstrap(t).ps
	prelude := `function Invoke-WebRequest {
    param($Uri, $OutFile, $TimeoutSec, [switch]$UseBasicParsing)
    if ($Uri.EndsWith('?resolve=1')) { return @{ Content = 'https://evil.example/zyno-io/diskspace.cc/releases/download/v1/diskspace_windows_amd64.zip' } }
    throw 'DOWNLOADED_FOREIGN'
}
`
	stdout, stderr, err := psRun(t, pwsh, "windows", prelude, script, " '--insecure-skip-checksum'", t.TempDir(), "1")
	if err == nil || strings.Contains(stdout+stderr, "DOWNLOADED_FOREIGN") || !strings.Contains(stdout+stderr, "Invalid diskspace release URL") {
		t.Fatalf("accepted foreign URL: %v %s %s", err, stdout, stderr)
	}
}

// ---- dev binary dir ----

func TestDevBinaryDir(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "diskspace_linux_amd64.tar.gz"), []byte("archive-bytes"))
	writeFixture(t, filepath.Join(dir, "secret.txt"), []byte("nope"))
	b := mustBootstrap(t, BootstrapConfig{BaseURL: "http://127.0.0.1:1234", Dev: true, DevBinaryDir: dir})
	h := routedMux(b)

	rec := get(h, "/dl/linux/amd64?resolve=1")
	want := "http://127.0.0.1:1234/devrel/local/diskspace_linux_amd64.tar.gz"
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("resolve: %d %q", rec.Code, rec.Body.String())
	}
	if rec := get(h, "/dl/linux/arm64?resolve=1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing asset resolved: %d", rec.Code)
	}
	if rec := get(h, "/devrel/local/diskspace_linux_amd64.tar.gz"); rec.Code != 200 || rec.Body.String() != "archive-bytes" {
		t.Fatalf("serve: %d", rec.Code)
	}
	rec = get(h, "/devrel/local/checksums.txt")
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("archive-bytes")))
	if rec.Code != 200 || rec.Body.String() != sum+"  diskspace_linux_amd64.tar.gz\n" {
		t.Fatalf("checksums: %d %q", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"/devrel/local/secret.txt", "/devrel/local/../secret.txt", "/devrel/other/diskspace_linux_amd64.tar.gz"} {
		if rec := get(h, p); rec.Code == 200 {
			t.Errorf("%s served", p)
		}
	}
	if rec := get(h, "/dl/linux/amd64"); rec.Code != 302 || rec.Header().Get("Location") != "/devrel/local/diskspace_linux_amd64.tar.gz" {
		t.Fatalf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
