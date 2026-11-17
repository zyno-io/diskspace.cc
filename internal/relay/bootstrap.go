// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// devReleasePath is where --dev-binary-dir content is served. Scripts treat
// {BASE}/devrel/ as the trusted release prefix, tag "local".
const (
	devReleasePath = "/devrel/"
	devReleaseTag  = "local"
)

var (
	// Hostname (or bracketed IPv6) with optional port and path; nothing that
	// could break out of a shell/PowerShell string.
	safeBaseRe = regexp.MustCompile(`^https?://(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(?::[0-9]{1,5})?(?:/[A-Za-z0-9._~-]+)*$`)
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

	// assetNameRe whitelists release asset names reachable via /dl/{file}.
	assetNameRe = regexp.MustCompile(`^(?:` +
		`diskspace_(?:linux|darwin|windows)_(?:amd64|arm64)\.(?:tar\.gz|zip)` +
		`|diskspace-relay_linux_(?:amd64|arm64)\.tar\.gz` +
		`|diskspace_[a-z0-9_]+\.(?:deb|rpm|apk)` +
		`|diskspace-desktop_macos_universal\.(?:dmg|zip)` +
		`|diskspace-desktop_windows_(?:amd64|arm64)\.zip` +
		`|checksums(?:-gui)?\.txt)$`)
)

// BootstrapConfig configures the install/download handlers.
type BootstrapConfig struct {
	BaseURL      string // public base URL, e.g. https://diskspace.cc
	Repo         string // GitHub "owner/name"; default DefaultGitHubRepo
	Dev          bool   // allow http:// base URLs
	DevBinaryDir string // serve local builds instead of GitHub releases
	Resolver     *ReleaseResolver
}

// Bootstrap serves /run, /run.ps1 and /dl/*.
type Bootstrap struct {
	base     string
	repo     string
	prefix   string // trusted release URL prefix the scripts enforce
	devDir   string
	resolver *ReleaseResolver
	sh, ps   string
}

// NewBootstrap validates cfg (so nothing unsafe is interpolated into the
// generated scripts) and renders both scripts.
func NewBootstrap(cfg BootstrapConfig) (*Bootstrap, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if !safeBaseRe.MatchString(base) {
		return nil, fmt.Errorf("unsafe or malformed base URL %q", cfg.BaseURL)
	}
	if strings.HasPrefix(base, "http://") && !cfg.Dev {
		return nil, fmt.Errorf("base URL must be https (got %q)", cfg.BaseURL)
	}
	repo := cfg.Repo
	if repo == "" {
		repo = DefaultGitHubRepo
	}
	if !repoRe.MatchString(repo) {
		return nil, fmt.Errorf("invalid GitHub repo %q", repo)
	}
	b := &Bootstrap{base: base, repo: repo, devDir: cfg.DevBinaryDir, resolver: cfg.Resolver}
	if b.devDir != "" {
		if !cfg.Dev {
			return nil, fmt.Errorf("dev binary dir requires dev mode")
		}
		b.prefix = base + devReleasePath
	} else {
		b.prefix = "https://github.com/" + repo + "/releases/download/"
		if b.resolver == nil {
			b.resolver = NewReleaseResolver(repo)
		}
	}
	b.sh = renderScript(shTemplate, b.base, b.prefix)
	b.ps = renderScript(psTemplate, b.base, b.prefix)
	return b, nil
}

// Routes returns the handlers to mount via Config.Extra.
func (b *Bootstrap) Routes() map[string]http.Handler {
	m := map[string]http.Handler{
		"GET /run":     http.HandlerFunc(b.serveSh),
		"GET /run.ps1": http.HandlerFunc(b.servePS),
		"GET /dl/":     http.HandlerFunc(b.serveDL),
	}
	if b.devDir != "" {
		m["GET "+devReleasePath] = http.HandlerFunc(b.serveDev)
	}
	return m
}

// Start/Stop control background release polling.
func (b *Bootstrap) Start() {
	if b.devDir == "" && b.resolver != nil {
		b.resolver.Start()
	}
}

func (b *Bootstrap) Stop() {
	if b.resolver != nil {
		b.resolver.Stop()
	}
}

func (b *Bootstrap) serveSh(w http.ResponseWriter, _ *http.Request) { serveScript(w, b.sh) }
func (b *Bootstrap) servePS(w http.ResponseWriter, _ *http.Request) { serveScript(w, b.ps) }

func serveScript(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, s)
}

