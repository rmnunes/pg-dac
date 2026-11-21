import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// `tauri build` refuses to bundle when a @tauri-apps/* package and its Rust
// crate differ in major/minor. Only the release workflow runs that build, so
// this check keeps the mismatch from surfacing for the first time at release.

const ui = fileURLToPath(new URL("..", import.meta.url));
const read = (path: string) => readFileSync(ui + path, "utf8");

function crateVersions(): Map<string, string> {
  const versions = new Map<string, string>();
  for (const block of read("src-tauri/Cargo.lock").split("[[package]]")) {
    const name = /^name = "(.+)"$/m.exec(block)?.[1];
    const version = /^version = "(.+)"$/m.exec(block)?.[1];
    if (name && version) versions.set(name, version);
  }
  return versions;
}

// @tauri-apps/api pairs with the tauri crate, @tauri-apps/plugin-x with tauri-plugin-x.
function crateFor(pkg: string): string | undefined {
  if (pkg === "@tauri-apps/api") return "tauri";
  const plugin = /^@tauri-apps\/plugin-(.+)$/.exec(pkg)?.[1];
  return plugin && `tauri-plugin-${plugin}`;
}

const minor = (version: string) => version.split(".").slice(0, 2).join(".");

describe("tauri package versions", () => {
  it("npm packages match their Rust crates on major.minor", () => {
    const manifest = JSON.parse(read("package.json"));
    const crates = crateVersions();
    const pairs = Object.keys(manifest.dependencies ?? {})
      .map((pkg) => ({ pkg, crate: crateFor(pkg) }))
      .filter((p): p is { pkg: string; crate: string } => p.crate !== undefined);
    expect(pairs.length).toBeGreaterThan(0);
    for (const { pkg, crate } of pairs) {
      const installed = JSON.parse(read(`node_modules/${pkg}/package.json`)).version;
      const locked = crates.get(crate);
      expect(locked, `${crate} is not in Cargo.lock`).toBeDefined();
      expect(minor(installed), `${pkg} ${installed} vs ${crate} ${locked}`).toBe(minor(locked!));
    }
  });
});
