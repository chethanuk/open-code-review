#!/usr/bin/env node
"use strict";

// Inline the two pure functions under test so we don't need a test framework.
const SEMVER_RE = /^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/;

function semverGt(a, b) {
  const pa = a.replace(/-.*$/, "").split(".").map(Number);
  const pb = b.replace(/-.*$/, "").split(".").map(Number);
  for (let i = 0; i < 3; i++) {
    if ((pa[i] || 0) > (pb[i] || 0)) return true;
    if ((pa[i] || 0) < (pb[i] || 0)) return false;
  }
  const aPre = a.includes("-");
  const bPre = b.includes("-");
  if (bPre && !aPre) return true;
  return false;
}

function normalizeVersion(v) {
  return v && v.startsWith("v") ? v.slice(1) : v;
}

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { console.log(`  ok: ${msg}`); passed++; }
  else       { console.error(`FAIL: ${msg}`); failed++; }
}

// --- semverGt ---
assert(!semverGt("1.8.6", "1.8.6"), "equal versions: not gt");
assert( semverGt("1.8.7", "1.8.6"), "patch bump: gt");
assert(!semverGt("1.8.5", "1.8.6"), "older: not gt");
assert( semverGt("2.0.0", "1.9.9"), "major bump: gt");
assert( semverGt("1.8.6", "1.8.6-beta"), "stable > pre-release: gt");
assert( semverGt("1.8.6", "1.8.6-rc1"), "stable > rc: gt");

// --- normalizeVersion (the v-prefix fix) ---
assert(normalizeVersion("v1.8.6") === "1.8.6", "strips leading v");
assert(normalizeVersion("1.8.6")  === "1.8.6", "leaves bare version alone");
assert(normalizeVersion("")       === "",       "empty string unchanged");

// --- equal after normalize => no nudge ---
const latestRaw = "1.8.6";   // npm registry style
const installed = "1.8.6";   // from binary
const latest = normalizeVersion(latestRaw);
assert(SEMVER_RE.test(latest),         "normalized version passes SEMVER_RE");
assert(!semverGt(latest, installed),   "equal versions: no nudge");

// --- v-prefixed registry response handled correctly ---
const latestRawV = "v1.8.6";
const latestNorm = normalizeVersion(latestRawV);
assert(SEMVER_RE.test(latestNorm),     "v-prefixed from registry normalizes and passes SEMVER_RE");
assert(!semverGt(latestNorm, "1.8.6"), "v-prefixed equal: no nudge");
assert( semverGt(latestNorm, "1.8.5"), "v-prefixed newer: nudge shown");

// --- hint display guard (ocr.js logic) ---
function shouldShowNudge(hintVersion, pkgVersion) {
  const hintNorm = normalizeVersion(hintVersion);
  const pkgNorm  = normalizeVersion(pkgVersion);
  return !pkgNorm || hintNorm !== pkgNorm;
}
assert(!shouldShowNudge("1.8.6", "1.8.6"),  "same bare: no nudge");
assert(!shouldShowNudge("v1.8.6", "1.8.6"), "v-hint vs bare: no nudge");
assert(!shouldShowNudge("1.8.6", "v1.8.6"), "bare hint vs v-pkg: no nudge");
assert( shouldShowNudge("1.8.7", "1.8.6"),  "newer hint: show nudge");
assert( shouldShowNudge("1.8.6", ""),        "unknown pkg version: show nudge (safe default)");

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
