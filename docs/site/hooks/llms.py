"""Agent-readable docs: per-page Markdown endpoints + llms.txt.

AI agents are a large share of docs traffic and parse Markdown far more
reliably than themed HTML. This hook publishes, at build time:

- ``<page-url>index.md`` — each page's final Markdown next to its HTML
- ``llms.txt`` — a site map for agents (https://llmstxt.org/) linking the
  Markdown endpoints
- ``llms-full.txt`` — every page's Markdown concatenated into one document

The Markdown is captured after earlier hooks ran (e.g. the generated
changelog), so the endpoints can't drift from the rendered pages.
"""

import re
from pathlib import Path

_pages = []  # (page, markdown) in build order


def on_pre_build(config):
    _pages.clear()  # mkdocs serve rebuilds in-process


def on_page_markdown(markdown, page, config, files):
    _pages.append((page, markdown))
    return markdown


def _summary(markdown):
    """First plain-prose line, de-markup'd, as the llms.txt description."""
    for line in markdown.splitlines():
        line = line.strip()
        if line and not line.startswith(("#", "<", "!", "-", "|", "`", ":", "=")):
            return re.sub(r"[*_`]|\[|\]\([^)]*\)", "", line)[:160]
    return ""


def on_post_build(config):
    if not config["use_directory_urls"]:
        # page.url would be "page.html", colliding with the emitted HTML file.
        raise RuntimeError("hooks/llms.py requires use_directory_urls: true")
    site = Path(config["site_dir"])
    # Relative links follow the version of the index the reader opened.
    # The HTML canonical URL can point to a different release.
    toc = [
        "# " + config["site_name"],
        "",
        "> " + config["site_description"],
        "",
        "Every page below is also served as raw Markdown at its URL plus"
        " `index.md`.",
        "",
        "## Agent skill",
        "",
        "- [HAPTIC skill](agent-skills/haptic/SKILL.md): "
        "Workflow for Scriggo customization, resource access, and validation.",
        "- [Install the skill](development/agent-skill/index.md): "
        "Installation, downloads, and example prompts.",
        "",
        "## Documentation",
        "",
    ]
    full = []
    for page, markdown in _pages:
        out = site / page.url / "index.md"
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_text(markdown, encoding="utf-8")
        desc = page.meta.get("description") or _summary(markdown)
        toc.append(
            f"- [{page.title}](./{page.url}index.md)"
            + (f": {desc}" if desc else "")
        )
        full.append(f"# {page.title}\n\nSource: ./{page.url}\n\n{markdown}")
    (site / "llms.txt").write_text("\n".join(toc) + "\n", encoding="utf-8")
    (site / "llms-full.txt").write_text(
        "\n\n---\n\n".join(full) + "\n", encoding="utf-8"
    )
