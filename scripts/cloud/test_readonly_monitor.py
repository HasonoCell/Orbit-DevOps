"""通过公开 CLI 验证专用只读 SSH 巡检身份的安装与精确撤销。"""

import base64
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("install-readonly-monitor.py")
CHECKER = "/usr/local/lib/orbit-devops/check-host-health.py"
FORCED = 'restrict,command="sudo -n ' + CHECKER + '"'
EXISTING_KEY = "ssh-ed25519 EXISTING-KEY existing-user\n"


def public_key():
    def ssh_string(value):
        return len(value).to_bytes(4, "big") + value

    blob = ssh_string(b"ssh-ed25519") + ssh_string(bytes(range(32)))
    return "ssh-ed25519 " + base64.b64encode(blob).decode() + " workflow-comment\n"


class ReadonlyMonitorCommandTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="orbit-readonly-monitor-fixture-")
        self.root = Path(self.temporary.name).resolve()
        self.root.chmod(0o700)
        (self.root / "source").mkdir()
        self.checker_source = self.root / "source/check-host-health.py"
        self.checker_source.write_text("#!/usr/bin/python3 -I\nraise SystemExit(0)\n")
        (self.root / "source/sshd_config").write_text("Include /etc/ssh/sshd_config.d/*.conf\n")
        (self.root / "source/sshd_config.d").mkdir()
        self.key_file = self.root / "monitor.pub"
        self.key_file.write_text(public_key())
        self.ssh = self.root / "home/ubuntu/.ssh"
        self.ssh.mkdir(parents=True, mode=0o700)
        self.authorized_keys = self.ssh / "authorized_keys"
        self.authorized_keys.write_text(EXISTING_KEY)
        self.authorized_keys.chmod(0o600)
        (self.root / "bin").mkdir()
        adapter = """#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
root = Path(__file__).resolve().parents[1]
with (root / 'external-commands.jsonl').open('a') as trace:
    trace.write(json.dumps([Path(__file__).name] + sys.argv[1:]) + '\\n')
name = Path(__file__).name
joined = ' '.join(sys.argv[1:])
if name == 'k3s':
    if 'get nodes' in joined:
        value = {'items': [{'metadata': {'labels': {'orbit-devops.dev/environment': 'private-test',
            'kubernetes.io/arch': 'amd64'}}, 'status': {'conditions': [{'type': 'Ready', 'status': 'True'}]}}]}
    elif 'get namespace kube-system' in joined:
        value = {'metadata': {'uid': 'fixture-cluster'}}
    elif 'get namespace orbit-system' in joined:
        value = {'metadata': {'uid': 'fixture-namespace', 'labels': {'app.kubernetes.io/managed-by': 'orbit-devops',
            'orbit-devops.dev/environment': 'private-test'}}}
    elif 'get certificate orbit-management-tls' in joined:
        value = {'metadata': {'name': 'orbit-management-tls'}, 'status': {'conditions': [{'type': 'Ready', 'status': 'True'}]}}
    else:
        sys.exit(90)
    print(json.dumps(value))
elif name == 'sshd':
    print('pubkeyauthentication yes')
    print('authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2')
    print('authorizedkeyscommand none')
    print('forcecommand none')
elif name == 'visudo':
    if os.environ.get('FAIL_CANDIDATE_SUDOERS') and (root / 'etc/sudoers.d/orbit-readonly-monitor').exists():
        print('SUDOERS-CONTENT-MUST-NOT-LEAK', file=sys.stderr)
        sys.exit(94)
    sys.exit(0)
elif name == 'runuser':
    expected = ['-u', 'ubuntu', '--', 'sudo', '-n', '/usr/local/lib/orbit-devops/check-host-health.py']
    if sys.argv[1:] != expected:
        sys.exit(91)
    if os.environ.get('FAIL_HEALTH'):
        print('PRODUCTION-DATA-MUST-NOT-LEAK', file=sys.stderr)
        sys.exit(93)
    for check in ('identity','node','system_services','workloads','dependencies','backup','capacity','task_lag','certificate'):
        print('HOST_HEALTH_OK check=' + check)
    print('HOST_HEALTH_READY')
else:
    sys.exit(92)
"""
        for name in ("k3s", "sshd", "visudo", "runuser"):
            path = self.root / "bin" / name
            path.write_text(adapter)
            path.chmod(0o755)

    def tearDown(self):
        self.temporary.cleanup()

    def cli(self, action, *extra, expected=0, environment=None, input_data=None, umask=-1):
        result = subprocess.run([
            sys.executable, "-B", str(SCRIPT), "--fixture-root", str(self.root), action, *extra,
        ], text=True, capture_output=True, timeout=15,
            env={**os.environ, **(environment or {})}, input=input_data, umask=umask)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        self.assertEqual(result.stderr, "")
        return json.loads(result.stdout)

    def install(self):
        return self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file),
        )

    def visudo_alternatives(self):
        """构造发行版命令链，所有目标仍位于 CLI 允许的私有 fixture 根内。"""
        executable = self.root / "usr/lib/cargo/bin/visudo"
        executable.parent.mkdir(parents=True)
        executable.write_text("#!/usr/bin/env python3\nraise SystemExit(0)\n")
        executable.chmod(0o755)
        alternatives = self.root / "etc/alternatives/visudo"
        alternatives.parent.mkdir(parents=True)
        alternatives.symlink_to("/usr/lib/cargo/bin/visudo")
        command = self.root / "bin/visudo"
        command.unlink()
        command.symlink_to("/etc/alternatives/visudo")
        return executable, alternatives

    def assert_failed_before_monitor_writes(self):
        self.assertEqual(self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
        ), {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in (CHECKER.removeprefix("/"), "etc/orbit-devops/host-health.json",
                         "etc/sudoers.d/orbit-readonly-monitor", "var/lib/orbit-devops/readonly-monitor/receipt.json"):
            self.assertFalse((self.root / relative).exists())

    def test_install_preserves_existing_keys_and_proves_exact_readonly_command(self):
        result = self.install()
        self.assertEqual(result, {"phase": "installed"})

        key_content = self.authorized_keys.read_text()
        self.assertTrue(key_content.startswith(EXISTING_KEY))
        installed = [line for line in key_content.splitlines() if line.startswith(FORCED)]
        self.assertEqual(len(installed), 1)
        self.assertRegex(installed[0], r"^" + FORCED.replace('"', r'\"') +
                         r" ssh-ed25519 [A-Za-z0-9+/]+=* orbit-readonly-monitor:[0-9a-f-]{36}$")

        checker = self.root / CHECKER.removeprefix("/")
        config = self.root / "etc/orbit-devops/host-health.json"
        sudoers = self.root / "etc/sudoers.d/orbit-readonly-monitor"
        receipt = self.root / "var/lib/orbit-devops/readonly-monitor/receipt.json"
        self.assertEqual(checker.read_bytes(), self.checker_source.read_bytes())
        self.assertEqual(stat.S_IMODE(checker.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(checker.parent.stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE(config.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(config.parent.stat().st_mode), 0o700)
        self.assertEqual(json.loads(config.read_text()), {
            "certificate_name": "orbit-management-tls",
            "cluster_uid": "fixture-cluster",
            "kind": "orbit-host-health",
            "namespace_uid": "fixture-namespace",
            "version": 1,
        })
        self.assertEqual(stat.S_IMODE(sudoers.stat().st_mode), 0o440)
        self.assertEqual(sudoers.read_text(),
                         'ubuntu ALL=(root) NOPASSWD: ' + CHECKER + ' ""\n')
        self.assertEqual(stat.S_IMODE(receipt.stat().st_mode), 0o600)
        self.assertNotIn("EXISTING-KEY", receipt.read_text())
        self.assertNotIn(str(self.key_file), receipt.read_text())

        calls = [json.loads(line) for line in (self.root / "external-commands.jsonl").read_text().splitlines()]
        self.assertIn(["runuser", "-u", "ubuntu", "--", "sudo", "-n", CHECKER], calls)

    def test_wrong_cluster_identity_fails_before_installation_writes(self):
        result = self.cli(
            "install", "--cluster-uid", "wrong-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
        )
        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in ("usr", "etc", "var"):
            self.assertFalse((self.root / relative).exists())

    def test_restrictive_admin_umask_does_not_change_owned_directory_contract(self):
        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), umask=0o077,
        )

        self.assertEqual(result, {"phase": "installed"})
        self.assertEqual(stat.S_IMODE((self.root / "usr/local/lib/orbit-devops").stat().st_mode), 0o755)
        self.assertEqual(stat.S_IMODE((self.root / "etc/orbit-devops").stat().st_mode), 0o700)

    def test_official_visudo_alternatives_chain_supports_install_and_remove(self):
        # Ubuntu sudo-rs 的固定发行版链；fixture 将绝对逻辑路径映射到私有根。
        self.visudo_alternatives()

        self.assertEqual(self.install(), {"phase": "installed"})
        self.assertEqual(self.cli("remove"), {"phase": "removed"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)

    def test_arbitrary_visudo_alternatives_target_fails_before_monitor_writes(self):
        _, alternatives = self.visudo_alternatives()
        alternatives.unlink()
        alternatives.symlink_to("/usr/bin/true")

        self.assert_failed_before_monitor_writes()

    def test_group_writable_visudo_executable_fails_before_monitor_writes(self):
        executable, _ = self.visudo_alternatives()
        executable.chmod(0o775)

        self.assert_failed_before_monitor_writes()

    def test_group_writable_visudo_alternatives_directory_fails_before_monitor_writes(self):
        _, alternatives = self.visudo_alternatives()
        alternatives.parent.chmod(0o775)

        self.assert_failed_before_monitor_writes()

    def test_symlinked_visudo_final_executable_fails_before_monitor_writes(self):
        executable, _ = self.visudo_alternatives()
        executable.unlink()
        executable.symlink_to("/usr/bin/true")

        self.assert_failed_before_monitor_writes()

    def test_unprotected_existing_authorized_keys_fails_before_installation_writes(self):
        self.authorized_keys.chmod(0o644)

        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
        )

        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in ("usr", "etc", "var"):
            self.assertFalse((self.root / relative).exists())

    def test_existing_key_cannot_be_reused_as_a_restricted_monitor_identity(self):
        for extra_file in (False, True):
            with self.subTest(authorized_keys2=extra_file):
                target = self.ssh / "authorized_keys2" if extra_file else self.authorized_keys
                target.write_text(public_key())
                target.chmod(0o600)

                result = self.cli(
                    "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
                    "--public-key-file", str(self.key_file), expected=1,
                )

                self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
                self.assertEqual(target.read_text(), public_key())
                for relative in ("usr", "etc", "var"):
                    self.assertFalse((self.root / relative).exists())
                if not extra_file:
                    self.authorized_keys.write_text(EXISTING_KEY)

    def test_address_specific_ssh_policy_is_rejected_before_installation_writes(self):
        (self.root / "source/sshd_config.d/conditional.conf").write_text(
            "Match Address 192.0.2.0/24\n    AuthorizedKeysCommand /custom/key-provider\n")

        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
        )

        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in ("usr", "etc", "var"):
            self.assertFalse((self.root / relative).exists())

    def test_remove_deletes_only_owned_key_and_files_after_external_key_change(self):
        self.install()
        external = "ssh-ed25519 EXTERNAL-AFTER-INSTALL another-user\n"
        with self.authorized_keys.open("a") as handle:
            handle.write(external)

        result = self.cli("remove")

        self.assertEqual(result, {"phase": "removed"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY + external)
        for relative in (CHECKER.removeprefix("/"), "etc/orbit-devops/host-health.json",
                         "etc/sudoers.d/orbit-readonly-monitor"):
            self.assertFalse((self.root / relative).exists())
        receipt = json.loads((self.root / "var/lib/orbit-devops/readonly-monitor/receipt.json").read_text())
        self.assertEqual(receipt["phase"], "removed")

    def test_failed_health_proof_removes_all_installer_owned_access(self):
        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
            environment={"FAIL_HEALTH": "1"},
        )

        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in (CHECKER.removeprefix("/"), "etc/orbit-devops/host-health.json",
                         "etc/sudoers.d/orbit-readonly-monitor"):
            self.assertFalse((self.root / relative).exists())
        receipt = json.loads((self.root / "var/lib/orbit-devops/readonly-monitor/receipt.json").read_text())
        self.assertEqual(receipt["phase"], "removed")

    def test_candidate_sudoers_failure_removes_the_rejected_candidate(self):
        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
            environment={"FAIL_CANDIDATE_SUDOERS": "1"},
        )

        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in (CHECKER.removeprefix("/"), "etc/orbit-devops/host-health.json",
                         "etc/sudoers.d/orbit-readonly-monitor"):
            self.assertFalse((self.root / relative).exists())
        receipt = json.loads((self.root / "var/lib/orbit-devops/readonly-monitor/receipt.json").read_text())
        self.assertEqual(receipt["phase"], "removed")

    def test_private_key_material_is_rejected_without_installation_writes(self):
        self.key_file.write_text("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n")
        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
        )
        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        for relative in ("usr", "etc", "var"):
            self.assertFalse((self.root / relative).exists())

    def test_public_key_can_be_supplied_by_stdin_without_persisting_input_location(self):
        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", "-", input_data=public_key(),
        )
        self.assertEqual(result, {"phase": "installed"})
        receipt = (self.root / "var/lib/orbit-devops/readonly-monitor/receipt.json").read_text()
        self.assertNotIn(str(self.key_file), receipt)

    def test_symlinked_root_target_is_rejected_without_following_it(self):
        outside = self.root / "outside"
        outside.mkdir()
        (self.root / "etc").symlink_to(outside, target_is_directory=True)
        result = self.cli(
            "install", "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
            "--public-key-file", str(self.key_file), expected=1,
        )
        self.assertEqual(result, {"error": "MONITOR_OPERATION_FAILED"})
        self.assertEqual(list(outside.iterdir()), [])
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)

    def test_remove_preserves_externally_modified_owned_file_for_review(self):
        self.install()
        checker = self.root / CHECKER.removeprefix("/")
        checker.write_text(checker.read_text() + "# external change\n")
        checker.chmod(0o700)

        result = self.cli("remove")

        self.assertEqual(result, {"phase": "review"})
        self.assertTrue(checker.exists())
        self.assertIn("external change", checker.read_text())
        self.assertEqual(self.authorized_keys.read_text(), EXISTING_KEY)
        self.assertFalse((self.root / "etc/orbit-devops/host-health.json").exists())
        self.assertFalse((self.root / "etc/sudoers.d/orbit-readonly-monitor").exists())


if __name__ == "__main__":
    unittest.main()
