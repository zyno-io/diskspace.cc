// SPDX-License-Identifier: MIT
//
// MockSession: a faithful, offline implementation of Session used by /?demo
// and by the landing-page hero. It generates a plausible macOS home/volume
// tree lazily (parents always announce child ids before the child's own DIR
// record), streams it as NodeBatch messages over ~10 s, sends PROGRESS at 4 Hz
// and SCAN_DONE with denied counts, and supports cancel, browse and dirFiles.
// Deletion requests run the whole flow (preparing, awaiting approval "on the
// host", running, DELETED + done); items under ~/Library/Containers fail with
// "denied" so the partial-result dialog can be seen too.

import type { AgentInfo, BrowseResult, ChildRec, ConnState, DeletedItem, DeleteReq, DeleteResult, DeleteStatus, DirFiles, DirRec, FileRec, NodeRecord, ScanOptions, Session, SessionEvents, Volume } from "./types";
import { CHILD_FLAG, DIR_FLAG, PROTO_VERSION } from "./types";

export interface MockOptions {
  /** approximate number of dirs in a full scan */
  targetDirs?: number;
  /** stream duration for a full scan */
  durationMs?: number;
  /** faster connect sequence (landing hero) */
  quick?: boolean;
  seed?: number;
  /** force an end state for previews: superseded | key-mismatch | no-session | closed */
  demoState?: string | null;
  /** scan this path automatically once connected (agent autoScan) */
  autoScan?: string | null;
  /** ms until a delete request is "approved on the host"; "no" declines it (default 3500) */
  approve?: number | "no";
}

// ---- deterministic rng --------------------------------------------------------
class Rng {
  constructor(private s: number) {}
  next(): number {
    // mulberry32
    let t = (this.s = (this.s + 0x6d2b79f5) | 0);
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  }
  int(lo: number, hi: number): number {
    return lo + Math.floor(this.next() * (hi - lo + 1));
  }
  /** log-uniform integer */
  logInt(lo: number, hi: number): number {
    return Math.round(Math.exp(Math.log(lo + 1) + this.next() * (Math.log(hi + 1) - Math.log(lo + 1)))) - 1;
  }
  pick<T>(a: readonly T[]): T {
    return a[Math.floor(this.next() * a.length)];
  }
  normal(): number {
    const u = 1 - this.next(), v = this.next();
    return Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
  }
  logNormal(median: number, sigma: number): number {
    return median * Math.exp(sigma * this.normal());
  }
  hex(n: number): string {
    let s = "";
    for (let i = 0; i < n; i++) s += "0123456789abcdef"[this.int(0, 15)];
    return s;
  }
}

// ---- vocab ------------------------------------------------------------------------
const KB = 1024, MB = KB * 1024, GB = MB * 1024;
const NPM = "react react-dom typescript esbuild vite rollup webpack lodash eslint prettier postcss tailwindcss next sharp jest vitest chokidar glob semver yargs commander chalk debug ms axios zod date-fns dayjs moment rxjs core-js regenerator-runtime caniuse-lite browserslist electron-to-chromium terser source-map acorn ajv uuid ws express body-parser cookie qs mime-types iconv-lite safe-buffer readable-stream inherits minimatch brace-expansion picomatch micromatch fast-glob fsevents nanoid immer redux zustand graphql apollo-client prisma pg mysql2 sqlite3 better-sqlite3 dotenv cross-env rimraf mkdirp fs-extra execa cosmiconfig resolve enhanced-resolve memfs jsdom parse5 entities htmlparser2 cheerio puppeteer playwright-core three d3 chart.js monaco-editor pdfjs-dist highlight.js prismjs katex mermaid lucide-react framer-motion styled-components emotion sass less stylus autoprefixer cssnano svgo lightningcss swc turbo nx lerna husky lint-staged".split(" ");
const SCOPES: Record<string, string[]> = {
  "@babel": "core parser traverse types generator template helpers runtime preset-env preset-react plugin-transform-runtime compat-data code-frame".split(" "),
  "@types": "node react react-dom jest lodash express estree json-schema babel__core ws".split(" "),
  "@esbuild": ["darwin-arm64"],
  "@swc": "core core-darwin-arm64 helpers".split(" "),
  "@typescript-eslint": "parser eslint-plugin typescript-estree scope-manager utils types visitor-keys".split(" "),
  "@next": "env swc-darwin-arm64 font".split(" "),
  "@img": "sharp-darwin-arm64 sharp-libvips-darwin-arm64".split(" "),
  "@radix-ui": "react-dialog react-popover react-tooltip react-select primitive react-slot".split(" "),
  "@rollup": "rollup-darwin-arm64 pluginutils".split(" "),
  "@jest": "core environment expect reporters transform types".split(" "),
};
const PKG_SUB = "lib dist src esm cjs types build bin locales test modules internal utils helpers plugins compiled umd".split(" ");
const PROJECTS = "atlas-api atlas-web billing-service dotfiles infra-terraform relay-server diskspace.cc photo-sorter blog advent-of-code design-system mobile-app data-pipeline ml-experiments zyno-dashboard chrome-extension cli-tools notes-app rust-playground go-workers old-portfolio invoice-generator home-automation kubernetes-manifests weather-bot game-jam-2024 slides".split(" ");
const WORDS = "assets images components utils lib core common shared models views controllers services handlers routes api internal pkg cmd config scripts docs fixtures data migrations templates static public styles icons fonts locales tests mocks schemas workers jobs storage cache tmp logs archive old backup exports reports drafts 2021 2022 2023 2024 2025 misc vendor third_party proto generated".split(" ");
const CACHES = "com.apple.Safari com.apple.bird com.apple.akd com.apple.nsurlsessiond com.apple.helpd com.apple.iconservices.store com.apple.ap.adprivacyd com.apple.parsecd com.apple.Music com.apple.dt.Xcode com.apple.python com.spotify.client com.google.Chrome com.microsoft.VSCode com.tinyspeck.slackmacgap com.figma.Desktop com.docker.docker Homebrew pip yarn ms-playwright go-build node-gyp JetBrains CloudKit org.swift.swiftpm typescript electron com.openai.chat zoom.us com.hnc.Discord company.thebrowser.Browser Firefox GeoServices".split(" ");
const APPSUP = "Slack Code Google Spotify discord Figma JetBrains MobileSync Steam zoom.us Arc obsidian Claude Postman Docker Desktop CrashReporter com.apple.sharedfilelist Caches Firefox 1Password Raycast".split(" ");
const CONTAINERS = "com.docker.docker com.apple.mail com.apple.Notes com.apple.Safari com.apple.photoanalysisd com.apple.mediaanalysisd com.apple.Maps com.apple.news com.apple.stocks com.apple.weather com.apple.iWork.Pages com.apple.iWork.Keynote com.microsoft.Word com.microsoft.Excel com.apple.Preview".split(" ");
const APPS = "Xcode.app Docker.app Google Chrome.app Slack.app Visual Studio Code.app Figma.app Spotify.app Arc.app Logic Pro.app Final Cut Pro.app Microsoft Word.app Microsoft Excel.app Zoom.app Discord.app Steam.app Blender.app Affinity Photo 2.app OBS.app Obsidian.app 1Password.app".split(" ").reduce<string[]>((a, w) => {
  // re-join multi-word app names split above
  if (a.length && !a[a.length - 1].endsWith(".app")) a[a.length - 1] += " " + w;
  else a.push(w);
  return a;
}, []);
const SYS_NAMES = "Frameworks PrivateFrameworks Resources lib share bin Library Extensions Fonts Caches db var log etc include Modules en.lproj Assets CoreServices Kernels Speech Templates Updates Filesystems Plugins Components".split(" ");
const ARTISTS = "Radiohead Bonobo Khruangbin Phoebe Bridgers Nils Frahm Tycho Fleetwood Mac Daft Punk Burial Little Simz Jon Hopkins Floating Points Massive Attack Beach House Bon Iver".split(" ");

