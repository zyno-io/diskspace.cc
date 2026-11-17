// SPDX-License-Identifier: MIT

const base = { width: 14, height: 14, viewBox: "0 0 16 16", fill: "none", stroke: "currentColor", "stroke-width": 1.6, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true" } as const;

export const Chevron = () => (
  <svg {...base} width={12} height={12}>
    <path d="M6 3.5 10.5 8 6 12.5" />
  </svg>
);
export const Back = () => (
  <svg {...base}>
    <path d="M10 3.5 5.5 8l4.5 4.5" />
  </svg>
);
export const Folder = () => (
  <svg {...base}>
    <path d="M2 4.5c0-.6.4-1 1-1h3.2l1.5 1.6H13c.6 0 1 .4 1 1v6.4c0 .6-.4 1-1 1H3c-.6 0-1-.4-1-1z" />
  </svg>
);
export const Lock = () => (
  <svg {...base} width={12} height={12} class="lock">
    <rect x="3" y="7" width="10" height="7" rx="1.5" />
    <path d="M5.5 7V5a2.5 2.5 0 0 1 5 0v2" />
  </svg>
);
export const Copy = () => (
  <svg {...base}>
    <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" />
    <path d="M3.5 10.5h-.2c-.7 0-1.3-.6-1.3-1.3V3.3C2 2.6 2.6 2 3.3 2h5.9c.7 0 1.3.6 1.3 1.3v.2" />
  </svg>
);
export const Check = () => (
  <svg {...base}>
    <path d="m3 8.5 3.2 3L13 4.5" />
  </svg>
);
export const Download = () => (
  <svg {...base}>
    <path d="M8 2.5v8M4.5 7 8 10.5 11.5 7M3 13.5h10" />
  </svg>
);
export const Pause = () => (
  <svg {...base}>
    <path d="M5.5 3.5v9M10.5 3.5v9" />
  </svg>
);
export const Play = () => (
  <svg {...base}>
    <path d="M5 3.2v9.6L12.6 8z" />
  </svg>
);
export const Close = () => (
  <svg {...base} width={12} height={12}>
    <path d="m4 4 8 8M12 4l-8 8" />
  </svg>
);
