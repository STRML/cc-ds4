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


if __name__ == "__main__":
    unittest.main()
