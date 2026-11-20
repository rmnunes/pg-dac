import { beforeEach, describe, expect, it } from "vitest";

import { mock } from "../ipc/mock";
import { useUpdates } from "./update";
import pinned from "../../PGDAC_VERSION?raw";

const phase = () => useUpdates.getState().phase;

describe("update checks", () => {
  beforeEach(() => {
    mock.offerUpdate(null);
    useUpdates.setState({ phase: { kind: "idle" }, snoozed: undefined });
  });

  it("stays quiet when an automatic check finds nothing, and says so after a manual one", async () => {
    await useUpdates.getState().check(false);
    expect(phase().kind).toBe("idle");
    await useUpdates.getState().check(true);
    expect(phase().kind).toBe("uptodate");
  });

  it("offers a newer version, and a postponed one only on a manual check", async () => {
    mock.offerUpdate("99.0.0");
    await useUpdates.getState().check(false);
    expect(phase()).toMatchObject({ kind: "available", info: { version: "99.0.0", currentVersion: pinned.trim() } });

    useUpdates.getState().dismiss();
    expect(useUpdates.getState().snoozed).toBe("99.0.0");
    await useUpdates.getState().check(false);
    expect(phase().kind).toBe("idle");
    await useUpdates.getState().check(true);
    expect(phase().kind).toBe("available");
  });

  it("offers a version newer than the postponed one", async () => {
    useUpdates.setState({ snoozed: "99.0.0" });
    mock.offerUpdate("99.0.1");
    await useUpdates.getState().check(false);
    expect(phase()).toMatchObject({ kind: "available", info: { version: "99.0.1" } });
  });

  it("downloads with progress and becomes ready to restart", async () => {
    mock.offerUpdate("99.0.0");
    await useUpdates.getState().check(true);
    const seen: number[] = [];
    const unsub = useUpdates.subscribe((s) => {
      if (s.phase.kind === "downloading") seen.push(s.phase.received);
    });
    await useUpdates.getState().download();
    unsub();
    expect(phase()).toMatchObject({ kind: "ready", info: { version: "99.0.0" } });
    expect(seen.length).toBeGreaterThan(2);
    expect(seen).toEqual([...seen].sort((a, b) => a - b));
  }, 10_000);

  it("never lets a check replace an update being acted on", async () => {
    mock.offerUpdate("99.0.0");
    await useUpdates.getState().check(true);
    await useUpdates.getState().download();
    mock.offerUpdate(null);
    await useUpdates.getState().check(true);
    expect(phase().kind).toBe("ready");
  }, 10_000);
});
