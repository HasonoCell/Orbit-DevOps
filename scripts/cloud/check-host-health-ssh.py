#!/usr/bin/env python3
"""GitHub Actions 的 SSH 巡检客户端；凭据不进入命令行或检查输出。"""
import base64
import ipaddress
import os
from pathlib import Path
import re
import selectors
import subprocess
import sys
import tempfile
import time

CHECKS = ("identity", "node", "system_services", "workloads", "dependencies", "backup", "capacity", "task_lag", "certificate")


class SSHFailure(Exception):
    """SSH 失败不携带远程诊断或认证配置。"""
    def __init__(self, check="connection"):
        self.check = check


class SSHBoundary:
    """专用公钥只用于固定命令，临时凭据文件在私有目录内生成并自动删除。"""
    def __init__(self, address, key, host_key):
        self.address, self.key, self.host_key = address, key, host_key

    def execute(self):
        """总等待最多 180 秒、输出最多 32 KiB；超时或超限会终止并回收 SSH 子进程。

        只返回完整结果交给公开入口校验，任何失败都不发布已经读取的部分远程输出。
        """
        with tempfile.TemporaryDirectory(prefix="orbit-host-health-") as temporary:
            directory = Path(temporary)
            os.chmod(directory, 0o700)
            for name, content in (("key", self.key), ("known_hosts", self.address + " " + self.host_key + "\n")):
                descriptor = os.open(directory / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(descriptor, "w") as file:
                    file.write(content)
            command = ["/usr/bin/ssh", "-F", "/dev/null", "-T", "-i", str(directory / "key"),
                       "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes",
                       "-o", "UserKnownHostsFile=" + str(directory / "known_hosts"), "-o", "GlobalKnownHostsFile=/dev/null",
                       "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
                       "-o", "PreferredAuthentications=publickey", "-o", "ConnectTimeout=15",
                       "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=2",
                       "ubuntu@" + self.address, "orbit-host-health"]
            # 环境中不传入 Actions 密钥，也不使用本机 Agent、SSH 配置或未知 Host Key。
            with subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                  env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"}) as process:
                deadline, output = time.monotonic() + 180, bytearray()
                try:
                    with selectors.DefaultSelector() as selector:
                        selector.register(process.stdout, selectors.EVENT_READ)
                        while selector.get_map():
                            remaining = deadline - time.monotonic()
                            if remaining <= 0:
                                raise SSHFailure()
                            for event, _ in selector.select(min(remaining, 1)):
                                chunk = os.read(event.fd, 4096)
                                if not chunk:
                                    selector.unregister(event.fileobj)
                                    continue
                                output.extend(chunk)
                                if len(output) > 32768:
                                    raise SSHFailure()
                    status = process.wait(timeout=max(0.01, deadline - time.monotonic()))
                except (SSHFailure, OSError, subprocess.TimeoutExpired):
                    process.kill()
                    process.wait(timeout=5)
                    raise SSHFailure() from None
            return subprocess.CompletedProcess(command, status, bytes(output).decode("utf-8"), "")


def configuration(environ):
    """只允许全球单一 IPv4 与 exact ed25519 host key，拒绝 known_hosts 行注入。"""
    names = ("ORBIT_ORIGIN_IP", "ORBIT_MONITOR_SSH_KEY", "ORBIT_MONITOR_HOST_KEY")
    values = [environ.get(name, "") for name in names]
    if any(not isinstance(value, str) or not value for value in values):
        raise SSHFailure("configuration")
    address, key, host_key = values
    try:
        parsed = ipaddress.ip_address(address)
        if parsed.version != 4 or not parsed.is_global or str(parsed) != address:
            raise ValueError()
        if len(key) > 16384 or not re.fullmatch(
                r"-----BEGIN OPENSSH PRIVATE KEY-----\n(?:[A-Za-z0-9+/=]+\n)+-----END OPENSSH PRIVATE KEY-----\n?", key):
            raise ValueError()
        host_key = host_key.strip()
        if not re.fullmatch(r"ssh-ed25519 [A-Za-z0-9+/]+=*", host_key):
            raise ValueError()
        blob = base64.b64decode(host_key.split()[1], validate=True)
        if len(blob) != 51 or blob[:19] != b"\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20":
            raise ValueError()
    except ValueError:
        raise SSHFailure("configuration") from None
    # Secret 传输不保证保留文件末尾换行，OpenSSH 解析私钥却要求它存在。
    # 只补齐已经通过严格格式校验的结束行，不接受额外内容或改变密钥主体。
    return address, key.rstrip("\n") + "\n", host_key


def main(*, environ=None, stdout=None, stderr=None, boundary_factory=None):
    """公开命令入口只报告固定类别，不回显配置或认证错误正文。"""
    environ = os.environ if environ is None else environ
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr
    boundary_factory = SSHBoundary if boundary_factory is None else boundary_factory
    try:
        result = boundary_factory(*configuration(environ)).execute()
        if result.returncode == 255:
            raise SSHFailure()
        expected = ["HOST_HEALTH_OK check=" + check for check in CHECKS] + ["HOST_HEALTH_READY"]
        lines = result.stdout.splitlines()
        if result.returncode == 1:
            failed = len(lines) - 1
            if not 0 <= failed < len(CHECKS) or lines[:-1] != expected[:failed] or \
                    lines[-1] != "HOST_HEALTH_FAILED check=" + CHECKS[failed]:
                raise SSHFailure("protocol")
            for line in lines:
                print(line, file=stdout)
            raise SSHFailure("health")
        if result.returncode != 0 or lines != expected:
            raise SSHFailure("protocol")
        for line in lines:
            print(line, file=stdout)
    except SSHFailure as failure:
        print("HOST_HEALTH_SSH_FAILED check=" + failure.check, file=stderr)
        return 1
    except (OSError, UnicodeError):
        print("HOST_HEALTH_SSH_FAILED check=connection", file=stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
