"""Docs that are executable instructions.

The profile docs are pasted into Claude Code and the skill docs are read by
agents, so a stale command in them is a broken command, not a typo. Two things
went out of date at once in this repo: the profile setup kept telling Linux
users to run a proxy that had been deleted, and the skill prose kept naming
reasoning tiers that argparse now rejects with exit 2.
"""
import os
import re
import unittest
from importlib.machinery import SourceFileLoader
from importlib.util import module_from_spec, spec_from_loader

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def _docs(*rel):
    out = []
    for r in rel:
        base = os.path.join(ROOT, r)
        for dirpath, _, names in os.walk(base):
            for n in names:
                if n.endswith(".md"):
                    out.append(os.path.join(dirpath, n))
    return out


def _valid_tiers():
    loader = SourceFileLoader(
        "ds4_run", os.path.join(ROOT, "skills", "ds4-skill-family", "bin", "ds4-run"))
    spec = spec_from_loader(loader.name, loader)
    mod = module_from_spec(spec)
    loader.exec_module(mod)
    return set(mod.TIERS)


class DocsTest(unittest.TestCase):
    def test_no_doc_points_at_the_deleted_python_proxy(self):
        offenders = []
        for path in _docs("profiles", "skills") + [os.path.join(ROOT, "README.md")]:
            with open(path) as fh:
                if "src/proxy.py" in fh.read():
                    offenders.append(os.path.relpath(path, ROOT))
        self.assertEqual(offenders, [], "these docs still tell the reader to run a deleted file")

    def test_every_tier_named_in_a_skill_is_dispatchable(self):
        valid = _valid_tiers()
        offenders = []
        for path in _docs("skills"):
            with open(path) as fh:
                for m in re.finditer(r"--tier\s+`?([a-z][a-z-]*)", fh.read()):
                    if m.group(1) not in valid:
                        offenders.append((os.path.relpath(path, ROOT), m.group(1)))
        self.assertEqual(offenders, [], "ds4-run would reject these with exit 2")

    def test_no_skill_doc_names_a_retired_tier(self):
        # The --tier check above only sees a name attached to the flag, so the
        # one file actually titled "Roles and tiers" — a table whose tier column
        # read "xhigh/max", and the file SKILL.md tells agents to choose from —
        # slipped straight past it. Match the bare names anywhere instead, since
        # in a doc about tiers that is what they mean.
        retired = re.compile(r"ds4-max|ds4-xhigh|ds4-high|ds4-low")
        bare = re.compile(r"(?<![\w-])(xhigh|high|low|max)(?![\w-])")
        # Where a bare word reads as a tier NAME rather than as English: inside
        # backticks, or alone in a table cell. Scoping by "does this line say
        # tier" was tried and is wrong — the roles.md table puts the column
        # header on one line and the values on others, and the verify floor
        # says "never routes lower than `high`" without the word at all, so
        # both regressions this guard exists for slipped straight through.
        # Whole-file matching is also wrong: it fails on "a high level of
        # detail".
        offenders = []
        for path in _docs("skills"):
            with open(path) as fh:
                text = fh.read()
            rel = os.path.relpath(path, ROOT)
            for m in retired.finditer(text):
                offenders.append((rel, m.group(0)))
            spans = re.findall(r"`([^`]+)`", text)
            for line in text.splitlines():
                if line.lstrip().startswith("|"):
                    spans.extend(c.strip() for c in line.split("|"))
            for span in spans:
                for m in bare.finditer(span):
                    offenders.append((rel, m.group(0)))
        self.assertEqual(offenders, [], "these name tiers ds4-run no longer accepts")