func renderScript(tmpl, base, prefix string) string {
	return strings.NewReplacer(
		"{{BASE}}", base,
		"{{PREFIX}}", prefix,
		"{{PREFIX_RE}}", regexp.QuoteMeta(prefix),
	).Replace(tmpl)
}

// serveDL handles /dl/{os}/{arch}, /dl/gui/mac, /dl/gui/windows[/{arch}] and
// /dl/{filename}. With ?resolve=1 the pinned asset URL is returned as text.
func (b *Bootstrap) serveDL(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")
	var asset string
	switch {
	case len(parts) == 1:
		asset = parts[0]
		if !assetNameRe.MatchString(asset) {
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
	case len(parts) == 2 && parts[0] == "gui" && parts[1] == "mac":
		asset = "diskspace-desktop_macos_universal.dmg"
	case len(parts) == 2 && parts[0] == "gui" && parts[1] == "windows":
		asset = "diskspace-desktop_windows_amd64.zip"
	case len(parts) == 3 && parts[0] == "gui" && parts[1] == "windows" && (parts[2] == "amd64" || parts[2] == "arm64"):
		asset = "diskspace-desktop_windows_" + parts[2] + ".zip"
	case len(parts) == 2:
		osName, arch := parts[0], parts[1]
		if (osName != "linux" && osName != "darwin" && osName != "windows") || (arch != "amd64" && arch != "arm64") {
			http.Error(w, "unsupported platform", http.StatusNotFound)
			return
		}
		ext := ".tar.gz"
		if osName == "windows" {
			ext = ".zip"
		}
		asset = "diskspace_" + osName + "_" + arch + ext
	default:
		http.Error(w, "expected /dl/{os}/{arch} or /dl/{filename}", http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("resolve") == "1" {
		b.serveResolved(w, r, asset)
		return
	}
	b.redirect(w, r, asset)
}

// pinned returns the release URL for asset if it is under the trusted prefix
// with a safe tag. Never a moving "latest" URL.
func (b *Bootstrap) pinned(r *http.Request, asset string) (string, bool) {
	var target string
	if b.devDir != "" {
		if !b.devFileExists(asset) {
			return "", false
		}
		target = b.prefix + devReleaseTag + "/" + asset
	} else {
		t, err := b.resolver.ResolveAssetURL(r.Context(), asset)
		if err != nil {
			return "", false
		}
		target = t
	}
	if !strings.HasPrefix(target, b.prefix) || !strings.HasSuffix(target, "/"+asset) {
		return "", false
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(target, b.prefix), "/"+asset)
	if tag == "" || tag == "." || tag == ".." || strings.ContainsFunc(tag, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-')
	}) {
		return "", false
	}
	return target, true
}

func (b *Bootstrap) serveResolved(w http.ResponseWriter, r *http.Request, asset string) {
	target, ok := b.pinned(r, asset)
	if !ok {
		http.Error(w, "pinned release unavailable; retry later", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, target)
}

func (b *Bootstrap) redirect(w http.ResponseWriter, r *http.Request, asset string) {
	w.Header().Set("Cache-Control", "no-store")
	if b.devDir != "" {
		if !b.devFileExists(asset) {
			http.Error(w, "not available in dev binary dir", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, devReleasePath+devReleaseTag+"/"+asset, http.StatusFound)
		return
	}
	target, err := b.resolver.ResolveAssetURL(r.Context(), asset)
	if err != nil {
		http.Error(w, "failed to resolve asset", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// ---- dev binary dir ----

func (b *Bootstrap) devFileExists(name string) bool {
	if name == "checksums.txt" {
		return true
	}
	if !assetNameRe.MatchString(name) {
		return false
	}
	st, err := os.Stat(filepath.Join(b.devDir, name))
	return err == nil && st.Mode().IsRegular()
}

// serveDev serves /devrel/local/{asset}; checksums.txt is generated from the
// directory contents.
func (b *Bootstrap) serveDev(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, devReleasePath+devReleaseTag+"/")
	if name == "" || strings.ContainsAny(name, "/\\") || !assetNameRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if name == "checksums.txt" {
		entries, err := os.ReadDir(b.devDir)
		if err != nil {
			http.Error(w, "cannot read dev binary dir", http.StatusInternalServerError)
			return
		}
		var names []string
		for _, e := range entries {
			if e.Type().IsRegular() && assetNameRe.MatchString(e.Name()) && !strings.HasPrefix(e.Name(), "checksums") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		var sb strings.Builder
		for _, n := range names {
			sum, err := fileSHA256(filepath.Join(b.devDir, n))
			if err != nil {
				continue
			}
			fmt.Fprintf(&sb, "%s  %s\n", sum, n)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, sb.String())
		return
	}
	if !b.devFileExists(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(b.devDir, name))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// ---- script templates ----

const shTemplate = `#!/bin/sh
# diskspace.cc bootstrap: downloads the CLI for this machine, verifies it, and runs it.
# Usage: curl -fsSL {{BASE}}/run | sh -s -- [diskspace args...]
main() {
    set -e

    DS_SKIP_CHECKSUM=0
    if [ "${1-}" = "--insecure-skip-checksum" ]; then
        DS_SKIP_CHECKSUM=1
        shift
        echo "WARNING: --insecure-skip-checksum disables archive integrity verification; downloaded code will run unchecked." >&2
    fi

    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64) ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
    esac

    case "$OS" in
        linux|darwin) ;;
        *) echo "Unsupported OS: $OS" >&2; exit 1 ;;
    esac

    if [ "$DS_SKIP_CHECKSUM" = 0 ]; then
        if command -v sha256sum >/dev/null 2>&1; then
            hash_file() { sha256sum "$1"; }
        elif command -v shasum >/dev/null 2>&1; then
            hash_file() { shasum -a 256 "$1"; }
        elif command -v openssl >/dev/null 2>&1; then
            hash_file() { openssl dgst -sha256 -r "$1"; }
        else
            echo "Error: sha256sum, shasum, or openssl is required (or explicitly opt out with --insecure-skip-checksum as the first bootstrap argument)" >&2; exit 1
        fi
    fi
    if command -v curl >/dev/null 2>&1; then
        download() { curl -fsSL --connect-timeout 10 --max-time 120 "$1" -o "$2"; }
    elif command -v wget >/dev/null 2>&1; then
        download() { wget -q --timeout=30 --tries=2 -O "$2" "$1"; }
    else
        echo "Error: curl or wget is required" >&2; exit 1
    fi

    DS_TMP=$(mktemp -d)
    trap 'rm -rf "$DS_TMP"' EXIT
    ASSET="diskspace_${OS}_${ARCH}.tar.gz"
    download "{{BASE}}/dl/${OS}/${ARCH}?resolve=1" "$DS_TMP/release-url"
    DOWNLOAD_URL=$(cat "$DS_TMP/release-url")
    PREFIX={{PREFIX}}
    case "$DOWNLOAD_URL" in
        "$PREFIX"*/"$ASSET") ;;
        *) echo "Invalid diskspace release URL" >&2; exit 1 ;;
    esac
    RELEASE_URL=${DOWNLOAD_URL%/*}
    TAG=${RELEASE_URL#"$PREFIX"}
    case "$TAG" in
        ''|.|..|*[!a-zA-Z0-9._-]*) echo "Invalid diskspace release tag" >&2; exit 1 ;;
    esac

    echo "Downloading diskspace..." >&2
    if [ "$DS_SKIP_CHECKSUM" = 0 ]; then
        download "$RELEASE_URL/checksums.txt" "$DS_TMP/checksums.txt"
        EXPECTED=$(awk -v asset="$ASSET" '
            { sub(/\r$/, "") }
            $2 == asset || $2 == "./" asset || $2 == "*" asset || $2 == "*./" asset {
                count++; digest = $1
                if (NF != 2 || length(digest) != 64 || digest ~ /[^0-9a-fA-F]/) bad = 1
            }
            END { if (count != 1 || bad) exit 1; print tolower(digest) }
        ' "$DS_TMP/checksums.txt") || { echo "Missing or invalid diskspace checksum" >&2; exit 1; }
    fi
    download "$DOWNLOAD_URL" "$DS_TMP/diskspace.tar.gz"
    if [ "$DS_SKIP_CHECKSUM" = 0 ]; then
        HASH_OUTPUT=$(hash_file "$DS_TMP/diskspace.tar.gz")
        ACTUAL=${HASH_OUTPUT%% *}
        if [ "$ACTUAL" != "$EXPECTED" ]; then
            echo "diskspace checksum verification failed; refusing to extract or execute" >&2; exit 1
        fi
    fi
    tar xz -C "$DS_TMP" -f "$DS_TMP/diskspace.tar.gz"
    chmod +x "$DS_TMP/diskspace"

    # Not exec: the EXIT trap must clean up afterwards. stdin stays the pipe;
    # the CLI reads prompts from the terminal itself.
    "$DS_TMP/diskspace" "$@"
}
main "$@"
`

const psTemplate = `# diskspace.cc bootstrap: downloads the CLI for this machine, verifies it, and runs it.
# Usage: irm {{BASE}}/run.ps1 | iex
#   with arguments: & ([scriptblock]::Create((irm {{BASE}}/run.ps1))) C:\Users\me
& {
$ErrorActionPreference = 'Stop'

$skipChecksum = $false
$dsArgs = @($args)
if ($dsArgs.Count -gt 0 -and $dsArgs[0] -ceq '--insecure-skip-checksum') {
    $skipChecksum = $true
    if ($dsArgs.Count -eq 1) { $dsArgs = @() }
    else { $dsArgs = @($dsArgs[1..($dsArgs.Count - 1)]) }
    [Console]::Error.WriteLine('WARNING: --insecure-skip-checksum disables archive integrity verification; downloaded code will run unchecked.')
}

if ($IsWindows -or $env:OS -eq 'Windows_NT') { $os = 'windows' }
elseif ($IsMacOS) { $os = 'darwin' }
elseif ($IsLinux) { $os = 'linux' }
else { throw 'Unsupported OS' }

try { $arch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLower() }
catch { $arch = $env:PROCESSOR_ARCHITECTURE.ToLower() }
switch ($arch) {
    'x64'   { $arch = 'amd64' }
    'amd64' { $arch = 'amd64' }
    'arm64' { $arch = 'arm64' }
    default { throw "Unsupported architecture: $arch" }
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) "diskspace-$([guid]::NewGuid())"
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $ext = if ($os -eq 'windows') { '.zip' } else { '.tar.gz' }
    $asset = "diskspace_${os}_${arch}$ext"
    $archive = Join-Path $tmp "diskspace$ext"
    $releaseResponse = Invoke-WebRequest -UseBasicParsing -Uri "{{BASE}}/dl/$os/${arch}?resolve=1" -TimeoutSec 120
    $downloadURL = ([string]$releaseResponse.Content).Trim()
    $pattern = '\A{{PREFIX_RE}}([a-zA-Z0-9._-]+)/' + [regex]::Escape($asset) + '\z'
    $releaseMatch = [regex]::Match($downloadURL, $pattern)
    if (-not $releaseMatch.Success -or $releaseMatch.Groups[1].Value -in @('.', '..')) { throw 'Invalid diskspace release URL' }
    $releaseURL = $downloadURL.Substring(0, $downloadURL.LastIndexOf('/'))
    $checksums = Join-Path $tmp 'checksums.txt'
    Write-Host 'Downloading diskspace...' -ForegroundColor DarkGray
    if (-not $skipChecksum) {
        Invoke-WebRequest -UseBasicParsing -Uri "$releaseURL/checksums.txt" -OutFile $checksums -TimeoutSec 120
        $entries = @(foreach ($line in Get-Content -LiteralPath $checksums) {
            $fields = $line.Trim() -split '\s+'
            if ($fields.Length -ge 2 -and $fields[1] -cin @($asset, "./$asset", "*$asset", "*./$asset")) {
                if ($fields.Length -ne 2 -or $fields[0] -notmatch '\A[0-9a-fA-F]{64}\z') { throw 'Invalid diskspace checksum' }
                $fields[0]
            }
        })
        if ($entries.Count -ne 1) { throw 'Missing or duplicate diskspace checksum' }
    }
    Invoke-WebRequest -UseBasicParsing -Uri $downloadURL -OutFile $archive -TimeoutSec 120
    if (-not $skipChecksum) {
        $actual = Get-FileHash -LiteralPath $archive -Algorithm SHA256
        if ($actual.Hash -ine $entries[0]) { throw 'diskspace checksum verification failed; refusing to extract or execute.' }
    }
    if ($os -eq 'windows') {
        Expand-Archive -Path $archive -DestinationPath $tmp
    } else {
        tar xzf $archive -C $tmp
        if ($LASTEXITCODE -ne 0) { throw 'diskspace archive extraction failed' }
    }
    $binExt = if ($os -eq 'windows') { '.exe' } else { '' }
    $bin = Join-Path $tmp "diskspace$binExt"
    if ($os -ne 'windows') {
        chmod +x $bin
        if ($LASTEXITCODE -ne 0) { throw 'Cannot make diskspace executable' }
    }
    & $bin @dsArgs
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
} @args
`