// ---- dir kinds ---------------------------------------------------------------------
enum K {
  VolRoot, Home, Library, Caches, CacheEntry, AppSupport, Containers, Container, Docker, Chain, Developer, Xcode, Derived, DerivedProj, Sim, Projects, Project, NodeModules, Scope, Pkg, PkgSub, Git, GitObjects, GitShard, GitPack, Src, Movies, FcpBundle, Pictures, PhotosLib, PhotoShard, Music, Artist, Album, Documents, Downloads, Generic, System, Apps, AppBundle, Users, Denied, Volumes, Trash, GoMod, Backup, BackupShard,
}

const STRUCTURAL = new Set<K>([K.VolRoot, K.Home, K.Users, K.Volumes, K.Library, K.Containers, K.Docker, K.Chain, K.Backup, K.AppSupport, K.Developer, K.Xcode, K.Movies, K.Pictures, K.PhotosLib, K.Music, K.Projects, K.Project, K.Derived]);

interface FileProfile {
  n: [number, number];
  median: number;
  sigma: number;
  ext: string[];
  /** extra named big files */
  big?: (g: Gen) => [string, number][];
}

const P = {
  none: { n: [0, 0], median: 0, sigma: 0, ext: [] } as FileProfile,
  tiny: { n: [0, 6], median: 4 * KB, sigma: 1.2, ext: ["plist", "json", "log", "db"] } as FileProfile,
  pkg: { n: [2, 30], median: 7 * KB, sigma: 1.7, ext: ["js", "d.ts", "map", "json", "md", "mjs", "cjs"] } as FileProfile,
  src: { n: [1, 24], median: 5 * KB, sigma: 1.4, ext: ["ts", "tsx", "go", "rs", "py", "swift", "json", "md", "css"] } as FileProfile,
  cache: { n: [3, 160], median: 48 * KB, sigma: 2.3, ext: ["db", "cache", "data", "bin", "sqlite", "dat"] } as FileProfile,
  derived: { n: [2, 40], median: 40 * KB, sigma: 1.6, ext: ["o", "swiftmodule", "swiftdoc", "dia", "d", "pcm", "a"] } as FileProfile,
  movies: { n: [3, 18], median: 700 * MB, sigma: 1.1, ext: ["mov", "mp4", "m4v"] } as FileProfile,
  render: { n: [4, 40], median: 90 * MB, sigma: 1.2, ext: ["mov", "fcpcache", "plist"] } as FileProfile,
  photo: { n: [300, 1400], median: 3.2 * MB, sigma: 0.55, ext: ["heic", "heic", "heic", "jpeg", "mov", "png"] } as FileProfile,
  thumb: { n: [150, 1100], median: 140 * KB, sigma: 0.6, ext: ["jpeg", "heic"] } as FileProfile,
  music: { n: [6, 18], median: 9 * MB, sigma: 0.5, ext: ["m4a", "mp3", "flac"] } as FileProfile,
  docs: { n: [3, 60], median: 300 * KB, sigma: 1.7, ext: ["pdf", "docx", "xlsx", "key", "pages", "txt", "md", "png", "jpg", "csv"] } as FileProfile,
  downloads: { n: [10, 70], median: 6 * MB, sigma: 2.4, ext: ["dmg", "zip", "pdf", "pkg", "png", "jpg", "mp4", "tar.gz", "csv"] } as FileProfile,
  generic: { n: [0, 24], median: 60 * KB, sigma: 2.1, ext: ["dat", "json", "plist", "db", "log", "bin", "txt"] } as FileProfile,
  system: { n: [2, 50], median: 30 * KB, sigma: 1.9, ext: ["dylib", "plist", "strings", "nib", "car", "tbd", "bundle"] } as FileProfile,
  git: { n: [4, 60], median: 1.8 * KB, sigma: 1.4, ext: [""] } as FileProfile,
  app: { n: [1, 30], median: 120 * KB, sigma: 2.4, ext: ["dylib", "plist", "nib", "car", "strings", "framework", "pak", "dat"] } as FileProfile,
  backup: { n: [40, 255], median: 300 * KB, sigma: 1.8, ext: [""] } as FileProfile,
};

const BULK = new Set<FileProfile>([P.pkg, P.src, P.derived, P.generic, P.system, P.cache, P.docs, P.git, P.tiny]);

const HEX16 = "0123456789ABCDEF".split("");

interface KidSpec {
  name: string;
  kind: K;
  flags?: number;
}

class Gen {
  rng: Rng;
  // per dir id
  kind = new Uint8Array(0);
  depth = new Uint8Array(0);
  parent = new Int32Array(0);
  ownAlloc = new Float64Array(0);
  ownApparent = new Float64Array(0);
  ownFiles = new Float64Array(0);
  names: string[] = [];
  extra: (string | undefined)[] = []; // per-dir hint (project type, big file…)
  listed = new Map<number, FileRec[]>();
  pool: number[] = [];
  nextId = 1;
  denied = 0;
  folds: { id: number; left: number }[] = [];
  totals = { dirs: 0, files: 0, alloc: 0, apparent: 0 };

  readonly bulkScale: number;

  constructor(readonly target: number, readonly rootKind: K, readonly rootName: string, readonly fdaDenied: boolean, seed: number, bulkBase = target) {
    this.rng = new Rng(seed);
    this.bulkScale = Math.max(0.3, Math.min(1, 30000 / Math.max(1, bulkBase)));
    const cap = target + 4096;
    this.kind = new Uint8Array(cap);
    this.depth = new Uint8Array(cap);
    this.parent = new Int32Array(cap);
    this.ownAlloc = new Float64Array(cap);
    this.ownApparent = new Float64Array(cap);
    this.ownFiles = new Float64Array(cap);
    this.kind[0] = rootKind;
    this.parent[0] = -1;
    this.names[0] = rootName;
    this.pool.push(0);
  }

