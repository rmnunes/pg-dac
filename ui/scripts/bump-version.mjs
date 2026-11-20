#!/usr/bin/env node
// Sets the release version everywhere it has to match (the release workflow
// checks them against the tag):
//   - ui/PGDAC_VERSION                (the version the engine and the UI pin)
//   - ui/package.json
//   - ui/src-tauri/tauri.conf.json    (what the updater compares)
//   - ui/src-tauri/Cargo.toml         ([package].version) and Cargo.lock
//   - cmd/pg-dac/main.go              (the CLI's default "<version>-dev"; release
//                                      builds stamp the exact version)
// cmd/pg-dac/version_test.go (TestVersionsAgree) fails when they disagree.
//
// Usage (from ui/): pnpm release <patch|minor|major|x.y.z>
// Then review the diff, add the CHANGELOG section, commit, tag and push the tag
// by name (docs/UPDATES.md).

import { execSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ui = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const at = (p) => resolve(ui, p);

const arg = process.argv[2];
if (!arg) {
  console.error("usage: pnpm release <patch|minor|major|x.y.z>");
  process.exit(1);
}

const current = readFileSync(at("PGDAC_VERSION"), "utf8").trim();
const next = computeNext(current, arg);
console.log(`${current} -> ${next}`);

// Prepare every change first, so a failure leaves no file half-updated.
const versionLine = /^version\s*=\s*"[^"]+"/m;
const cargoToml = readFileSync(at("src-tauri/Cargo.toml"), "utf8");
if (!versionLine.test(cargoToml)) {
  console.error("no [package].version line found in src-tauri/Cargo.toml");
  process.exit(1);
}
const goVersion = /^var version = "[^"]+"/m;
const mainGo = readFileSync(at("../cmd/pg-dac/main.go"), "utf8");
if (!goVersion.test(mainGo)) {
  console.error('no `var version = "..."` line found in cmd/pg-dac/main.go');
  process.exit(1);
}
const writes = [
  ["PGDAC_VERSION", next + "\n"],
  ["src-tauri/Cargo.toml", cargoToml.replace(versionLine, `version = "${next}"`)],
  ["../cmd/pg-dac/main.go", mainGo.replace(goVersion, `var version = "${next}-dev"`)],
];
for (const file of ["package.json", "src-tauri/tauri.conf.json"]) {
  const json = JSON.parse(readFileSync(at(file), "utf8"));
  json.version = next;
  writes.push([file, JSON.stringify(json, null, 2) + "\n"]);
}
for (const [file, content] of writes) writeFileSync(at(file), content);

console.log("updating Cargo.lock...");
execSync("cargo update -w --offline", { cwd: at("src-tauri"), stdio: "inherit" });

console.log(`
done. Review the diff and add the CHANGELOG section, then:
  git commit -am "Release v${next}"
  git tag v${next}
  git push origin main v${next}`);

function computeNext(curr, bump) {
  if (/^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/.test(bump)) return bump;
  const [maj, min, pat] = curr.split("-")[0].split(".").map(Number);
  if (bump === "patch") return `${maj}.${min}.${pat + 1}`;
  if (bump === "minor") return `${maj}.${min + 1}.0`;
  if (bump === "major") return `${maj + 1}.0.0`;
  console.error(`unknown bump: ${bump} (patch, minor, major or x.y.z)`);
  process.exit(1);
}
