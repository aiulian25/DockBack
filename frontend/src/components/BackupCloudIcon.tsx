// Lucide's CloudUpload geometry, kept identical so the icon at rest is the one
// the app has always shown.
const CLOUD_PATH = "M4 14.899A7 7 0 1 1 15.71 8h1.79a4.5 4.5 0 0 1 2.5 8.242";
const ARROW_SHAFT_PATH = "M12 13v8";
const ARROW_HEAD_PATH = "m8 17 4-4 4 4";

/**
 * BackupCloudIcon is the app's "backup" cloud, drawn here rather than taken from
 * lucide so its arrow can move on its own.
 *
 * While `active`, the arrow keeps lifting into the cloud: the one sign, used on
 * every page, that a backup is underway. At rest it is pixel-identical to
 * lucide's CloudUpload. The animation lives in index.css and stops for anyone
 * whose system asks for reduced motion.
 */
export default function BackupCloudIcon({ active = false, size = 24, className = "" }: {
  active?: boolean;
  size?: number;
  className?: string;
}) {
  const activeClass = active ? "backup-cloud-active" : "";
  return (
    <svg
      xmlns="http://www.w3.org/2000/svg" width={size} height={size} viewBox="0 0 24 24"
      fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round"
      className={`${activeClass} ${className}`} aria-hidden="true"
    >
      <path d={CLOUD_PATH} />
      <g className="backup-cloud-arrow">
        <path d={ARROW_SHAFT_PATH} />
        <path d={ARROW_HEAD_PATH} />
      </g>
    </svg>
  );
}
