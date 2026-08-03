"""Regression tests for install.sh's agent and proxy handling.

These shell out to the real install.sh against throwaway profile dirs. They never
touch the live launchd agent: a stub launchctl on PATH records invocations and
answers like the real tool for an unloaded job, HOME is redirected so the plist
lands in a temp LaunchAgents dir, and uname is stubbed so the launchd branch is
exercised on Linux CI as well as macOS.

Run: python3 -m unittest discover -s tests -v
"""
import json
import os
import plistlib
import subprocess
import tempfile
import textwrap
import unittest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
INSTALL = os.path.join(REPO, "install.sh")

PROFILE_DIRS = {
    "direct": ".claude-ds4",
    "openrouter": ".claude-or-ds4",
    "nous": ".claude-nous",
}


def _write_settings(profile_dir):
    os.makedirs(profile_dir, exist_ok=True)
    with open(os.path.join(profile_dir, "settings.json"), "w") as fh:
        json.dump({"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:1"}}, fh)


def _write_stub(bindir, name, body):
    path = os.path.join(bindir, name)
    with open(path, "w") as fh:
        fh.write(body)
    os.chmod(path, 0o755)


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = os.path.join(self.tmp.name, "home")
        os.makedirs(self.home)
        self.bindir = os.path.join(self.tmp.name, "bin")
        os.makedirs(self.bindir)
        self.log = os.path.join(self.tmp.name, "launchctl.log")
        self.state = os.path.join(self.tmp.name, "unloaded")

        # print must exit 113 for a job that is not loaded, exactly like the
        # real launchctl. install.sh uses that for the legacy-agent sweep and
        # this stub for the (always silent) unload-wait loop. bootout is what
        # unloads the fake job, so a print before the first bootout can report
        # it running via FAKE_LAUNCHD_RUNNING.
        _write_stub(self.bindir, "launchctl", textwrap.dedent(f"""\
            #!/usr/bin/env bash
            echo "$@" >> "{self.log}"
            case "$1" in
              bootout)
                touch "{self.state}"
                exit 0
                ;;
              print)
                if [ ! -e "{self.state}" ] && [ "${{FAKE_LAUNCHD_RUNNING:-0}}" = 1 ]; then
                  printf '\\tstate = running\\n'
                  exit 0
                fi
                exit 113
                ;;
              *)
                exit 0
                ;;
            esac
            """))
        # install.sh gates the launchd branch on `uname` = Darwin. Stub it so the
        # branch is taken on Linux CI too; the rest of the script does not care
        # about the real kernel.
        _write_stub(self.bindir, "uname", "#!/usr/bin/env bash\necho Darwin\n")

    def run_install(self, profile, *extra, env=None):
        profile_dir = os.path.join(self.home, PROFILE_DIRS[profile])
        _write_settings(profile_dir)
        full_env = dict(os.environ)
        full_env["HOME"] = self.home
        full_env["PATH"] = self.bindir + os.pathsep + full_env["PATH"]
        if env:
            full_env.update(env)
        return subprocess.run(
            ["bash", INSTALL, "--profile", profile, *extra],
            capture_output=True, text=True, env=full_env,
        )

    def launchctl_calls(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log) as fh:
            return [line.split() for line in fh.read().splitlines()]

    def plist_path(self):
        return os.path.join(self.home, "Library", "LaunchAgents",
                            "com.strml.cc-ds4.proxy.plist")

    def read_plist(self):
        with open(self.plist_path(), "rb") as fh:
            return plistlib.load(fh)

    def test_plist_unchanged_skips_reload(self):
        # The shared agent serves every profile, so a no-op reinstall must not
        # bootout the live job. First run writes and loads; second must skip.
        first = self.run_install("direct")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertIn("agent:    loaded", first.stdout)

        second = self.run_install("direct")
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertIn("agent:    unchanged, not reloaded", second.stdout)

        bootouts = [c for c in self.launchctl_calls() if c[0] == "bootout"]
        self.assertEqual(len(bootouts), 1, bootouts)

    def test_reload_kickstarts_a_running_job(self):
        # When the plist genuinely changes and the old job was running, reload
        # must bring it back: the kickstart is what stops live sessions dropping.
        proc = self.run_install("direct", env={"FAKE_LAUNCHD_RUNNING": "1"})
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("agent:    loaded", proc.stdout)
        calls = self.launchctl_calls()
        self.assertIn("kickstart", [c[0] for c in calls], calls)
        bootouts = [c for c in calls if c[0] == "bootout"]
        self.assertEqual(len(bootouts), 1, bootouts)

    def test_plist_environment_variables(self):
        # A DS4_* knob exported at install time must reach the agent. Without the
        # EnvironmentVariables block the knobs proxy.py reads are all dead.
        proc = self.run_install("nous", env={"DS4_IDLE_EXIT": "0", "DS4_DEBUG": "1"})
        self.assertEqual(proc.returncode, 0, proc.stderr)
        env = self.read_plist()["EnvironmentVariables"]
        self.assertEqual(env["DS4_IDLE_EXIT"], "0")
        self.assertEqual(env["DS4_DEBUG"], "1")
        # A non-DS4 variable must not be swept into the agent.
        self.assertNotIn("FAKE_LAUNCHD_RUNNING", env)

    def test_plist_changed_by_new_knob_reloads(self):
        # Adding a knob changes the plist, so the agent reloads; an unchanged
        # plist with a new knob would silently leave the old environment live.
        self.run_install("direct")
        before = len([c for c in self.launchctl_calls() if c[0] == "bootout"])
        proc = self.run_install("direct", env={"DS4_IDLE_EXIT": "0"})
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertIn("agent:    loaded", proc.stdout)
        after = len([c for c in self.launchctl_calls() if c[0] == "bootout"])
        self.assertEqual(after, before + 1, self.launchctl_calls())

    def test_no_proxy_does_not_delete_proxy_files(self):
        # --no-proxy leaves the proxy files alone: an earlier run's base URL
        # still points at the proxy port, so removing them would break it.
        profile_dir = os.path.join(self.home, PROFILE_DIRS["direct"])
        os.makedirs(profile_dir, exist_ok=True)
        stale = os.path.join(profile_dir, "ds4-effort-proxy.py")
        with open(stale, "w") as fh:
            fh.write("")

        proc = self.run_install("direct", "--no-proxy")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertTrue(os.path.exists(stale), "stale proxy removed under --no-proxy")

        # A normal install does remove it, so the guard is real and not a no-op.
        proc = self.run_install("direct")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertFalse(os.path.exists(stale))

    def test_dir_is_rejected(self):
        # --dir writes a base URL for a port that nothing binds, because
        # src/proxy.py serves only the three fixed profile directories.
        proc = self.run_install("direct", "--dir", "/tmp/somewhere-else")
        self.assertEqual(proc.returncode, 2)
        self.assertIn("--dir is not supported", proc.stderr)

    def test_effort_command_is_linked_into_the_profile(self):
        # No shared ~/.claude/commands exists, so install.sh makes a real
        # commands dir in the profile and links the command into it.
        proc = self.run_install("openrouter")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        cmd = os.path.join(self.home, ".claude-or-ds4", "commands", "ds4-effort.md")
        self.assertTrue(os.path.islink(cmd))
        self.assertEqual(os.readlink(cmd),
                         os.path.join(REPO, "src", "commands", "ds4-effort.md"))

    def test_effort_command_lands_in_the_shared_commands_dir(self):
        # The profile prompt symlinks commands -> ~/.claude/commands when it
        # exists; install.sh must write through that link, not replace it.
        shared = os.path.join(self.home, ".claude", "commands")
        os.makedirs(shared)
        profile_dir = os.path.join(self.home, PROFILE_DIRS["nous"])
        os.makedirs(profile_dir, exist_ok=True)
        os.symlink(shared, os.path.join(profile_dir, "commands"))
        proc = self.run_install("nous")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertTrue(os.path.isdir(shared))
        self.assertTrue(os.path.islink(os.path.join(shared, "ds4-effort.md")))
        self.assertTrue(os.path.islink(os.path.join(profile_dir, "commands")),
                        "commands symlink must not be replaced by a real dir")