  get done(): boolean {
    return this.pool.length === 0 && this.folds.length === 0;
  }

  path(id: number, rootPath: string): string {
    const parts: string[] = [];
    for (let k = id; k > 0; k = this.parent[k]) parts.push(this.names[k]);
    return (rootPath === "/" ? "" : rootPath) + "/" + parts.reverse().join("/");
  }

  /** pick the next pending dir: mostly depth-first-ish, sometimes anywhere */
  private take(): number {
    const p = this.pool;
    const r = this.rng.next();
    let i: number;
    if (r < 0.62) i = p.length - 1 - Math.floor(this.rng.next() * Math.min(48, p.length));
    else i = Math.floor(this.rng.next() * p.length);
    const id = p[i];
    p[i] = p[p.length - 1];
    p.pop();
    return id;
  }

  /** emit up to n records */
  emit(n: number, out: NodeRecord[]): number {
    let bytes = 0;
    // pending folds first (a few per tick)
    for (let f = 0; f < 3 && this.folds.length; f++) {
      const fi = Math.floor(this.rng.next() * this.folds.length);
      const fold = this.folds[fi];
      const files = this.rng.logInt(20, 2000);
      const alloc = files * this.rng.logNormal(9 * KB, 0.8);
      const apparent = alloc * 0.86;
      const dirs = this.rng.int(4, 120);
      out.push({ k: 2, targetId: fold.id, alloc: Math.round(alloc), apparent: Math.round(apparent), files, dirs });
      this.ownAlloc[fold.id] += Math.round(alloc);
      this.ownApparent[fold.id] += Math.round(apparent);
      this.ownFiles[fold.id] += files;
      this.add(files, Math.round(alloc), Math.round(apparent), dirs);
      bytes += 24;
      if (--fold.left <= 0) {
        out.push({ k: 3, targetId: fold.id });
        this.folds[fi] = this.folds[this.folds.length - 1];
        this.folds.pop();
        bytes += 4;
      }
    }
    for (let i = 0; i < n && this.pool.length; i++) {
      const rec = this.dir(this.take());
      out.push(rec);
      if (rec.dirs.length >= 6) {
        // exercise DIRMORE: move the second half of the child list into a continuation
        const cut = rec.dirs.length >> 1;
        out.push({ k: 4, id: rec.id, firstChildId: rec.firstChildId + cut, dirs: rec.dirs.splice(cut) });
      }
      bytes += 14;
      for (let m = out.length - 1; m >= 0; m--) {
        const o = out[m];
        if (o === rec) break;
        if (o.k === 4) for (const c of o.dirs) bytes += c.name.length + 2;
      }
      for (const c of rec.dirs) bytes += c.name.length + 2;
      for (const f of rec.files) bytes += f.name.length + 12;
    }
    return bytes;
  }

  private add(files: number, alloc: number, apparent: number, dirs: number): void {
    this.totals.files += files;
    this.totals.alloc += alloc;
    this.totals.apparent += apparent;
    this.totals.dirs += dirs;
  }

  private dir(id: number): DirRec {
    const kind = this.kind[id] as K;
    const depth = this.depth[id];
    const g = this.rng;
    let flags = 0;
    let kids: KidSpec[] = [];
    let prof: FileProfile = P.generic;
    if (kind === K.Denied) {
      flags = DIR_FLAG.READ_DENIED;
      this.denied++;
      prof = P.none;
    } else {
      [kids, prof] = this.spec(id, kind, depth);
    }
    // global budget: stop growing bulk dirs once we hit the target
    // (structural paths like Docker.raw or the Photos library always resolve)
    if (!STRUCTURAL.has(kind)) {
      const room = this.target - this.nextId;
      if (room <= 0) kids = kids.filter((k) => k.flags);
      else if (kids.length > room) kids = kids.slice(0, room);
    }

    // fold some deep, bushy dirs (scanner limits)
    if (kids.length > 6 && depth > 7 && g.next() < 0.03) {
      flags |= DIR_FLAG.FOLDED_CHILDREN;
      kids = [];
      this.folds.push({ id, left: g.int(1, 3) });
    }

    // files
    const nFiles = prof.n[1] ? g.logInt(prof.n[0], prof.n[1]) : 0;
    const big = prof.big ? prof.big(this) : [];
    const sample = Math.min(nFiles, 12);
    const indiv: [string, number][] = [];
    // names are generated lazily, only for files that end up listed
    // bulk (many-small-files) profiles shrink as the tree grows so totals stay plausible
    const sc = BULK.has(prof) ? this.bulkScale : 1;
    for (let i = 0; i < sample; i++) indiv.push(["", Math.max(1, Math.round(sc * g.logNormal(prof.median, prof.sigma)))]);
    let restApparent = 0;
    if (nFiles > sample) restApparent = sc * (nFiles - sample) * prof.median * Math.exp((prof.sigma * prof.sigma) / 4);
    for (const b of big) indiv.push(b);
    let apparent = restApparent;
    let alloc = Math.ceil(restApparent / 4096) * 4096 + (nFiles - sample) * 2048 + 4096;
    for (const [, s] of indiv) {
      apparent += s;
      alloc += Math.ceil(s / 4096) * 4096;
    }
    // listed: top-8 files ≥ 1 MiB
    indiv.sort((a, b) => b[1] - a[1]);
    const files: FileRec[] = [];
    for (let i = 0; i < indiv.length && files.length < 8; i++) {
      const [nm, s] = indiv[i];
      if (s < MB) break;
      const name = nm || this.fileName(prof);
      files.push({ name, alloc: Math.ceil(s / 4096) * 4096, apparent: s, flags: 0 });
    }
    if (files.length) this.listed.set(id, files);
    const fileCount = nFiles + big.length;
    if (flags & DIR_FLAG.READ_DENIED) {
      alloc = 0;
      apparent = 0;
    }
    this.ownAlloc[id] = Math.round(alloc);
    this.ownApparent[id] = Math.round(apparent);
    this.ownFiles[id] = flags & DIR_FLAG.READ_DENIED ? 0 : fileCount;
    this.add(this.ownFiles[id], this.ownAlloc[id], this.ownApparent[id], 1);

    // announce children
    const first = this.nextId;
    const dirs: ChildRec[] = [];
    for (const k of kids) {
      const cid = this.nextId++;
      this.kind[cid] = k.kind;
      this.depth[cid] = depth + 1;
      this.parent[cid] = id;
      this.names[cid] = k.name;
      dirs.push({ name: k.name, flags: k.flags ?? 0 });
      if (!k.flags) this.pool.push(cid);
    }
    return { k: 1, id, flags, ownAlloc: this.ownAlloc[id], ownApparent: this.ownApparent[id], ownFiles: this.ownFiles[id], entryErrors: 0, totalDirs: dirs.length, firstChildId: dirs.length ? first : 0, dirs, files };
  }

