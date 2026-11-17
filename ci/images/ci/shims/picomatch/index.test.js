"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");
const picomatch = require("./index.js");

test("matches extglob-free globs like picomatch", () => {
  const cases = [
    ["**/*.md", "docs/site/docs/index.md", true],
    ["**/*.md", "README.md", true],
    ["**/*.md", "README.txt", false],
    ["node_modules/**", "node_modules/a/b.md", true],
    ["docs/*/site/**", "docs/site/site/x.md", true],
    ["docs/*/site/**", "docs/site/docs/x.md", false],
    [".claude/**", ".claude/agents/x.md", true],
    ["a?c.md", "abc.md", true],
    ["[ab].md", "c.md", false],
    ["(a|b).md", "b.md", true],
  ];
  for (const [glob, path, want] of cases) {
    assert.equal(picomatch.isMatch(path, glob), want, `${glob} vs ${path}`);
  }
});

test("rejects every extglob", () => {
  for (const glob of ["+(a).md", "*(a|b)", "?(x)", "@(a|b)", "!(a).md", "docs/**/+(a|aa)"]) {
    assert.throws(() => picomatch.makeRe(glob), /GHSA-fc5q-39xc-vjpf/, glob);
  }
});

test("keeps extglob syntax literal when the caller disables extglobs", () => {
  assert.equal(picomatch.isMatch("+(a).md", "+(a).md", { noext: true }), true);
  assert.equal(picomatch.isMatch("a.md", "+(a).md", { noextglob: true }), false);
});

test("removeBackslashes matches upstream's regex and stays linear", () => {
  const { removeBackslashes } = require("./lib/utils.js");
  const { REGEX_REMOVE_BACKSLASH } = require("./lib/constants.js");
  const upstream = (str) => str.replace(REGEX_REMOVE_BACKSLASH, (m) => (m === "\\" ? "" : m));
  const alphabet = ["[", "]", "\\", "a", "\n", " ", "*", "/"];
  let seed = 42;
  const next = () => (seed = (seed * 1103515245 + 12345) % 2147483648);
  for (let round = 0; round < 50000; round++) {
    let str = "";
    for (let len = next() % 12; len > 0; len--) str += alphabet[next() % alphabet.length];
    assert.equal(removeBackslashes(str), upstream(str), JSON.stringify(str));
  }
  const started = Date.now();
  removeBackslashes("[".repeat(200000));
  assert.ok(Date.now() - started < 1000, "200k '[' must not backtrack");
});
