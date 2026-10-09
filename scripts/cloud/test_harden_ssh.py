"""通过公开 CLI 演练 SSH 配置边界；只使用非 root 的临时文件与命令适配器。"""
import fcntl
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import time
import unittest


SCRIPT = Path(__file__).with_name("harden-ssh.py")
DROPIN = "00-orbit-key-only.conf"


class SshHardeningCommandTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="orbit-ssh-fixture-")
        self.root = Path(self.temporary.name).resolve()
        self.ssh = self.root / "etc/ssh"
        (self.ssh / "sshd_config.d").mkdir(parents=True)
        self.original = "Include /etc/ssh/sshd_config.d/*.conf\nPubkeyAuthentication yes\n"
        (self.ssh / "sshd_config").write_text(self.original)
        (self.ssh / "sshd_config.d/50-cloud-init.conf").write_text("PasswordAuthentication yes\n")
        (self.root / "bin").mkdir()
        adapter = """#!/usr/bin/env python3
import json, os
from pathlib import Path
import sys
root = Path(__file__).resolve().parents[1]
with (root / 'external-commands.jsonl').open('a') as trace:
    trace.write(json.dumps([Path(__file__).name] + sys.argv[1:]) + '\\n')
dropin = root / 'etc/ssh/sshd_config.d/00-orbit-key-only.conf'
if Path(__file__).name == 'systemd-run':
    command = sys.argv[sys.argv.index('--') + 1:]
    (root / 'timer-command.json').write_text(json.dumps(command))
    sys.exit(0)
if Path(__file__).name == 'systemctl':
    if len(sys.argv) == 3 and sys.argv[1] == 'stop' and sys.argv[2].startswith('orbit-ssh-safety-') and sys.argv[2].endswith('.timer'):
        if os.getenv('CHANGE_CONFIG_DURING_TIMER_CANCEL') == '1':
            (root / 'etc/ssh/sshd_config.d/50-cloud-init.conf').write_text('PasswordAuthentication yes\\n# timer cancellation admin edit\\n')
        (root / 'timer-cancelled').write_text(sys.argv[2])
        sys.exit(0)
    if sys.argv[1:] != ['reload', 'ssh']:
        sys.exit(90)
    if os.getenv('FAIL_CANDIDATE_RELOAD') == '1' and dropin.exists():
        sys.exit(1)
    if dropin.exists():
        (root / 'candidate-reloaded').touch()
    if os.getenv('CHANGE_CONFIG_DURING_RELOAD') == '1' and dropin.exists():
        (root / 'etc/ssh/sshd_config.d/50-cloud-init.conf').write_text('PasswordAuthentication yes\\n# post reload admin edit\\n')
    sys.exit(0)
if '-t' in sys.argv:
    if os.getenv('FAIL_CANDIDATE_SYNTAX') == '1' and dropin.exists():
        sys.exit(1)
    sys.exit(0)
if '-T' not in sys.argv:
    sys.exit(91)
if os.getenv('CHANGE_CONFIG_DURING_CANDIDATE_CHECK') == '1' and dropin.exists():
    (root / 'etc/ssh/sshd_config.d/50-cloud-init.conf').write_text('PasswordAuthentication yes\\n# concurrent admin edit\\n')
print('permitrootlogin ' + ('no' if dropin.exists() else 'prohibit-password'))
print('passwordauthentication ' + ('no' if dropin.exists() and not
    (os.getenv('POST_RELOAD_PASSWORD') == '1' and (root / 'candidate-reloaded').exists()) else 'yes'))
print('kbdinteractiveauthentication ' + ('no' if dropin.exists() else 'yes'))
print('pubkeyauthentication yes')
print('authenticationmethods ' + (os.getenv('CANDIDATE_AUTH_METHODS', 'any') if dropin.exists() else 'any'))
"""
        for name in ("sshd", "systemctl", "systemd-run"):
            path = self.root / "bin" / name
            path.write_text(adapter)
            path.chmod(0o755)

    def tearDown(self):
        self.temporary.cleanup()

    def cli(self, *args, expected=0, environment=None):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT), "--fixture-root", str(self.root), *args],
                                text=True, capture_output=True, timeout=15,
                                env={**os.environ, **(environment or {})})
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return json.loads(result.stdout)

    def test_confirmed_key_login_applies_key_only_policy_and_reload_without_restart(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login")
        self.assertEqual(result["phase"], "awaiting_key_confirmation")
        self.assertTrue(result["requires_fresh_key_login"])
        self.assertEqual((self.ssh / "sshd_config.d" / DROPIN).read_text(),
                         "# Orbit SSH 加固配置；仅由对应 rollback 命令撤销。\n"
                         "PermitRootLogin no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n"
                         "# 操作编号：" + result["operation_id"] + "\n")
        self.assertEqual((self.ssh / "sshd_config").read_text(), self.original)
        commands = [json.loads(line) for line in (self.root / "external-commands.jsonl").read_text().splitlines()]
        self.assertIn(["systemctl", "reload", "ssh"], commands)
        self.assertFalse(any("restart" in command for command in commands))
        checkpoint = self.root / "var/lib/orbit-devops/ssh-hardening"
        for path in checkpoint.rglob("*"):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o700 if path.is_dir() else 0o600)
        self.assertTrue(any(path.name == "sshd_config" and path.read_text() == self.original
                            for path in checkpoint.rglob("*") if path.is_file()))

    def test_independent_rollback_only_removes_owned_dropin(self):
        self.cli("apply", "--confirm-ubuntu-key-login")
        result = self.cli("rollback")
        self.assertEqual(result["phase"], "rolled_back")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertEqual((self.ssh / "sshd_config").read_text(), self.original)
        self.assertEqual((self.ssh / "sshd_config.d/50-cloud-init.conf").read_text(), "PasswordAuthentication yes\n")

    def test_candidate_syntax_failure_removes_only_its_new_configuration(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1,
                          environment={"FAIL_CANDIDATE_SYNTAX": "1"})
        self.assertEqual(result["error"], "SSH_CONFIG_VALIDATION_FAILED")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertEqual((self.ssh / "sshd_config").read_text(), self.original)
        self.assertEqual(self.cli("apply", "--confirm-ubuntu-key-login")["phase"], "awaiting_key_confirmation")

    def test_active_match_requires_manual_review_instead_of_claiming_global_key_only_policy(self):
        (self.ssh / "sshd_config.d/50-cloud-init.conf").write_text("Match User root\n    PermitRootLogin yes\n")
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1)
        self.assertEqual(result["error"], "SSH_COMPLEX_POLICY_REQUIRES_REVIEW")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())

    def test_reload_failure_reinstates_prior_policy_without_restarting_sshd(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1,
                          environment={"FAIL_CANDIDATE_RELOAD": "1"})
        self.assertEqual(result["error"], "SSH_RELOAD_FAILED")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertEqual(self.cli("rollback")["phase"], "rolled_back")

    def test_concurrent_configuration_edit_does_not_reload_or_report_success(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1,
                          environment={"CHANGE_CONFIG_DURING_CANDIDATE_CHECK": "1"})
        self.assertEqual(result["error"], "SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
        self.assertIn("concurrent admin edit", (self.ssh / "sshd_config.d/50-cloud-init.conf").read_text())
        commands = [json.loads(line) for line in (self.root / "external-commands.jsonl").read_text().splitlines()]
        self.assertFalse(any(command[0] == "systemctl" and "reload" in command for command in commands))
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())

    def test_post_reload_configuration_change_removes_owned_policy_without_reloading_unknown_policy(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1,
                          environment={"CHANGE_CONFIG_DURING_RELOAD": "1"})
        self.assertEqual(result["error"], "SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertIn("post reload admin edit", (self.ssh / "sshd_config.d/50-cloud-init.conf").read_text())
        commands = [json.loads(line) for line in (self.root / "external-commands.jsonl").read_text().splitlines()]
        self.assertEqual(sum(command == ["systemctl", "reload", "ssh"] for command in commands), 1)

    def test_password_factor_required_by_authentication_methods_cannot_lock_out_key_login(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1,
                          environment={"CANDIDATE_AUTH_METHODS": "publickey,password"})
        self.assertEqual(result["error"], "SSH_KEY_ONLY_POLICY_REJECTED")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())

    def test_key_session_confirmation_is_required_before_configuration_write(self):
        result = self.cli("apply", expected=1)
        self.assertEqual(result["error"], "SSH_FRESH_KEY_LOGIN_CONFIRMATION_REQUIRED")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertEqual((self.ssh / "sshd_config").read_text(), self.original)

    def test_existing_dropin_is_never_overwritten_or_adopted(self):
        target = self.ssh / "sshd_config.d" / DROPIN
        target.write_text("# administrator-owned configuration\n")
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1)
        self.assertEqual(result["error"], "SSH_EXISTING_DROPIN_REJECTED")
        self.assertEqual(target.read_text(), "# administrator-owned configuration\n")

    def test_rollback_refuses_modified_owned_dropin(self):
        self.cli("apply", "--confirm-ubuntu-key-login")
        target = self.ssh / "sshd_config.d" / DROPIN
        target.write_text("# externally changed owned configuration\n")
        result = self.cli("rollback", expected=1)
        self.assertEqual(result["error"], "SSH_OWNED_DROPIN_CHANGED")
        self.assertEqual(target.read_text(), "# externally changed owned configuration\n")

    def test_rollback_refuses_to_overwrite_concurrent_administrator_changes(self):
        self.cli("apply", "--confirm-ubuntu-key-login")
        changed = self.ssh / "sshd_config.d/50-cloud-init.conf"
        changed.write_text("# administrator-owned edit\nPasswordAuthentication no\n")
        result = self.cli("rollback", expected=1)
        self.assertEqual(result["error"], "SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertEqual(changed.read_text(), "# administrator-owned edit\nPasswordAuthentication no\n")

    @unittest.skipIf(os.geteuid() == 0, "生产 root 路径不能由此本地测试修改")
    def test_production_command_requires_root_and_does_not_use_fixture_adapters(self):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT), "apply", "--confirm-ubuntu-key-login"],
                                text=True, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stdout)["error"], "SSH_ROOT_REQUIRED")
        self.assertFalse((self.root / "external-commands.jsonl").exists())

    def test_unconfirmed_policy_has_180_second_independent_withdrawal(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login")
        self.assertEqual(result["phase"], "awaiting_key_confirmation")
        commands = [json.loads(line) for line in (self.root / "external-commands.jsonl").read_text().splitlines()]
        armed = next(command for command in commands if command[0] == "systemd-run")
        self.assertIn("--on-active=180s", armed)
        self.assertIn("--safety-timeout", armed)
        self.assertIn(result["operation_id"], armed)
        callback = json.loads((self.root / "timer-command.json").read_text())
        withdrawn = subprocess.run(callback, text=True, capture_output=True, timeout=15)
        self.assertEqual(withdrawn.returncode, 0, withdrawn.stdout + withdrawn.stderr)
        self.assertEqual(json.loads(withdrawn.stdout)["phase"], "rolled_back")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())

    def test_fresh_key_confirmation_cancels_timer_and_queued_handler_cannot_retract_confirmed_policy(self):
        applied = self.cli("apply", "--confirm-ubuntu-key-login")
        callback = json.loads((self.root / "timer-command.json").read_text())
        confirmed = self.cli("confirm", "--confirm-fresh-key-login", "--operation-id", applied["operation_id"])
        self.assertEqual(confirmed["phase"], "key_only_applied")
        self.assertFalse(confirmed["requires_fresh_key_login"])
        self.assertTrue((self.root / "timer-cancelled").exists())
        queued = subprocess.run(callback, text=True, capture_output=True, timeout=15)
        self.assertEqual(queued.returncode, 0, queued.stdout + queued.stderr)
        self.assertEqual(json.loads(queued.stdout)["safety_action"], "not_pending")
        self.assertTrue((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertEqual(confirmed["operation_id"], applied["operation_id"])

    def test_confirmation_failure_after_timer_cancellation_removes_only_owned_file(self):
        applied = self.cli("apply", "--confirm-ubuntu-key-login")
        result = self.cli("confirm", "--confirm-fresh-key-login", "--operation-id", applied["operation_id"], expected=1,
                          environment={"CHANGE_CONFIG_DURING_TIMER_CANCEL": "1"})
        self.assertEqual(result["error"], "SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertIn("timer cancellation admin edit", (self.ssh / "sshd_config.d/50-cloud-init.conf").read_text())

    def test_post_reload_effective_policy_failure_is_not_reported_as_pending_success(self):
        result = self.cli("apply", "--confirm-ubuntu-key-login", expected=1,
                          environment={"POST_RELOAD_PASSWORD": "1"})
        self.assertEqual(result["error"], "SSH_KEY_ONLY_POLICY_REJECTED")
        self.assertFalse((self.ssh / "sshd_config.d" / DROPIN).exists())

    def test_old_timer_cannot_retract_a_later_pending_operation(self):
        first = self.cli("apply", "--confirm-ubuntu-key-login")
        stale = json.loads((self.root / "timer-command.json").read_text())
        self.cli("rollback")
        current = self.cli("apply", "--confirm-ubuntu-key-login")
        self.assertNotEqual(first["operation_id"], current["operation_id"])
        expired = subprocess.run(stale, text=True, capture_output=True, timeout=15)
        self.assertEqual(expired.returncode, 0, expired.stdout + expired.stderr)
        self.assertEqual(json.loads(expired.stdout)["safety_action"], "not_pending")
        self.assertIn(current["operation_id"], (self.ssh / "sshd_config.d" / DROPIN).read_text())

    def test_safety_handler_waits_for_mutex_instead_of_skipping_pending_withdrawal(self):
        self.cli("apply", "--confirm-ubuntu-key-login")
        callback = json.loads((self.root / "timer-command.json").read_text())
        lock = self.root / "var/lib/orbit-devops/ssh-hardening/lock"
        with lock.open("rb") as owner:
            fcntl.flock(owner, fcntl.LOCK_EX)
            process = subprocess.Popen(callback, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                time.sleep(0.1)
                self.assertIsNone(process.poll())
                fcntl.flock(owner, fcntl.LOCK_UN)
                stdout, stderr = process.communicate(timeout=15)
                self.assertEqual(process.returncode, 0, stdout + stderr)
                self.assertEqual(json.loads(stdout)["phase"], "rolled_back")
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=5)

    def test_confirmation_requires_external_fresh_key_session_declaration(self):
        applied = self.cli("apply", "--confirm-ubuntu-key-login")
        result = self.cli("confirm", "--operation-id", applied["operation_id"], expected=1)
        self.assertEqual(result["error"], "SSH_FRESH_KEY_CONFIRMATION_REQUIRED")
        self.assertTrue((self.ssh / "sshd_config.d" / DROPIN).exists())
        self.assertFalse((self.root / "timer-cancelled").exists())

    def test_stale_confirmation_requires_matching_operation_and_cannot_confirm_later_policy(self):
        earlier = self.cli("apply", "--confirm-ubuntu-key-login")
        self.cli("rollback")
        later = self.cli("apply", "--confirm-ubuntu-key-login")
        result = self.cli("confirm", "--confirm-fresh-key-login", expected=1)
        self.assertEqual(result["error"], "SSH_CONFIRMATION_OPERATION_REQUIRED")
        cancelled = (self.root / "timer-cancelled").read_text()
        wrong = self.cli("confirm", "--confirm-fresh-key-login", "--operation-id", earlier["operation_id"], expected=1)
        self.assertEqual(wrong["error"], "SSH_CONFIRMATION_OPERATION_MISMATCH")
        self.assertEqual((self.root / "timer-cancelled").read_text(), cancelled)
        self.assertIn(later["operation_id"], (self.ssh / "sshd_config.d" / DROPIN).read_text())
        callback = json.loads((self.root / "timer-command.json").read_text())
        expired = subprocess.run(callback, text=True, capture_output=True, timeout=15)
        self.assertEqual(expired.returncode, 0, expired.stdout + expired.stderr)
        self.assertEqual(json.loads(expired.stdout)["phase"], "rolled_back")


if __name__ == "__main__":
    unittest.main()
