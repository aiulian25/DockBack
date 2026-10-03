// NodeAvatar / NodeLogo render the DockBack mark used to represent a node across
// the app (dashboard cards, Servers table, Backups headers) so branding stays
// consistent and the asset lives in exactly one place. The mark is served from
// /dockback-mark.png (see also VersionModal). It has a transparent background,
// so it sits cleanly on the dark "surface-low" well without extra chrome.

// NodeLogo is the bare mark at a given pixel size — for inline/compact spots
// (e.g. a dense table cell) where a boxed avatar would be too heavy.
export function NodeLogo({ size = 20, className = "" }: { size?: number; className?: string }) {
  return (
    <img
      src="/dockback-mark.png"
      alt=""
      style={{ width: size, height: size }}
      className={`shrink-0 object-contain ${className}`}
      draggable={false}
    />
  );
}

// NodeAvatar is the boxed treatment: the mark centered in a bordered
// surface-low tile, matching the node card header. size is the box edge in px;
// the mark scales proportionally inside it.
export function NodeAvatar({ size = 40, className = "" }: { size?: number; className?: string }) {
  return (
    <div
      style={{ width: size, height: size }}
      className={`grid shrink-0 place-items-center rounded-[10px] border border-outline-variant bg-surface-low ${className}`}
    >
      <NodeLogo size={Math.round(size * 0.67)} />
    </div>
  );
}