  fileName(p: FileProfile): string {
    const g = this.rng;
    const ext = g.pick(p.ext);
    let base: string;
    if (p === P.photo) base = `IMG_${g.int(1000, 9999)}`;
    else if (p === P.movies || p === P.render) base = g.pick(["Screen Recording 2025-0", "Export ", "Interview take ", "Drone ", "Wedding cut ", "B-roll "]) + g.int(1, 98);
    else if (p === P.music) base = `${String(g.int(1, 14)).padStart(2, "0")} ${g.pick(["Intro", "Night Drive", "Weightless", "Kerala", "Reckoner", "Says", "Lune", "Awake", "Sunday", "Glow"])}`;
    else if (p === P.git || p === P.backup) return g.hex(38);
    else if (p === P.downloads) base = g.pick(["Install ", "invoice-", "Screenshot 2025-", "statement-", "export-", "dataset-", "recording-", "slides-"]) + g.int(1, 400);
    else base = g.pick(WORDS) + (g.next() < 0.5 ? "-" + g.hex(4) : "");
    return ext ? `${base}.${ext}` : base;
  }

  private spec(id: number, kind: K, depth: number): [KidSpec[], FileProfile] {
    const g = this.rng;
    const k = (name: string, kind: K, flags?: number): KidSpec => ({ name, kind, flags });
    const many = (n: number, mk: (i: number) => KidSpec): KidSpec[] => {
      const out: KidSpec[] = [];
      const seen = new Set<string>();
      for (let i = 0; i < n; i++) {
        const s = mk(i);
        if (!seen.has(s.name)) {
          seen.add(s.name);
          out.push(s);
        }
      }
      return out;
    };
    const decay = (base: number) => Math.max(0, Math.round(base - depth * 0.9 + g.normal() * 1.5));
    const fda = this.fdaDenied;
    switch (kind) {
      case K.VolRoot:
        return [[k("Applications", K.Apps), k("Library", K.System), k("System", K.System), k("Users", K.Users), k("Volumes", K.Volumes), k("opt", K.System), k("private", K.System), k("usr", K.System), k("cores", K.Denied), k(".Spotlight-V100", K.Denied), k(".fseventsd", K.Denied)], P.tiny];
      case K.Users:
        return [[k("alex", K.Home), k("Shared", K.Generic), k("maria", K.Denied), k("Guest", K.Denied)], P.none];
      case K.Volumes:
        return [[k("Backup Plus", K.Generic, CHILD_FLAG.OTHER_DEVICE), k("Shared", K.Generic, CHILD_FLAG.OTHER_DEVICE), k("Macintosh HD", K.Generic, CHILD_FLAG.REPARSE)], P.none];
      case K.Home:
        return [
          [k("Library", K.Library), k("Projects", K.Projects), k("Movies", K.Movies), k("Pictures", K.Pictures), k("Music", K.Music), k("Documents", K.Documents), k("Downloads", K.Downloads), k("Desktop", K.Documents), k(".Trash", K.Trash), k(".npm", K.Caches), k(".cache", K.Caches), k("go", K.GoMod), k(".vscode", K.Generic), k(".docker", K.Generic), k("Applications", K.Apps)],
          { ...P.tiny, n: [4, 14] },
        ];
      case K.Library:
        return [
          [
            k("Caches", K.Caches),
            k("Application Support", K.AppSupport),
            k("Containers", K.Containers),
            k("Developer", K.Developer),
            k("Group Containers", K.AppSupport),
            k("Mail", fda ? K.Denied : K.Generic),
            k("Messages", fda ? K.Denied : K.Generic),
            k("Safari", fda ? K.Denied : K.Generic),
            k("Logs", K.Generic),
            k("Mobile Documents", K.Documents),
            k("Preferences", K.Generic),
            k("Saved Application State", K.Generic),
          ],
          P.tiny,
        ];
      case K.Caches:
        return [many(g.int(16, 34), () => k(g.pick(CACHES), K.CacheEntry)), P.cache];
      case K.CacheEntry:
        return [many(decay(6), () => k(g.next() < 0.5 ? g.hex(2) : g.pick(["Cache.db", "fsCachedData", "com.apple.metal", "blobs", "index", "Code Cache", "GPUCache", "_cacache", "content-v2", "wheels", "http-v2"]), K.CacheEntry)), P.cache];
      case K.AppSupport:
        return [many(g.int(10, 20), (i) => {
          const n = i === 0 && this.names[id] === "Application Support" ? "MobileSync" : g.pick(APPSUP);
          return k(n, n === "MobileSync" ? K.Backup : K.Generic);
        }), P.tiny];
      case K.Backup:
        return [[k("Backup", K.Chain)], P.none];
      case K.Containers:
        return [many(g.int(10, 15), (i) => {
          const n = i === 0 ? "com.docker.docker" : g.pick(CONTAINERS);
          return k(n, n === "com.docker.docker" ? K.Docker : n === "com.apple.Safari" && fda ? K.Denied : K.Container);
        }), P.none];
      case K.Container:
        return [[k("Data", K.Generic)], P.tiny];
      case K.Docker:
        return [[k("Data", K.Chain)], P.tiny];
      case K.Chain: {
        // fixed single-path chains ending in a huge file
        const pth = this.path(id, "");
        if (pth.endsWith("com.docker.docker/Data")) return [[k("vms", K.Chain), k("log", K.Generic)], P.tiny];
        if (pth.endsWith("Data/vms")) return [[k("0", K.Chain)], P.none];
        if (pth.endsWith("vms/0")) return [[k("data", K.Chain)], { ...P.tiny, big: () => [["console-ring", 4 * MB]] }];
        if (pth.endsWith("0/data")) return [[], { ...P.none, big: () => [["Docker.raw", 61.4 * GB]] }];
        if (pth.endsWith("MobileSync/Backup")) return [[k(g.hex(8).toUpperCase() + "-" + g.hex(16).toUpperCase(), K.Chain), k(g.hex(8).toUpperCase() + "-" + g.hex(16).toUpperCase(), K.Chain)], P.none];
        if (/MobileSync\/Backup\/[0-9A-F-]+$/.test(pth)) return [many(64, (i) => k(g.hex(1) + HEX16[i % 16].toLowerCase(), K.BackupShard)), { ...P.tiny, big: () => [["Manifest.db", 210 * MB]] }];
        return [[], P.generic];
      }
      case K.Developer:
        return [[k("Xcode", K.Xcode), k("CoreSimulator", K.Sim)], P.none];
      case K.Xcode:
        return [[k("DerivedData", K.Derived), k("iOS DeviceSupport", K.Generic), k("Archives", K.Generic), k("UserData", K.Generic)], P.tiny];
      case K.Derived:
        return [many(g.int(8, 18), () => k(`${g.pick(["Atlas", "Weather", "Notes", "Relay", "Scanner", "Widgets", "Camera", "Kit"])}-${g.hex(28).replace(/[0-9]/g, (c) => "abcdefghij"[+c])}`, K.DerivedProj)), P.none];
      case K.DerivedProj:
        return [many(decay(11), () => k(g.pick(["Build", "Index.noindex", "Intermediates.noindex", "Products", "Debug-iphonesimulator", "Objects-normal", "arm64", "SourcePackages", "checkouts", "ModuleCache.noindex", g.hex(10)]), K.DerivedProj)), P.derived];
      case K.Sim:
        return [many(g.int(4, 9), () => k(`${g.hex(8)}-${g.hex(4)}-${g.hex(4)}-${g.hex(4)}-${g.hex(12)}`.toUpperCase(), K.Generic)), P.tiny];
      case K.Projects:
        return [many(g.int(18, 28), () => k(g.pick(PROJECTS), K.Project)), P.tiny];
      case K.Project: {
        const kids = [k("src", K.Src), k(".git", K.Git)];
        if (g.next() < 0.75) kids.push(k("node_modules", K.NodeModules));
        if (g.next() < 0.5) kids.push(k(g.pick(["dist", "build", "out"]), K.Src));
        if (g.next() < 0.5) kids.push(k("test", K.Src));
        if (g.next() < 0.3) kids.push(k("docs", K.Src));
        if (g.next() < 0.18) kids.push(k("target", K.DerivedProj));
        if (g.next() < 0.2) kids.push(k(".next", K.DerivedProj));
        if (g.next() < 0.3) kids.push(k("public", K.Documents));
        return [kids, { ...P.src, n: [3, 14] }];
      }
      case K.NodeModules: {
        const n = depth > 6 ? g.int(2, 12) : g.int(120, 640);
        const kids = many(n, () => {
          if (g.next() < 0.12) {
            const scopes = Object.keys(SCOPES);
            return k(g.pick(scopes), K.Scope);
          }
          if (g.next() < 0.55) return k(g.pick(NPM), K.Pkg);
          return k(`${g.pick(["is-", "get-", "has-", "to-", "p-", "fast-", "@", "node-", ""])}${g.pick(WORDS)}${g.pick(["", "-js", "-utils", "-parser", "ify", "2", "-plugin"])}`.replace("@", ""), K.Pkg);
        });
        kids.push(k(".bin", K.Generic), k(".cache", K.CacheEntry));
        return [kids, P.tiny];
      }
      case K.Scope: {
        const nm = this.names[id];
        return [(SCOPES[nm] ?? []).map((s) => k(s, K.Pkg)), P.none];
      }
      case K.Pkg: {
        const kids = many(g.int(1, 4), () => k(g.pick(PKG_SUB), K.PkgSub));
        if (g.next() < 0.06 && depth < 8) kids.push(k("node_modules", K.NodeModules));
        const nm = this.names[id];
        const big: [string, number][] = [];
        if (/esbuild|darwin-arm64|swc|sharp|libvips|typescript|playwright|electron|monaco|turbo/.test(nm)) big.push([g.pick(["index.node", "bin", "typescript.js", "lib.dom.d.ts", "libvips-cpp.dylib", "next-swc.darwin-arm64.node"]), g.logNormal(18 * MB, 0.7)]);
        return [kids, { ...P.pkg, big: () => big }];
      }
      case K.PkgSub:
        return [many(decay(3.5), () => k(g.pick(PKG_SUB), K.PkgSub)), P.pkg];
      case K.Git:
        return [[k("objects", K.GitObjects), k("refs", K.Generic), k("hooks", K.Generic), k("logs", K.Generic), k("info", K.Generic)], P.tiny];
      case K.GitObjects:
        return [[k("pack", K.GitPack), k("info", K.Generic), ...many(g.int(10, 90), () => k(g.hex(2), K.GitShard))], P.none];
      case K.GitShard:
        return [[], P.git];
      case K.GitPack:
        return [[], { ...P.none, big: () => { const s = g.logNormal(60 * MB, 1.3); return [[`pack-${g.hex(40)}.pack`, s], [`pack-${g.hex(40)}.idx`, s * 0.03]]; } }];
      case K.Src:
        return [many(decay(6.5), () => k(g.pick(WORDS), K.Src)), P.src];
      case K.Movies:
        return [[k("Screen Recordings", K.Generic), k("Final Cut Projects", K.FcpBundle), k("Exports", K.Generic), k("iMovie Library.imovielibrary", K.FcpBundle)], { ...P.movies, n: [4, 10] }];
      case K.FcpBundle:
        return [many(decay(5), () => k(g.pick(["Render Files", "Transcoded Media", "Original Media", "Analysis Files", "Peaks Data", "High Quality Media", g.pick(["Trip to Lisbon", "Wedding", "Client promo", "Kids 2024"])]), K.FcpBundle)), P.render];
      case K.Pictures:
        return [[k("Photos Library.photoslibrary", K.PhotosLib), k("Screenshots", K.Generic), k("Lightroom", K.Generic), k("Wallpapers", K.Generic)], { ...P.docs, n: [2, 8] }];
      case K.PhotosLib: {
        const nm = this.names[id];
        if (nm === "Photos Library.photoslibrary") return [[k("originals", K.PhotosLib), k("resources", K.PhotosLib), k("database", K.Generic), k("private", K.Generic), k("scopes", K.Generic)], P.tiny];
        if (nm === "resources") return [[k("derivatives", K.PhotosLib), k("renders", K.PhotosLib)], P.none];
        return [HEX16.map((h) => k(h, K.PhotoShard)), P.none];
      }
      case K.PhotoShard: {
        const par = this.names[this.parent[id]];
        return [[], par === "originals" ? P.photo : P.thumb];
      }
      case K.Music:
        return [[k("Music", K.Generic), k("Logic", K.Generic), k("GarageBand", K.Generic), k("Media", K.Artist)], P.tiny];
      case K.Artist:
        return [many(g.int(8, 15), () => k(g.pick(ARTISTS), K.Album)), P.none];
      case K.BackupShard:
        return [[], P.backup];
      case K.Album:
        return [[], P.music];
      case K.Documents:
        return [many(decay(7), () => k(g.pick(["Invoices", "Taxes 2024", "Contracts", "Receipts", "Scans", "Notes", "Presentations", "Archive", "Old", "Travel", "Manuals", "Resumes", ...WORDS]), K.Documents)), P.docs];
      case K.Downloads:
        return [many(g.int(4, 9), () => k(g.pick(["unzipped", "Archive", "drivers", "fonts", "assets", "old installers", "datasets"]), K.Documents)), {
          ...P.downloads,
          big: () => [["Xcode_16.2.xip", 3.1 * GB], ["ubuntu-24.04.1-desktop-arm64.iso", 2.9 * GB], ["Docker.dmg", 615 * MB], ["Figma.dmg", 245 * MB]],
        }];
      case K.Trash:
        return [many(g.int(2, 6), () => k(g.pick(["old-project", "Untitled Folder", "export copy", "node_modules"]), K.Src)), { ...P.downloads, n: [2, 20] }];
      case K.GoMod:
        return [[k("pkg", K.Src), k("bin", K.Generic)], P.tiny];
      case K.Apps:
        return [many(g.int(10, 20), () => k(g.pick(APPS), K.AppBundle)), P.none];
      case K.AppBundle:
        return [[k("Contents", K.System)], { ...P.none, big: () => [["Assets.car", g.logNormal(380 * MB, 1.1)]] }];
      case K.System: {
        const nm = this.names[id];
        const top: FileProfile | null =
          depth === 1 && nm === "System"
            ? { ...P.system, big: () => [["dyld_shared_cache_arm64e", 3.4 * GB], ["dyld_shared_cache_arm64e.symbols", 1.9 * GB], ["kernelcache", 92 * MB]] }
            : depth === 1 && nm === "private"
              ? { ...P.tiny, big: () => [["sleepimage", 2.1 * GB], ["swapfile0", 1 * GB], ["swapfile1", 1 * GB]] }
              : null;
        const res = this.systemKids(depth);
        return [res, top ?? P.system];
      }
      case K.Generic:
      default:
        return [many(decay(5), () => k(g.pick(WORDS), K.Generic)), P.generic];
    }
  }

