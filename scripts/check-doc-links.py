#!/usr/bin/env python3
"""Validate that relative links in repository Markdown files exist."""

from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit


ROOT = Path(__file__).resolve().parent.parent
LINK = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")
SKIP_SCHEMES = {"http", "https", "mailto"}


def local_target(markdown: Path, raw_target: str) -> Path | None:
    target = raw_target.strip().split(maxsplit=1)[0].strip("<>")
    if not target or target.startswith("#"):
        return None

    parsed = urlsplit(target)
    if parsed.scheme in SKIP_SCHEMES or parsed.netloc:
        return None

    path = Path(unquote(parsed.path))
    if not path.parts:
        return None
    if path.is_absolute():
        return ROOT / Path(*path.parts[1:])
    return markdown.parent / path


def main() -> int:
    failures: list[str] = []
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "--", "*.md"],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    for relative_name in sorted(result.stdout.splitlines()):
        markdown = ROOT / relative_name
        text = markdown.read_text(encoding="utf-8")
        for match in LINK.finditer(text):
            target = local_target(markdown, match.group(1))
            if target is not None and not target.exists():
                relative = markdown.relative_to(ROOT)
                failures.append(f"{relative}: missing link target {match.group(1)!r}")

    if failures:
        print("\n".join(failures), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
