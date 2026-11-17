// SPDX-License-Identifier: MIT
//
// Mineral parcel palette. Everything is precomputed into CSS colour strings so
// the draw loop only assigns existing string references (no per-frame allocs).

export const PETROL0 = "#0D2A31";
export const PETROL1 = "#123C3E";
export const CONTOUR = "#2B5A58";
export const FOAM = "#E9F2EE";
export const MIST = "#9DB9B3";
export const SODIUM = "#F3B73F";

/** top-level branch hues, cycled in this order */
export const HUES = ["#D9A441", "#9DBA6E", "#74BFCF", "#4FA98F", "#DCD5C3", "#D99AAE", "#6F93B8", "#C48D5C"];
export const HUE_NAMES = ["ochre", "lichen", "glacier", "malachite", "quartz", "rose-quartz", "basalt-blue", "sandstone"];

/** file categories → hue index into HUES */
export const CAT_VIDEO = 5, CAT_IMAGE = 2, CAT_AUDIO = 3, CAT_ARCHIVE = 0, CAT_CODE = 1, CAT_DOC = 4, CAT_APP = 7, CAT_OTHER = 6;

const EXT: Record<string, number> = {};
const add = (cat: number, exts: string) => exts.split(" ").forEach((e) => (EXT[e] = cat));
add(CAT_VIDEO, "mov mp4 m4v mkv avi webm wmv flv mpg mpeg mts m2ts 3gp braw r3d prores");
add(CAT_IMAGE, "jpg jpeg png gif heic heif webp tif tiff bmp raw cr2 cr3 nef arw dng psd psb svg ico icns exr");
add(CAT_AUDIO, "mp3 m4a aac wav aif aiff flac ogg opus caf alac mid logicx band");
add(CAT_ARCHIVE, "zip tar gz tgz bz2 xz zst 7z rar dmg iso img vmdk vdi qcow2 vhd vhdx raw sparseimage sparsebundle pkg xip ipsw cab wim");
add(CAT_CODE, "js mjs cjs ts tsx jsx json map go rs c h cc cpp hpp py rb java kt swift m mm o a so dylib lib pdb class jar wasm node pyc sqlite db sqlite3 lock log pack idx");
add(CAT_DOC, "pdf doc docx xls xlsx ppt pptx key pages numbers txt md rtf csv epub mobi odt ods");
add(CAT_APP, "app exe msi dll bin appimage deb rpm apk ipa");

export function fileCategory(name: string): number {
  const dot = name.lastIndexOf(".");
  if (dot <= 0 || dot === name.length - 1) return CAT_OTHER;
  const c = EXT[name.slice(dot + 1).toLowerCase()];
  return c === undefined ? CAT_OTHER : c;
}

type RGB = [number, number, number];
const hex = (h: string): RGB => [parseInt(h.slice(1, 3), 16), parseInt(h.slice(3, 5), 16), parseInt(h.slice(5, 7), 16)];
const mix = (a: RGB, b: RGB, t: number): RGB => [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t];
const desat = (c: RGB, t: number): RGB => {
  const l = 0.299 * c[0] + 0.587 * c[1] + 0.114 * c[2];
  return mix(c, [l, l, l], t);
};
const css = (c: RGB, a = 1): string =>
  a >= 1
    ? `rgb(${Math.round(c[0])},${Math.round(c[1])},${Math.round(c[2])})`
    : `rgba(${Math.round(c[0])},${Math.round(c[1])},${Math.round(c[2])},${a})`;

const FOAM_RGB = hex(FOAM);
const PETROL_RGB = hex(PETROL0);
export const MAX_DEPTH = 14;

/** depth-shaded hue: each level ~6% toward foam, slightly less saturated */
export function shadeRGB(hue: number, depth: number): RGB {
  const d = Math.min(depth, 10);
  return desat(mix(hex(HUES[hue]), FOAM_RGB, d * 0.06), d * 0.035);
}

export interface Palette {
  /** filled leaf (file, collapsed dir) */
  leaf: string[][];
  /** directory header strip */
  header: string[][];
  /** directory body under its children (lot-line tint) */
  body: string[][];
  /** dark under-fill for hatched bucket parcels */
  bucketBase: string[][];
  /** hatch line colour */
  hatch: string[];
  /** text on a parcel */
  ink: string;
  inkSoft: string;
  inkLight: string;
  inkLightSoft: string;
  /** remainder of too-small-to-draw children */
  rest: string[][];
}

function build(): Palette {
  const p: Palette = { leaf: [], header: [], body: [], bucketBase: [], hatch: [], rest: [], ink: "rgba(9,30,35,0.92)", inkSoft: "rgba(9,30,35,0.62)", inkLight: "rgba(233,242,238,0.94)", inkLightSoft: "rgba(233,242,238,0.66)" };
  for (let h = 0; h < HUES.length; h++) {
    p.leaf.push([]);
    p.header.push([]);
    p.body.push([]);
    p.bucketBase.push([]);
    p.rest.push([]);
    for (let d = 0; d <= MAX_DEPTH; d++) {
      const c = shadeRGB(h, d);
      p.leaf[h].push(css(mix(c, PETROL_RGB, 0.08)));
      p.header[h].push(css(mix(c, PETROL_RGB, 0.3)));
      p.body[h].push(css(mix(c, PETROL_RGB, 0.82)));
      p.bucketBase[h].push(css(mix(c, PETROL_RGB, 0.62)));
      p.rest[h].push(css(mix(c, PETROL_RGB, 0.5)));
    }
    p.hatch.push(css(mix(hex(HUES[h]), FOAM_RGB, 0.1), 0.55));
  }
  return p;
}

export const PALETTE = build();

/** css colour of a hue at a depth (list bars, legend) */
export function hueCss(hue: number, depth = 0): string {
  return PALETTE.leaf[hue % HUES.length][Math.min(depth, MAX_DEPTH)];
}

/** canvas pattern cache for hatched parcels (per context) */
export function hatchPatterns(ctx: CanvasRenderingContext2D, dpr: number): CanvasPattern[] {
  const out: CanvasPattern[] = [];
  const s = Math.max(6, Math.round(6 * dpr));
  for (let h = 0; h < HUES.length; h++) {
    const c = document.createElement("canvas");
    c.width = c.height = s;
    const g = c.getContext("2d")!;
    g.strokeStyle = PALETTE.hatch[h];
    g.lineWidth = Math.max(1, dpr);
    g.beginPath();
    // 45° lines that tile seamlessly
    g.moveTo(-1, s + 1);
    g.lineTo(s + 1, -1);
    g.moveTo(-1, 1);
    g.lineTo(1, -1);
    g.moveTo(s - 1, s + 1);
    g.lineTo(s + 1, s - 1);
    g.stroke();
    const pat = ctx.createPattern(c, "repeat")!;
    // patterns are in device pixels; undo the DPR transform so lines stay crisp
    if (pat.setTransform && dpr !== 1) pat.setTransform(new DOMMatrix().scale(1 / dpr));
    out.push(pat);
  }
  return out;
}