  private systemKids(depth: number): KidSpec[] {
    const g = this.rng;
    const n = Math.max(0, Math.round((depth < 2 ? 9 : 5.5) - depth * 0.9 + g.normal() * 1.5));
    const out: KidSpec[] = [];
    const seen = new Set<string>();
    for (let i = 0; i < n; i++) {
      const name = g.pick(SYS_NAMES);
      if (seen.has(name)) continue;
      seen.add(name);
      out.push({ name, kind: g.next() < 0.004 ? K.Denied : K.System });
    }
    return out;
  }

}

// ---- session ----------------------------------------------------------------------

const DEMO_VOLUMES: Volume[] = [
  { path: "/", label: "Macintosh HD", fs: "apfs", total: 994_662_584_320, free: 658_262_584_320, used: 336_400_000_000, kind: "boot", readOnly: false },
  { path: "/Volumes/Backup Plus", label: "Backup Plus", fs: "exfat", total: 2_000_398_934_016, free: 788_200_000_000, used: 1_212_198_934_016, kind: "external", readOnly: false },
  { path: "/Volumes/Shared", label: "Shared", fs: "smbfs", total: 3_998_000_000_000, free: 1_102_000_000_000, used: 2_896_000_000_000, kind: "network", readOnly: false },
];

export class MockSession implements Session {
  private ev: SessionEvents | null = null;
  private timers: number[] = [];
  private scanTimer = 0;
  private progTimer = 0;
  private scanId = 0;
  private gen: Gen | null = null;
  private scanPath = "";
  private t0 = 0;
  private paused = false;
  private pausedAt = 0;
  private pausedTotal = 0;
  private reqId = 0;
  private closed = false;
  readonly info: AgentInfo;

