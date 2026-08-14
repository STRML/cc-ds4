"""ds4-run's model selection.

The dispatcher is a script without a .py suffix, so it is loaded by path.
"""
import os
import unittest
from importlib.machinery import SourceFileLoader
from importlib.util import module_from_spec, spec_from_loader

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RUN = os.path.join(ROOT, "skills", "ds4-skill-family", "bin", "ds4-run")


def _load():
    loader = SourceFileLoader("ds4_run", RUN)
    spec = spec_from_loader(loader.name, loader)
    mod = module_from_spec(spec)
    loader.exec_module(mod)
    return mod


class DirectModelTest(unittest.TestCase):
    def setUp(self):
        self.m = _load()

    def test_pro_tier_gets_pro_on_direct(self):
        # direct is the only profile whose endpoint serves the pro family.
        # Collapsing a pro tier to flash here meant a coordinator asking for
        # pro on the one endpoint that has it silently got flash.
        self.assertEqual(self.m.direct_model("pro-xhigh"), "deepseek-v4-pro[1m]")
        self.assertEqual(self.m.direct_model("pro-medium"), "deepseek-v4-pro[1m]")

    def test_flash_tier_gets_flash_on_direct(self):
        self.assertEqual(self.m.direct_model("flash-xhigh"), "deepseek-v4-flash[1m]")
        self.assertEqual(self.m.direct_model("flash-medium"), "deepseek-v4-flash[1m]")

    def test_explicit_model_wins(self):
        self.assertEqual(self.m.direct_model("pro-xhigh", "some-other-id"), "some-other-id")

    def test_every_tier_maps(self):
        # A tier added to TIERS without a direct mapping would otherwise fall
        # through to flash unnoticed.
        for tier in self.m.TIERS:
            self.assertIn(self.m.direct_model(tier), self.m.DIRECT_MODELS.values(), tier)


if __name__ == "__main__":
    unittest.main()
