// SPDX-License-Identifier: MIT
//
// QR code as one SVG path (light modules on the petrol plane). Built from the
// raw module matrix so the launcher controls the look, not the library.

import QRCode from "qrcode";

export interface QRPath {
  size: number; // modules per side
  d: string;
}

export function qrPath(text: string): QRPath {
  const m = QRCode.create(text, { errorCorrectionLevel: "M" }).modules;
  const n = m.size;
  let d = "";
  for (let r = 0; r < n; r++) {
    // merge horizontal runs into one rect each
    let c = 0;
    while (c < n) {
      if (!m.get(r, c)) {
        c++;
        continue;
      }
      const start = c;
      while (c < n && m.get(r, c)) c++;
      d += `M${start} ${r}h${c - start}v1h${start - c}z`;
    }
  }
  return { size: n, d };
}