  constructor(private o: MockOptions = {}) {
    this.info = {
      proto: PROTO_VERSION,
      version: "0.1.0-demo",
      os: "darwin",
      arch: "arm64",
      hostname: "Alexs-MacBook-Pro",
      user: "alex",
      invokingUser: "alex",
      elevated: false,
      elevation: "none",
      fda: "denied",
      root: null,
      cwd: { path: "/Users/alex/Projects" },
      home: "/Users/alex",
      autoScan: o.autoScan ?? null,
      volumes: DEMO_VOLUMES,
      scan: null,
      features: { pause: true, delete: { available: true, confirm: "terminal", trash: true, maxItems: 500 } },
      deleting: null,
    };
  }

  readonly id = "demo";
  private job = 0;
  private del: { job: number; req: DeleteReq; status: DeleteStatus; stop: boolean; timer: number } | null = null;

  private later(ms: number, fn: () => void): void {
    this.timers.push(window.setTimeout(fn, ms));
  }

  start(events: SessionEvents): void {
    this.ev = events;
    const q = this.o.quick ? 0.25 : 1;
    events.state({ s: "connecting" });
    this.later(380 * q, () => events.state({ s: "handshaking" }));
    this.later(820 * q, () => {
      const ds = this.o.demoState;
      if (ds === "key-mismatch") return events.state({ s: "error", kind: "key-mismatch", message: "handshake MAC mismatch" });
      if (ds === "no-session") return events.state({ s: "error", kind: "no-session", message: "unknown session" });
      events.state({ s: "connected", code: "482 913" });
      this.later(140 * q, () => {
        events.agentInfo(this.info);
        if (this.info.autoScan) this.scanStart(this.info.autoScan);
        if (ds === "superseded") this.later(600, () => events.state({ s: "superseded" }));
        if (ds === "closed") this.later(600, () => events.state({ s: "closed", reason: "shutdown" }));
      });
    });
  }

