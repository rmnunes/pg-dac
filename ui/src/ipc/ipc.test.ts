import { describe, expect, it } from "vitest";

import pinned from "../../PGDAC_VERSION?raw";
import { engineRelease } from "./types";
import { effectivePlan, mock, withoutPassword } from "./mock";

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

describe("saved passwords", () => {
  it("withoutPassword mirrors the desktop app's split (src-tauri/src/dsn.rs)", () => {
    const cases: [string, string | null][] = [
      ["postgres://app:s3cret@db.example:5432/app?sslmode=require", "postgres://app@db.example:5432/app?sslmode=require"],
      ["postgres://app:a@b@host/db", "postgres://app@host/db"],
      ["postgres://app@host/db?password=x&sslmode=require", "postgres://app@host/db?sslmode=require"],
      ["postgres://app@host/db?sslmode=require&password=x", "postgres://app@host/db?sslmode=require"],
      ["host=db port=5432 user=app password=s3cret dbname=app", "host=db port=5432 user=app dbname=app"],
      [String.raw`host=db password = 'it\'s a secret' dbname=app`, "host=db dbname=app"],
      ["postgres://app@host/db", null],
      ["host=db passfile=/home/app/.pgpass", null],
    ];
    for (const [dsn, want] of cases) expect(withoutPassword(dsn), dsn).toBe(want);
  });

  it("the mock, like the desktop app, keeps a saved password out of the profile list", async () => {
    const list = await mock.addProfile({ id: "t-secret", name: "secret", kind: "db", value: "postgres://app:s3cret@db.example/app" });
    const saved = list.find((p) => p.id === "t-secret");
    expect(saved).toMatchObject({ value: "postgres://app@db.example/app", hasSecret: true });
    expect(JSON.stringify(await mock.listProfiles())).not.toContain("s3cret");
    await mock.removeProfile("t-secret");
  });
});
