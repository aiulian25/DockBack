import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, Root } from "react-dom/client";
import NotificationsCard from "./NotificationsCard";
import { api, emptyNotifyConfig, NotifyConfigResponse } from "../api";

// The global Save bar needs a data router and plays no part in removing a URL.
vi.mock("./SettingsSave", () => ({ useRegisterSaver: () => {} }));

const REMOVE_LABEL = "Remove saved URL";
const SAVED_HOST = "hooks.example.test";

function storedView(webhookSet: boolean): NotifyConfigResponse {
  return {
    config: { ...emptyNotifyConfig, webhook: { ...emptyNotifyConfig.webhook, enabled: true, url: "" } },
    gotify_token_set: false,
    email_password_set: false,
    webhook_url_set: webhookSet,
    webhook_url_host: webhookSet ? SAVED_HOST : "",
    heartbeat_url_set: false,
    heartbeat_url_host: "",
  };
}

describe("NotificationsCard saved URLs", () => {
  let host: HTMLDivElement;
  let root: Root;

  const renderCard = async () => {
    await act(async () => { root.render(<NotificationsCard />); });
  };
  const removeButton = () =>
    Array.from(host.querySelectorAll("button")).find((b) => b.textContent === REMOVE_LABEL);

  beforeEach(() => {
    (globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
    vi.spyOn(api, "getNotifications").mockResolvedValue(storedView(true));
    host = document.createElement("div");
    document.body.appendChild(host);
    root = createRoot(host);
  });
  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    vi.restoreAllMocks();
  });

  it("names the host of a saved URL and leaves the field empty", async () => {
    await renderCard();
    expect(host.textContent).toContain(`Saved URL for ${SAVED_HOST}`);
    const values = Array.from(host.querySelectorAll("input")).map((i) => i.value);
    expect(values.filter((v) => v.includes(SAVED_HOST))).toEqual([]);
  });

  it("removes the saved URL only when asked, then stops offering it", async () => {
    const save = vi.spyOn(api, "setNotifications").mockResolvedValue(storedView(false));
    await renderCard();
    expect(save).not.toHaveBeenCalled();
    await act(async () => { removeButton()?.click(); });
    expect(save).toHaveBeenCalledTimes(1);
    expect(save.mock.calls[0][1]).toEqual(["webhook_url"]);
    expect(removeButton()).toBeUndefined();
  });
});
