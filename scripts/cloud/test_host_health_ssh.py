#!/usr/bin/env python3
"""SSH 巡检客户端只测试公开入口和外部 SSH 边界，不使用真实凭据。"""
import importlib.util
import io
from pathlib import Path
from types import SimpleNamespace
import unittest


SCRIPT = Path(__file__).with_name("check-host-health-ssh.py")
ENVIRONMENT = {
    "ORBIT_ORIGIN_IP": "1.1.1.1",
    "ORBIT_MONITOR_SSH_KEY": "-----BEGIN OPENSSH PRIVATE KEY-----\nZml4dHVyZQ==\n-----END OPENSSH PRIVATE KEY-----\n",
    "ORBIT_MONITOR_HOST_KEY": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
}
SUCCESS = "\n".join([
    "HOST_HEALTH_OK check=identity", "HOST_HEALTH_OK check=node",
    "HOST_HEALTH_OK check=system_services", "HOST_HEALTH_OK check=workloads",
    "HOST_HEALTH_OK check=dependencies", "HOST_HEALTH_OK check=backup",
    "HOST_HEALTH_OK check=capacity", "HOST_HEALTH_OK check=task_lag",
    "HOST_HEALTH_OK check=certificate", "HOST_HEALTH_READY", "",
])


class SSHAdapter:
    """只替代外部 SSH 进程；协议输出独立于生产检查器实现。"""
    def __init__(self, output=SUCCESS, status=0):
        self.output, self.status = output, status

    def execute(self):
        return SimpleNamespace(returncode=self.status, stdout=self.output, stderr="private-ssh-diagnostic")


def checker():
    specification = importlib.util.spec_from_file_location("host_health_ssh", SCRIPT)
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


class HostHealthSSHTests(unittest.TestCase):
    def test_missing_configuration_fails_without_disclosing_environment(self):
        output, error = io.StringIO(), io.StringIO()
        result = checker().main(environ={"SECRET_MARKER": "never-print-me"}, stdout=output, stderr=error)
        self.assertEqual(result, 1)
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(error.getvalue(), "HOST_HEALTH_SSH_FAILED check=configuration\n")

    def test_complete_readonly_report_is_printed_without_raw_ssh_diagnostics(self):
        output, error = io.StringIO(), io.StringIO()
        result = checker().main(environ=ENVIRONMENT, stdout=output, stderr=error,
                                boundary_factory=lambda *arguments: SSHAdapter())
        self.assertEqual(result, 0)
        self.assertEqual(output.getvalue(), SUCCESS)
        self.assertEqual(error.getvalue(), "")

    def test_unrecognized_remote_output_is_not_disclosed_even_on_success_exit(self):
        output, error = io.StringIO(), io.StringIO()
        result = checker().main(environ=ENVIRONMENT, stdout=output, stderr=error,
                                boundary_factory=lambda *arguments: SSHAdapter(SUCCESS + "a-private-value\n"))
        self.assertEqual(result, 1)
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(error.getvalue(), "HOST_HEALTH_SSH_FAILED check=protocol\n")

    def test_unsafe_target_or_key_material_is_rejected_before_ssh(self):
        def forbidden_boundary(*arguments):
            raise AssertionError("invalid input reached SSH")

        invalid = [
            ("ORBIT_ORIGIN_IP", "127.0.0.1"), ("ORBIT_ORIGIN_IP", "1.1.1.1\nother-host"),
            ("ORBIT_MONITOR_HOST_KEY", "ssh-ed25519 invalid"),
            ("ORBIT_MONITOR_HOST_KEY", ENVIRONMENT["ORBIT_MONITOR_HOST_KEY"] + "\n*.example ssh-rsa unsafe"),
            ("ORBIT_MONITOR_SSH_KEY", "not-an-openssh-key"),
        ]
        for name, value in invalid:
            with self.subTest(name=name, value=value):
                environment = dict(ENVIRONMENT, **{name: value})
                output, error = io.StringIO(), io.StringIO()
                result = checker().main(environ=environment, stdout=output, stderr=error,
                                        boundary_factory=forbidden_boundary)
                self.assertEqual(result, 1)
                self.assertEqual(output.getvalue(), "")
                self.assertEqual(error.getvalue(), "HOST_HEALTH_SSH_FAILED check=configuration\n")

    def test_failed_backup_is_a_health_alert_not_a_connection_failure(self):
        report = "\n".join(SUCCESS.splitlines()[:5] + ["HOST_HEALTH_FAILED check=backup", ""])
        output, error = io.StringIO(), io.StringIO()
        result = checker().main(environ=ENVIRONMENT, stdout=output, stderr=error,
                                boundary_factory=lambda *arguments: SSHAdapter(report, 1))
        self.assertEqual(result, 1)
        self.assertEqual(output.getvalue(), report)
        self.assertEqual(error.getvalue(), "HOST_HEALTH_SSH_FAILED check=health\n")


if __name__ == "__main__":
    unittest.main()