  scanStart(path: string, _opts?: ScanOptions): void {
    if (!this.ev || this.closed) return;
    this.stopScan();
    const ev = this.ev;
    const id = ++this.scanId;
    const norm = path.replace(/\/+$/, "") || "/";
    let kind = K.Generic;
    let rootName = norm.split("/").pop() || norm;
    if (norm === "/") {
      kind = K.VolRoot;
      rootName = "Macintosh HD";
    } else if (norm === this.info.home) kind = K.Home;
    else if (norm.endsWith("/Projects")) kind = K.Projects;
    else if (norm.startsWith("/Volumes/")) kind = K.Downloads;
    else if (/Library$/.test(norm)) kind = K.Library;
    else if (/node_modules$/.test(norm)) kind = K.NodeModules;
    else if (/Movies$/.test(norm)) kind = K.Movies;
    else if (/Pictures$/.test(norm)) kind = K.Pictures;
    const vol = this.info.volumes.find((v) => v.path !== "/" && (norm === v.path || norm.startsWith(v.path + "/"))) ?? this.info.volumes[0];
    const target = this.o.targetDirs ?? 160_000;
    const scale = kind === K.VolRoot ? 1.5 : kind === K.Home ? 1 : kind === K.Projects ? 0.6 : 0.25;
    const gen = new Gen(Math.round(target * scale), kind, rootName, this.info.fda === "denied" && !this.info.elevated, (this.o.seed ?? 7) + id * 7919, Math.round(target * Math.min(1, scale)));
    this.gen = gen;
    this.scanPath = norm;
    this.t0 = performance.now();
    this.paused = false;
    this.pausedTotal = 0;
    ev.scanStarted({ scanId: id, path: norm, rootName, volume: { total: vol.total, free: vol.free, used: vol.used }, opts: { crossMounts: false } });

    const duration = (this.o.durationMs ?? 10_000) * Math.min(1.15, Math.max(0.35, scale));
    const tickMs = 50;
    const perTick = Math.max(20, Math.ceil(gen.target / (duration / tickMs)));
    let first = true;
    const tick = () => {
      if (this.gen !== gen) return;
      if (this.paused) {
        this.scanTimer = window.setTimeout(tick, tickMs);
        return;
      }
      const recs: NodeRecord[] = [];
      // vary the pace like a real disk: bursts and slow patches
      const pace = 0.5 + Math.abs(Math.sin((performance.now() - this.t0) / 900)) * 1.1;
      const bytes = gen.emit(Math.round((first ? 8 : perTick) * pace), recs);
      first = false;
      if (recs.length) ev.nodes({ scanId: id, records: recs, wireBytes: bytes });
      if (gen.done) {
        this.sendProgress(id);
        const ms = this.elapsed();
        window.clearInterval(this.progTimer);
        this.progTimer = 0;
        this.scanTimer = 0;
        this.later(60, () => {
          if (this.gen !== gen) return;
          ev.scanDone({ scanId: id, totals: { ...gen.totals }, denied: gen.denied, errors: 0, vanished: 0, skippedMounts: 2, hardlinksDeduped: 214, folded: 0, ms });
        });
        return;
      }
      this.scanTimer = window.setTimeout(tick, tickMs);
    };
    this.scanTimer = window.setTimeout(tick, 30);
    this.progTimer = window.setInterval(() => this.sendProgress(id), 250);
  }

  private sendProgress(id: number): void {
    const g = this.gen;
    if (!g || !this.ev || id !== this.scanId || this.paused) return;
    const last = g.pool.length ? g.pool[g.pool.length - 1] : 0;
    this.ev.progress({ scanId: id, dirs: g.totals.dirs, files: g.totals.files, alloc: g.totals.alloc, apparent: g.totals.apparent, errors: 0, elapsedMs: this.elapsed(), currentPath: g.path(last, this.scanPath) });
  }

  /** running time excluding paused time */
  private elapsed(): number {
    const now = performance.now();
    return Math.round(now - this.t0 - this.pausedTotal - (this.paused ? now - this.pausedAt : 0));
  }

  scanPause(scanId: number, paused: boolean): void {
    if (scanId !== this.scanId || !this.gen || !this.ev) return;
    const ev = this.ev;
    if (paused !== this.paused) {
      const now = performance.now();
      if (paused) this.pausedAt = now;
      else this.pausedTotal += now - this.pausedAt;
      this.paused = paused;
    }
    // the agent answers every request with its current state
    this.later(60, () => {
      if (scanId === this.scanId) ev.scanState({ scanId, paused: this.paused, elapsedMs: this.elapsed() });
    });
  }

  private stopScan(): void {
    window.clearTimeout(this.scanTimer);
    window.clearInterval(this.progTimer);
    this.scanTimer = this.progTimer = 0;
  }

  scanCancel(scanId: number): void {
    // the generated tree stays (Show all, deletions); only a running scan can be canceled
    if (scanId !== this.scanId || !this.gen || !this.scanTimer) return;
    this.stopScan();
    this.paused = false;
    this.later(80, () => this.ev?.scanEnded({ scanId, reason: "canceled" }));
  }

  browse(path: string): Promise<BrowseResult> {
    const reqId = ++this.reqId;
    const norm = path.replace(/\/+$/, "") || "/";
    const fixed: Record<string, string[]> = {
      "/": ["Applications", "Library", "System", "Users", "Volumes", "opt", "private", "usr"],
      "/Users": ["Shared", "alex"],
      "/Users/alex": ["Applications", "Desktop", "Documents", "Downloads", "Library", "Movies", "Music", "Pictures", "Projects", "go"],
      "/Users/alex/Projects": [...PROJECTS].sort(),
      "/Volumes": ["Backup Plus", "Shared"],
      "/Users/alex/Library": ["Application Support", "Caches", "Containers", "Developer", "Group Containers", "Logs", "Mobile Documents", "Preferences"],
    };
    let dirs = fixed[norm];
    if (!dirs) {
      const r = new Rng(hash(norm));
      const n = norm.split("/").length > 7 ? 0 : r.int(0, 9);
      dirs = Array.from(new Set(Array.from({ length: n }, () => r.pick(WORDS)))).sort();
    }
    const parent = norm === "/" ? null : norm.slice(0, norm.lastIndexOf("/")) || "/";
    return new Promise((res) => this.later(160, () => res({ reqId, path: norm, parent, dirs })));
  }

  dirFiles(scanId: number, dirId: number, limit = 10_000): Promise<DirFiles> {
    const reqId = ++this.reqId;
    const g = this.gen;
    return new Promise((res, rej) =>
      this.later(280, () => {
        if (!g || scanId !== this.scanId) return rej(new Error("scan no longer available"));
        const own = g.ownAlloc[dirId], ownApp = g.ownApparent[dirId], nFiles = g.ownFiles[dirId];
        const listed = g.listed.get(dirId) ?? [];
        const files: FileRec[] = [...listed];
        let usedA = listed.reduce((s, f) => s + f.alloc, 0);
        let usedP = listed.reduce((s, f) => s + f.apparent, 0);
        const remaining = Math.max(0, nFiles - listed.length);
        const want = Math.min(remaining, limit - listed.length);
        const budget = Math.max(0, own - usedA - 4096);
        const prof = P.generic;
        // spread the rest with a decaying distribution so it sums ≤ own
        let left = budget * 0.92;
        for (let i = 0; i < want && left > 4096; i++) {
          const share = left * (0.12 + g.rng.next() * 0.2) * (want - i > 1 ? 1 : 1);
          const a = Math.max(4096, Math.floor(share / 4096) * 4096);
          left -= a;
          usedA += a;
          usedP += a * 0.93;
          files.push({ name: g.fileName(prof), alloc: a, apparent: Math.round(a * 0.93), flags: 0 });
        }
        files.sort((a, b) => b.alloc - a.alloc);
        res({ reqId, scanId, dirId, flags: 0, restCount: Math.max(0, nFiles - files.length), restAlloc: Math.max(0, own - usedA), restApparent: Math.max(0, ownApp - usedP), files });
      }),
    );
  }

  // ---- deletion -------------------------------------------------------------------

