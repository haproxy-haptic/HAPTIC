# Documentation checks

Run the Python packaging and search checks from the repository root:

```bash
make test-agent-skill-package
```

The search check builds a Material site with generated ADRs and verifies that
Development pages remain readable without appearing in the user search index.

The required docs CI job runs the embedded-playground browser regressions.
Website publication runs them again in `make test-github-pages-browser`. To run them locally:

```bash
npm ci --prefix scripts/visual-qa
cd scripts/visual-qa
npx playwright install chromium
cd ../..
node --test docs/site/tests/playground-embed.mjs
```

The browser suite uses the shared embed script and styles with a local frame
fixture and the real playground shell. It checks viewport layout, keyboard navigation, edit preservation,
solution comparison, input switching, load failures, output escaping, and cleanup during page navigation. It
doesn't test template rendering; use a built playground bundle to check that.
