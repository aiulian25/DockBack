import { describe, it, expect } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import { CloudUpload } from "lucide-react";
import BackupCloudIcon from "./BackupCloudIcon";

function pathData(markup: string): string[] {
  const holder = document.createElement("div");
  holder.innerHTML = markup;
  return [...holder.querySelectorAll("path")].map((path) => path.getAttribute("d") ?? "").sort();
}

describe("BackupCloudIcon", () => {
  // Idle backup buttons still use lucide's CloudUpload, often on the same page
  // as a busy one. If the two drifted apart — a lucide upgrade redrawing the
  // icon, say — the cloud would visibly change shape the moment a backup
  // started.
  it("is drawn with exactly lucide's CloudUpload geometry", () => {
    const ours = pathData(renderToStaticMarkup(<BackupCloudIcon />));
    const lucides = pathData(renderToStaticMarkup(<CloudUpload />));
    expect(ours).toEqual(lucides);
  });

  it("animates only while active", () => {
    expect(renderToStaticMarkup(<BackupCloudIcon active />)).toContain("backup-cloud-active");
    expect(renderToStaticMarkup(<BackupCloudIcon />)).not.toContain("backup-cloud-active");
  });

  // The animation moves the arrow as one piece; the cloud must stay put.
  it("groups the arrow apart from the cloud", () => {
    const holder = document.createElement("div");
    holder.innerHTML = renderToStaticMarkup(<BackupCloudIcon active />);
    const arrow = holder.querySelector(".backup-cloud-arrow");
    expect(arrow?.querySelectorAll("path")).toHaveLength(2);
    expect(holder.querySelectorAll("svg > path")).toHaveLength(1);
  });

  it("keeps the size and extra classes it is given", () => {
    const markup = renderToStaticMarkup(<BackupCloudIcon size={14} className="shrink-0" />);
    expect(markup).toContain('width="14"');
    expect(markup).toContain("shrink-0");
  });
});