  deleteRequest(req: DeleteReq): void {
    const ev = this.ev;
    const g = this.gen;
    if (!ev || this.closed) return;
    const job = ++this.job;
    const base: DeleteStatus = { job, reqId: req.reqId, state: "preparing", total: req.items.length, done: 0, freedAlloc: 0 };
    const refuse = (state: "refused" | "failed", code: string) => this.later(80, () => ev.deleteStatus({ ...base, state, code }));
    if (this.del) return refuse("failed", "busy");
    if (!g || req.scanId !== this.scanId) return refuse("failed", "invalid");
    if (this.scanTimer) return refuse("refused", "scanning");
    const d = { job, req, status: base, stop: false, timer: 0 };
    this.del = d;
    const send = (st: Partial<DeleteStatus>) => {
      d.status = { ...d.status, ...st };
      this.info.deleting = { job, state: d.status.state, done: d.status.done, total: d.status.total };
      ev.deleteStatus(d.status);
    };
    const finish = (st: Partial<DeleteStatus>) => {
      window.clearTimeout(d.timer);
      if (this.del === d) this.del = null;
      send(st);
      this.info.deleting = null;
    };
    this.later(120, () => send({}));
    d.timer = window.setTimeout(() => {
      send({ state: "awaiting" });
      const wait = this.o.approve ?? 3500;
      d.timer = window.setTimeout(() => {
        if (wait === "no") return finish({ state: "refused", code: "declined" });
        this.runDelete(d, g, send, finish);
      }, wait === "no" ? 2500 : wait);
    }, 600);
  }

  private runDelete(d: NonNullable<MockSession["del"]>, g: Gen, send: (s: Partial<DeleteStatus>) => void, finish: (s: Partial<DeleteStatus>) => void): void {
    const ev = this.ev!;
    const items = d.req.items;
    const results: DeleteResult[] = [];
    const deleted: DeletedItem[] = [];
    const tot = subtreeTotals(g);
    let i = 0;
    let freed = 0;
    const cur = (k: number) => {
      const it = items[k];
      return "dir" in it ? g.path(it.dir, this.scanPath) : g.path(it.parent, this.scanPath) + "/" + it.name;
    };
    send({ state: "running", current: items.length ? cur(0) : undefined });
    const step = () => {
      if (i >= items.length || d.stop) {
        for (let k = i; k < items.length; k++) results.push({ i: k, ok: false, code: "canceled", freedAlloc: 0, files: 0, dirs: 0 });
        if (deleted.length) ev.deleted({ scanId: d.req.scanId, job: d.job, items: deleted });
        finish({ state: "done", done: i, freedAlloc: freed, current: undefined, results });
        return;
      }
      const it = items[i];
      const path = cur(i);
      if (/\/Library\/Containers(\/|$)/.test(path)) {
        results.push({ i, ok: false, code: "denied", freedAlloc: 0, files: 0, dirs: 0 });
      } else if ("dir" in it) {
        const id = it.dir;
        if (id <= 0 || id >= g.nextId) results.push({ i, ok: false, code: "not_found", freedAlloc: 0, files: 0, dirs: 0 });
        else {
          freed += tot.alloc[id];
          results.push({ i, ok: true, freedAlloc: tot.alloc[id], files: tot.files[id], dirs: tot.dirs[id] });
          deleted.push({ dir: id, freedAlloc: tot.alloc[id], freedApparent: tot.apparent[id], files: tot.files[id], dirs: tot.dirs[id] });
        }
      } else {
        const f = g.listed.get(it.parent)?.find((x) => x.name === it.name);
        if (!f) results.push({ i, ok: false, code: "not_found", freedAlloc: 0, files: 0, dirs: 0 });
        else {
          freed += f.alloc;
          results.push({ i, ok: true, freedAlloc: f.alloc, files: 1, dirs: 0 });
          deleted.push({ parent: it.parent, name: it.name, freedAlloc: f.alloc, freedApparent: f.apparent });
        }
      }
      i++;
      send({ done: i, freedAlloc: freed, current: i < items.length ? cur(i) : undefined });
      d.timer = window.setTimeout(step, 450 + Math.min(900, items.length * 40));
    };
    d.timer = window.setTimeout(step, 700);
  }

  deleteCancel(job: number): void {
    const d = this.del;
    if (!d || d.job !== job || !this.ev) return;
    if (d.status.state === "running") {
      d.stop = true; // stop after the current item
      return;
    }
    window.clearTimeout(d.timer);
    this.del = null;
    this.info.deleting = null;
    const ev = this.ev;
    this.later(60, () => ev.deleteStatus({ ...d.status, state: "refused", code: "canceled" }));
  }

  applied(_wireBytes: number): void {}

  takeOver(): void {
    if (!this.ev) return;
    this.ev.state({ s: "handshaking" });
    this.later(400, () => {
      this.ev?.state({ s: "connected", code: "617 240" });
      this.ev?.agentInfo({ ...this.info, scan: this.gen ? { id: this.scanId, state: "done" } : null });
    });
  }

  close(): void {
    this.closed = true;
    if (this.del) window.clearTimeout(this.del.timer);
    this.del = null;
    this.stopScan();
    for (const t of this.timers) window.clearTimeout(t);
    this.timers = [];
    this.gen = null;
  }

  /** current state for previews */
  static stateLabel(s: ConnState): string {
    return s.s;
  }
}

/** per-dir subtree totals of a generated tree (child ids are always above their parent's) */
function subtreeTotals(g: Gen): { alloc: Float64Array; apparent: Float64Array; files: Float64Array; dirs: Float64Array } {
  const n = g.nextId;
  const alloc = g.ownAlloc.slice(0, n), apparent = g.ownApparent.slice(0, n), files = g.ownFiles.slice(0, n);
  const dirs = new Float64Array(n);
  for (let id = n - 1; id > 0; id--) {
    const p = g.parent[id];
    alloc[p] += alloc[id];
    apparent[p] += apparent[id];
    files[p] += files[id];
    dirs[p] += dirs[id] + 1;
  }
  return { alloc, apparent, files, dirs };
}

function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}

/** synchronous full generation (tests, benchmarks): records in stream order */
export function generateTree(targetDirs: number, opts: { root?: "home" | "volume"; seed?: number; fdaDenied?: boolean } = {}): { records: NodeRecord[]; totals: { dirs: number; files: number; alloc: number; apparent: number }; denied: number } {
  const gen = new Gen(targetDirs, opts.root === "volume" ? K.VolRoot : K.Home, opts.root === "volume" ? "Macintosh HD" : "alex", opts.fdaDenied ?? true, opts.seed ?? 7);
  const records: NodeRecord[] = [];
  while (!gen.done) gen.emit(2000, records);
  return { records, totals: gen.totals, denied: gen.denied };
}
