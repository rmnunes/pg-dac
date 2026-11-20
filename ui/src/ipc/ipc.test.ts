import { describe, expect, it } from "vitest";

import pinned from "../../PGDAC_VERSION?raw";
import { engineRelease } from "./types";
import { effectivePlan, mock } from "./mock";

describe("engineRelease", () => {
  it("strips the dev suffix the sidecar build stamps, nothing else", () => {
    expect(engineRelease("0.9.0")).toBe("0.9.0");
    expect(engineRelease("0.9.0-dev")).toBe("0.9.0");
    expect(engineRelease("0.9.0-dev+1cf4a15")).toBe("0.9.0");
    expect(engineRelease("0.9.0-rc.1")).toBe("0.9.0-rc.1");
  });

  it("matches the pin for the mock engine, as for a dev sidecar", async () => {
    const h = await mock.health();
    expect(engineRelease(h.version)).toBe(pinned.trim());
  });
});

describe("mock effectivePlan", () => {
  const all = effectivePlan();
  const regions = 'table:"sales"."regions"';
  const view = 'view:"sales"."v_region_contacts"';

  it("recomputes counts and the hazard summary for the selection", () => {
    const dataLoss = all.changes.filter((c) => c.hazards?.some((h) => h.type === "DELETES_DATA")).map((c) => c.id);
    expect(all.dataLoss).toBe(true);
    const without = effectivePlan(dataLoss);
    expect(without.dataLoss).toBe(false);
    expect(without.hazardSummary?.some((h) => h.code === "DELETES_DATA")).toBe(false);
    expect(without.statementCount).toBe(without.changes.reduce((n, c) => n + c.statements.length, 0));
  });

  it("keeps an excluded dependent the engine must recreate while its dependency runs", () => {
    expect(effectivePlan([view]).changes.some((c) => c.id === view)).toBe(true);
    expect(effectivePlan([view, regions]).changes.some((c) => c.id === view)).toBe(false);
  });

  it("refuses a re-plan of sources that were not compared, like the daemon", async () => {
    await expect(mock.replan({ from: "a", to: "b" })).rejects.toMatch(/^daemon returned 409/);
  });
});