class MemoryLinkTest(unittest.TestCase):
    """ds4-link-memory.sh shares project memory with the canonical ~/.claude copy.

    A profile is a separate config dir, so without this its projects/*/memory is
    per-profile state: notes written on one profile are invisible on the others.
    """

    LINK = os.path.join(REPO, "src", "ds4-link-memory.sh")

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.home = os.path.join(self.tmp.name, "home")
        self.canon = os.path.join(self.home, ".claude", "projects")
        self.profile = os.path.join(self.home, ".claude-ds4")

    def run_link(self, profile_dir):
        return subprocess.run(
            ["bash", self.LINK, profile_dir],
            capture_output=True, text=True,
            env=dict(os.environ, HOME=self.home),
        )

    def test_links_real_memory_dirs_and_merges_notes(self):
        # canonical project has a note; the profile dir has a different one plus a
        # collision. The helper must move the profile-only note into canonical,
        # keep canonical's version of the collision, and symlink the dir.
        canon1 = os.path.join(self.canon, "-proj1", "memory")
        os.makedirs(canon1)
        with open(os.path.join(canon1, "both.md"), "w") as fh:
            fh.write("canon\n")
        prof1 = os.path.join(self.profile, "projects", "-proj1", "memory")
        os.makedirs(prof1)
        with open(os.path.join(prof1, "both.md"), "w") as fh:
            fh.write("profile\n")
        with open(os.path.join(prof1, "prof-only.md"), "w") as fh:
            fh.write("x\n")

        proc = self.run_link(self.profile)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        # dir became a symlink to canonical
        self.assertTrue(os.path.islink(prof1))
        self.assertEqual(os.readlink(prof1), canon1)
        # profile-only note moved in, collision kept canonical's content
        self.assertTrue(os.path.exists(os.path.join(canon1, "prof-only.md")))
        with open(os.path.join(canon1, "both.md")) as fh:
            self.assertEqual(fh.read(), "canon\n")

    def test_no_memory_dir_is_left_alone(self):
        # a project dir with no memory subdir must not be touched
        proj3 = os.path.join(self.profile, "projects", "-proj3")
        os.makedirs(proj3)
        self.run_link(self.profile)
        self.assertFalse(os.path.exists(os.path.join(proj3, "memory")))

    def test_already_linked_is_a_noop(self):
        canon1 = os.path.join(self.canon, "-proj1", "memory")
        os.makedirs(canon1)
        prof1 = os.path.join(self.profile, "projects", "-proj1", "memory")
        os.makedirs(prof1)
        self.run_link(self.profile)
        self.assertTrue(os.path.islink(prof1))
        # second run must not error or re-create a real dir
        proc = self.run_link(self.profile)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertTrue(os.path.islink(prof1))

    def test_no_canonical_means_nothing_happens(self):
        # no ~/.claude/projects at all -> helper exits 0 and links nothing
        prof1 = os.path.join(self.profile, "projects", "-proj1", "memory")
        os.makedirs(prof1)
        proc = self.run_link(self.profile)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertFalse(os.path.islink(prof1))


if __name__ == "__main__":
    unittest.main()
