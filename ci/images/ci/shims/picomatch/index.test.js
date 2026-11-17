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
