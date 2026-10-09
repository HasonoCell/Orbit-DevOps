#!/usr/bin/env python3
"""为 ubuntu 安装只能执行固定主机巡检命令的独立 SSH 公钥。"""

import argparse
import base64
import binascii
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import stat
import subprocess
import sys
import tempfile
import uuid


CHECKER = "usr/local/lib/orbit-devops/check-host-health.py"
CONFIG = "etc/orbit-devops/host-health.json"
SUDOERS = "etc/sudoers.d/orbit-readonly-monitor"
STATE = "var/lib/orbit-devops/readonly-monitor"
AUTHORIZED_KEYS = "home/ubuntu/.ssh/authorized_keys"
CERTIFICATE = "orbit-management-tls"
CHECKS = ("identity", "node", "system_services", "workloads", "dependencies", "backup",
          "capacity", "task_lag", "certificate")
MAX_FILE_SIZE = 1024 * 1024


class Boundary:
    """把生产路径和命令固定到根目录；测试根只允许受限临时目录。"""

    def __init__(self, fixture_root=None):
        self.fixture = fixture_root is not None
        self.owner = os.geteuid()
        if self.fixture:
            candidate = Path(fixture_root)
            if self.owner == 0 or candidate.is_symlink() or not candidate.is_dir():
                raise RuntimeError("MONITOR_FIXTURE_BOUNDARY_REJECTED")
            self.root = candidate.resolve()
            info = self.root.stat()
            if self.root.parent != Path(tempfile.gettempdir()).resolve() or \
                    not self.root.name.startswith("orbit-readonly-monitor-fixture-") or \
                    info.st_uid != self.owner or stat.S_IMODE(info.st_mode) != 0o700:
                raise RuntimeError("MONITOR_FIXTURE_BOUNDARY_REJECTED")
            self.user_uid = self.owner
        else:
            if self.owner != 0:
                raise RuntimeError("MONITOR_ROOT_REQUIRED")
            self.root = Path("/")
            account = pwd.getpwnam("ubuntu")
            if account.pw_dir != "/home/ubuntu":
                raise RuntimeError("MONITOR_ACCOUNT_REJECTED")
            self.user_uid = account.pw_uid
            self.user_gid = account.pw_gid
        if self.fixture:
            self.user_gid = os.getegid()

    def path(self, relative, *, user_owned=False):
        """拒绝沿途符号链接和可被非所有者改写的目录。"""
        current = self.root
        parts = Path(relative).parts
        for index, part in enumerate(parts):
            current = current / part
            if not os.path.lexists(current):
                continue
            info = current.lstat()
            if stat.S_ISLNK(info.st_mode):
                raise RuntimeError("MONITOR_FILESYSTEM_BOUNDARY_REJECTED")
            expected = self.user_uid if user_owned and index >= len(parts) - 3 else self.owner
            # /home 仍由 root 拥有，ubuntu 只拥有自己的 home、.ssh 与 authorized_keys。
            if user_owned and index == len(parts) - 4:
                expected = self.owner
            if info.st_uid != expected or (stat.S_ISDIR(info.st_mode) and info.st_mode & 0o022):
                raise RuntimeError("MONITOR_FILESYSTEM_BOUNDARY_REJECTED")
        return current

    def directory(self, relative, mode, *, exact=False):
        current = self.root
        for part in Path(relative).parts:
            current = current / part
            if not os.path.lexists(current):
                created_mode = mode if current == self.root / relative else 0o755
                current.mkdir(mode=created_mode)
                # 管理员常用 umask 077；只对本操作刚创建的目录落实约定权限，
                # 不放宽已有目录。否则受限 umask 会把要求 755 的新目录变成 700。
                current.chmod(created_mode)
            info = current.lstat()
            if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or \
                    info.st_uid != self.owner or info.st_mode & 0o022:
                raise RuntimeError("MONITOR_DIRECTORY_REJECTED")
        if exact and stat.S_IMODE(current.stat().st_mode) != mode:
            raise RuntimeError("MONITOR_DIRECTORY_PERMISSIONS_REJECTED")
        return current

    def create(self, path, content, mode):
        """先完整落盘再独占发布，避免部分文件和覆盖并发出现的目标。"""
        temporary = path.parent / (".orbit-monitor-create-" + uuid.uuid4().hex)
        try:
            descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
            with os.fdopen(descriptor, "wb") as handle:
                os.fchmod(handle.fileno(), mode)
                handle.write(content)
                handle.flush()
                os.fsync(handle.fileno())
            os.link(temporary, path, follow_symlinks=False)
        finally:
            if os.path.lexists(temporary):
                temporary.unlink()
        self.fsync_directory(path.parent)

    @staticmethod
    def fsync_directory(directory):
        descriptor = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)

    def visudo_executable(self):
        """仅兼容 Ubuntu sudo-rs 的固定 alternatives 链，其他路径仍拒绝链接。

        每个链接所在目录沿普通边界检查，链接本身必须属于受信任所有者；
        不使用 resolve 接受任意目标。fixture 只把同一绝对逻辑路径映射到私有根。
        """
        relative = "bin/visudo" if self.fixture else "usr/sbin/visudo"
        command = self.path(str(Path(relative).parent)) / "visudo"
        if not command.is_symlink():
            return self.path(relative)
        for source, target in ((relative, "etc/alternatives/visudo"),
                               ("etc/alternatives/visudo", "usr/lib/cargo/bin/visudo")):
            path = self.path(str(Path(source).parent)) / "visudo"
            info = path.lstat()
            if not stat.S_ISLNK(info.st_mode) or info.st_uid != self.owner or info.st_nlink != 1 or \
                    os.readlink(path) != "/" + target:
                raise RuntimeError("MONITOR_COMMAND_UNAVAILABLE")
        return self.path("usr/lib/cargo/bin/visudo")

    def execute(self, name, arguments):
        commands = {
            "k3s": "usr/local/bin/k3s",
            "sshd": "usr/sbin/sshd",
            "visudo": "usr/sbin/visudo",
            "runuser": "usr/sbin/runuser",
        }
        executable = self.visudo_executable() if name == "visudo" else \
            self.path("bin/" + name if self.fixture else commands[name])
        if not executable.is_file() or executable.stat().st_mode & 0o022:
            raise RuntimeError("MONITOR_COMMAND_UNAVAILABLE")
        result = subprocess.run([str(executable), *arguments], capture_output=True, timeout=30)
        if result.returncode:
            # 命令输出可能包含真实主机配置或巡检数据，任何失败都只返回固定错误码。
            raise RuntimeError("MONITOR_EXTERNAL_CHECK_FAILED")
        try:
            return result.stdout.decode("utf-8")
        except UnicodeDecodeError:
            raise RuntimeError("MONITOR_EXTERNAL_RESPONSE_REJECTED") from None


class Installer:
    """只拥有固定巡检文件和一条带 UUID 标记的 authorized_keys 记录。"""

    def __init__(self, boundary):
        self.boundary = boundary
        self.checker = boundary.path(CHECKER)
        self.config = boundary.path(CONFIG)
        self.sudoers = boundary.path(SUDOERS)
        self.state_dir = boundary.path(STATE)
        self.receipt = boundary.path(STATE + "/receipt.json")
        self.authorized_keys = boundary.path(AUTHORIZED_KEYS, user_owned=True)

    def command(self, name, arguments):
        return self.boundary.execute(name, arguments)

    def kubectl_json(self, arguments):
        raw = self.command("k3s", ["kubectl", "--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
                                   "--request-timeout=15s", *arguments, "-o", "json"])
        try:
            value = json.loads(raw)
        except (json.JSONDecodeError, TypeError):
            raise RuntimeError("MONITOR_CLUSTER_RESPONSE_REJECTED") from None
        if not isinstance(value, dict):
            raise RuntimeError("MONITOR_CLUSTER_RESPONSE_REJECTED")
        return value

    def guard_cluster(self, cluster_uid, namespace_uid):
        """所有身份断言在写入前完成，避免把巡检入口装到错误主机。"""
        cluster = self.kubectl_json(["get", "namespace", "kube-system"])
        namespace = self.kubectl_json(["get", "namespace", "orbit-system"])
        node_list = self.kubectl_json(["get", "nodes"])
        certificate = self.kubectl_json(["-n", "orbit-system", "get", "certificate", CERTIFICATE])
        cluster_metadata = cluster.get("metadata")
        namespace_metadata = namespace.get("metadata")
        certificate_metadata = certificate.get("metadata")
        nodes = node_list.get("items")
        if not all(isinstance(value, dict) for value in
                   (cluster_metadata, namespace_metadata, certificate_metadata)) or \
                not isinstance(nodes, list) or any(not isinstance(node, dict) for node in nodes):
            raise RuntimeError("MONITOR_CLUSTER_RESPONSE_REJECTED")
        if cluster_metadata.get("uid") != cluster_uid or namespace_metadata.get("uid") != namespace_uid:
            raise RuntimeError("MONITOR_CLUSTER_IDENTITY_REJECTED")
        labels = namespace_metadata.get("labels")
        if not isinstance(labels, dict):
            raise RuntimeError("MONITOR_CLUSTER_RESPONSE_REJECTED")
        if labels.get("app.kubernetes.io/managed-by") != "orbit-devops" or \
                labels.get("orbit-devops.dev/environment") != "private-test":
            raise RuntimeError("MONITOR_CLUSTER_LABELS_REJECTED")
        if len(nodes) != 1:
            raise RuntimeError("MONITOR_NODE_TOPOLOGY_REJECTED")
        node = nodes[0]
        node_metadata = node.get("metadata")
        node_status = node.get("status")
        certificate_status = certificate.get("status")
        if not all(isinstance(value, dict) for value in (node_metadata, node_status, certificate_status)):
            raise RuntimeError("MONITOR_CLUSTER_RESPONSE_REJECTED")
        node_labels = node_metadata.get("labels")
        node_conditions = node_status.get("conditions")
        certificate_conditions = certificate_status.get("conditions")
        if not isinstance(node_labels, dict) or not isinstance(node_conditions, list) or \
                not isinstance(certificate_conditions, list) or \
                any(not isinstance(value, dict) for value in node_conditions + certificate_conditions):
            raise RuntimeError("MONITOR_CLUSTER_RESPONSE_REJECTED")
        ready = any(condition.get("type") == "Ready" and condition.get("status") == "True"
                    for condition in node_conditions)
        if not ready or node_labels.get("orbit-devops.dev/environment") != "private-test" or \
                node_labels.get("kubernetes.io/arch") != "amd64":
            raise RuntimeError("MONITOR_NODE_IDENTITY_REJECTED")
        cert_ready = any(condition.get("type") == "Ready" and condition.get("status") == "True"
                         for condition in certificate_conditions)
        if certificate_metadata.get("name") != CERTIFICATE or not cert_ready:
            raise RuntimeError("MONITOR_CERTIFICATE_REJECTED")

    def guard_ssh(self):
        # localhost 的 effective 结果不能代表任意 Actions 源地址；仅支持全局
        # Ubuntu 配置，遇到 Match 或其它 Include 必须先人工审查，而不是猜测授权源。
        base = "source" if self.boundary.fixture else "etc/ssh"
        config = self.boundary.path(base + "/sshd_config")
        dropins = self.boundary.path(base + "/sshd_config.d")
        paths = [config, *sorted(dropins.glob("*.conf"))]
        if len(paths) > 128:
            raise RuntimeError("MONITOR_SSH_POLICY_REJECTED")
        for candidate in paths:
            path = self.boundary.path(str(candidate.relative_to(self.boundary.root)))
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(descriptor, "rb") as handle:
                info = os.fstat(handle.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != self.boundary.owner or \
                        info.st_nlink != 1 or info.st_mode & 0o022 or info.st_size > MAX_FILE_SIZE:
                    raise RuntimeError("MONITOR_SSH_POLICY_REJECTED")
                content = handle.read(MAX_FILE_SIZE + 1)
            try:
                lines = content.decode("utf-8").splitlines()
                for line in lines:
                    words = shlex.split(line, comments=True)
                    if words and (words[0].lower() == "match" or
                                  (words[0].lower() == "include" and words[1:] != ["/etc/ssh/sshd_config.d/*.conf"])):
                        raise RuntimeError("MONITOR_SSH_POLICY_REJECTED")
            except (UnicodeDecodeError, ValueError):
                raise RuntimeError("MONITOR_SSH_POLICY_REJECTED") from None
        raw = self.command("sshd", ["-T", "-C", "user=ubuntu,host=localhost,addr=127.0.0.1"])
        values = {}
        for line in raw.splitlines():
            fields = line.lower().split(None, 1)
            if len(fields) == 2:
                values[fields[0]] = fields[1]
        key_files = values.get("authorizedkeysfile", "").split()
        if values.get("pubkeyauthentication") != "yes" or ".ssh/authorized_keys" not in key_files or \
                not set(key_files) <= {".ssh/authorized_keys", ".ssh/authorized_keys2"} or \
                values.get("authorizedkeyscommand") != "none" or values.get("forcecommand") != "none":
            raise RuntimeError("MONITOR_SSH_POLICY_REJECTED")
        return key_files

    def guard_fresh_key(self, public_key, key_files):
        """同一公钥已有普通记录时，追加 restrict 并不能收紧原授权，必须拒绝复用。"""
        expected = base64.b64decode(public_key.split()[1], validate=True)
        for relative in key_files:
            path = self.boundary.path("home/ubuntu/" + relative, user_owned=True)
            if not os.path.lexists(path):
                continue
            descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(descriptor, "rb") as handle:
                info = os.fstat(handle.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_uid != self.boundary.user_uid or \
                        info.st_gid != self.boundary.user_gid or info.st_nlink != 1 or \
                        stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > MAX_FILE_SIZE:
                    raise RuntimeError("MONITOR_AUTHORIZED_KEYS_REJECTED")
                content = handle.read(MAX_FILE_SIZE + 1)
                if len(content) > MAX_FILE_SIZE:
                    raise RuntimeError("MONITOR_AUTHORIZED_KEYS_REJECTED")
            for match in re.finditer(rb"(?:^|[ \t])ssh-ed25519[ \t]+([^ \t\r\n]+)", content, re.MULTILINE):
                try:
                    duplicate = base64.b64decode(match[1], validate=True) == expected
                except binascii.Error:
                    duplicate = False
                if duplicate:
                    raise RuntimeError("MONITOR_EXISTING_KEY_REJECTED")

    @staticmethod
    def read_public_key(filename):
        if filename == "-":
            content = sys.stdin.buffer.read(16 * 1024 + 1)
        else:
            descriptor = os.open(filename, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            with os.fdopen(descriptor, "rb") as handle:
                info = os.fstat(handle.fileno())
                if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > 16 * 1024:
                    raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED")
                content = handle.read(16 * 1024 + 1)
        if len(content) > 16 * 1024 or b"PRIVATE KEY" in content or len(content.splitlines()) != 1:
            raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED")
        try:
            fields = content.decode("ascii").split()
            if len(fields) < 2 or fields[0] != "ssh-ed25519":
                raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED")
            blob = base64.b64decode(fields[1], validate=True)
            position = 0

            def string():
                nonlocal position
                if position + 4 > len(blob):
                    raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED")
                length = int.from_bytes(blob[position:position + 4], "big")
                position += 4
                value = blob[position:position + length]
                if len(value) != length:
                    raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED")
                position += length
                return value

            if string() != b"ssh-ed25519" or len(string()) != 32 or position != len(blob):
                raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED")
        except (UnicodeDecodeError, binascii.Error, ValueError):
            raise RuntimeError("MONITOR_PUBLIC_KEY_REJECTED") from None
        return "ssh-ed25519 " + fields[1]

    def checker_source(self):
        source = self.boundary.root / "source/check-host-health.py" if self.boundary.fixture else \
            Path(__file__).with_name("check-host-health.py")
        descriptor = os.open(source, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > MAX_FILE_SIZE:
                raise RuntimeError("MONITOR_CHECKER_SOURCE_REJECTED")
            content = handle.read(MAX_FILE_SIZE + 1)
        if len(content) > MAX_FILE_SIZE or not content.startswith(b"#!/usr/bin/python3 -I\n"):
            raise RuntimeError("MONITOR_CHECKER_SOURCE_REJECTED")
        return content

    def authorized_keys_descriptor(self):
        descriptor = os.open(self.authorized_keys, os.O_RDWR | os.O_APPEND | os.O_NOFOLLOW)
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != self.boundary.user_uid or \
                info.st_gid != self.boundary.user_gid or info.st_nlink != 1 or \
                stat.S_IMODE(info.st_mode) != 0o600:
            os.close(descriptor)
            raise RuntimeError("MONITOR_AUTHORIZED_KEYS_REJECTED")
        return descriptor

    def write_receipt(self, state, *, create=False):
        content = (json.dumps(state, sort_keys=True, separators=(",", ":")) + "\n").encode()
        if create:
            self.boundary.create(self.receipt, content, 0o600)
            return
        temporary = self.state_dir / (".receipt-" + uuid.uuid4().hex)
        self.boundary.create(temporary, content, 0o600)
        os.replace(temporary, self.receipt)
        self.boundary.fsync_directory(self.state_dir)

    def install(self, cluster_uid, namespace_uid, public_key_file):
        self.guard_cluster(cluster_uid, namespace_uid)
        key_files = self.guard_ssh()
        # 先证明现有 sudoers 可解析，候选失败时才能把失败归因于自身片段并安全撤回。
        self.command("visudo", ["-cf", "/etc/sudoers"])
        public_key = self.read_public_key(public_key_file)
        self.guard_fresh_key(public_key, key_files)
        checker = self.checker_source()
        if os.path.lexists(self.receipt) or any(os.path.lexists(path) for path in
                                                (self.checker, self.config, self.sudoers)):
            raise RuntimeError("MONITOR_EXISTING_INSTALLATION_REJECTED")
        # 写入任何 root 配置前，先证明旧公钥文件可沿精确撤销契约打开。
        # 后续追加仍重新验证文件身份，预检不能代替并发修改边界。
        descriptor = self.authorized_keys_descriptor()
        os.close(descriptor)
        self.boundary.directory(str(Path(CHECKER).parent), 0o755, exact=True)
        self.boundary.directory(str(Path(CONFIG).parent), 0o700, exact=True)
        self.boundary.directory(str(Path(SUDOERS).parent), 0o755)
        self.state_dir = self.boundary.directory(STATE, 0o700, exact=True)
        operation = str(uuid.uuid4())
        line = ('restrict,command="sudo -n /' + CHECKER + '" ' + public_key +
                " orbit-readonly-monitor:" + operation)
        config = (json.dumps({
            "certificate_name": CERTIFICATE,
            "cluster_uid": cluster_uid,
            "kind": "orbit-host-health",
            "namespace_uid": namespace_uid,
            "version": 1,
        }, sort_keys=True, separators=(",", ":")) + "\n").encode()
        sudoers = ('ubuntu ALL=(root) NOPASSWD: /' + CHECKER + ' ""\n').encode()
        state = {
            "kind": "orbit-readonly-monitor-installation",
            "version": 1,
            "operation_id": operation,
            "phase": "prepared",
            "authorized_line": line,
            "owned_sha256": {
                "/" + CHECKER: hashlib.sha256(checker).hexdigest(),
                "/" + CONFIG: hashlib.sha256(config).hexdigest(),
                "/" + SUDOERS: hashlib.sha256(sudoers).hexdigest(),
            },
        }
        self.write_receipt(state, create=True)
        try:
            self.boundary.create(self.checker, checker, 0o700)
            self.boundary.create(self.config, config, 0o600)
            self.boundary.create(self.sudoers, sudoers, 0o440)
            self.command("visudo", ["-cf", "/etc/sudoers"])
            descriptor = self.authorized_keys_descriptor()
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX)
                self.guard_fresh_key(public_key, key_files)
                size = os.fstat(descriptor).st_size
                separator = b""
                if size:
                    os.lseek(descriptor, -1, os.SEEK_END)
                    separator = b"" if os.read(descriptor, 1) == b"\n" else b"\n"
                os.lseek(descriptor, 0, os.SEEK_END)
                addition = separator + (line + "\n").encode()
                if os.write(descriptor, addition) != len(addition):
                    raise RuntimeError("MONITOR_AUTHORIZED_KEYS_WRITE_FAILED")
                os.fsync(descriptor)
                current = self.authorized_keys.lstat()
                installed = os.fstat(descriptor)
                if (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns) != \
                        (installed.st_dev, installed.st_ino, installed.st_size, installed.st_mtime_ns):
                    raise RuntimeError("MONITOR_AUTHORIZED_KEYS_CHANGED")
            finally:
                os.close(descriptor)
            self.prove_health()
        except (OSError, RuntimeError, subprocess.TimeoutExpired):
            # receipt 让异常路径沿同一精确撤销契约收敛；撤销本身失败则保留
            # prepared 状态，供人工重试，绝不猜测或覆盖外部修改。
            try:
                self.remove()
            except (OSError, RuntimeError, subprocess.TimeoutExpired):
                pass
            raise RuntimeError("MONITOR_INSTALLATION_FAILED") from None
        state["phase"] = "installed"
        self.write_receipt(state)

    def load_receipt(self):
        descriptor = os.open(self.receipt, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != self.boundary.owner or \
                    info.st_nlink != 1 or stat.S_IMODE(info.st_mode) != 0o600 or \
                    info.st_size > MAX_FILE_SIZE:
                raise RuntimeError("MONITOR_RECEIPT_REJECTED")
            try:
                state = json.loads(handle.read(MAX_FILE_SIZE + 1))
            except json.JSONDecodeError:
                raise RuntimeError("MONITOR_RECEIPT_REJECTED") from None
        paths = {"/" + CHECKER, "/" + CONFIG, "/" + SUDOERS}
        if not isinstance(state, dict) or state.get("kind") != "orbit-readonly-monitor-installation" or \
                state.get("version") != 1 or \
                state.get("phase") not in {"prepared", "installed", "removed", "review"} or \
                not isinstance(state.get("owned_sha256"), dict) or set(state["owned_sha256"]) != paths:
            raise RuntimeError("MONITOR_RECEIPT_REJECTED")
        try:
            operation = str(uuid.UUID(state["operation_id"]))
        except (KeyError, ValueError, TypeError):
            raise RuntimeError("MONITOR_RECEIPT_REJECTED") from None
        prefix = 'restrict,command="sudo -n /' + CHECKER + '" ssh-ed25519 '
        suffix = " orbit-readonly-monitor:" + operation
        if not isinstance(state.get("authorized_line"), str) or \
                not state["authorized_line"].startswith(prefix) or \
                not state["authorized_line"].endswith(suffix):
            raise RuntimeError("MONITOR_RECEIPT_REJECTED")
        for digest in state["owned_sha256"].values():
            if not isinstance(digest, str) or len(digest) != 64 or any(character not in "0123456789abcdef"
                                                                       for character in digest):
                raise RuntimeError("MONITOR_RECEIPT_REJECTED")
        return state

    def remove_authorized_line(self, line):
        """锁内重写整文件，只删除 receipt 记录的单条完整记录。"""
        descriptor = self.authorized_keys_descriptor()
        temporary = self.authorized_keys.parent / (".orbit-monitor-keys-" + uuid.uuid4().hex)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX)
            original = os.fstat(descriptor)
            if original.st_size > MAX_FILE_SIZE:
                raise RuntimeError("MONITOR_AUTHORIZED_KEYS_REJECTED")
            os.lseek(descriptor, 0, os.SEEK_SET)
            content = os.read(descriptor, MAX_FILE_SIZE + 1)
            target = line.encode()
            records = content.splitlines(keepends=True)
            matches = [index for index, record in enumerate(records)
                       if record == target or record == target + b"\n"]
            if len(matches) > 1:
                raise RuntimeError("MONITOR_AUTHORIZED_KEY_AMBIGUOUS")
            if not matches:
                return
            del records[matches[0]]
            output = b"".join(records)
            target_descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
            with os.fdopen(target_descriptor, "wb") as handle:
                os.fchmod(handle.fileno(), 0o600)
                os.fchown(handle.fileno(), self.boundary.user_uid, self.boundary.user_gid)
                handle.write(output)
                handle.flush()
                os.fsync(handle.fileno())
            current = self.authorized_keys.lstat()
            if (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns) != \
                    (original.st_dev, original.st_ino, original.st_size, original.st_mtime_ns):
                raise RuntimeError("MONITOR_AUTHORIZED_KEYS_CHANGED")
            os.replace(temporary, self.authorized_keys)
            self.boundary.fsync_directory(self.authorized_keys.parent)
        finally:
            os.close(descriptor)
            if os.path.lexists(temporary):
                temporary.unlink()

    def unlink_owned(self, path, expected_digest, mode):
        """仅删除身份、权限与原摘要仍一致的文件；CAS 变化返回 False 并保留外部内容。

        目标已不存在视为幂等成功；不能把摘要不符当作可覆盖或强制清理的授权。
        """
        if not os.path.lexists(path):
            return True
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(descriptor, "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != self.boundary.owner or info.st_nlink != 1 or \
                    stat.S_IMODE(info.st_mode) != mode or info.st_size > MAX_FILE_SIZE:
                return False
            content = handle.read(MAX_FILE_SIZE + 1)
        current = path.lstat()
        if (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns) != \
                (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) or \
                hashlib.sha256(content).hexdigest() != expected_digest:
            return False
        path.unlink()
        self.boundary.fsync_directory(path.parent)
        return True

    def remove(self):
        """先撤销专用 key，再撤销仍由本操作拥有的 root 文件，保留其它授权与修改。

        receipt 为 removed 才代表精确撤销完成；review 表示部分文件或 sudoers
        状态需要人工复核。CLI 返回该 phase，即使退出码为 0 也不能忽略 review。
        """
        state = self.load_receipt()
        if state["phase"] == "removed":
            return "removed"
        self.remove_authorized_line(state["authorized_line"])
        results = [
            self.unlink_owned(self.sudoers, state["owned_sha256"]["/" + SUDOERS], 0o440),
            self.unlink_owned(self.config, state["owned_sha256"]["/" + CONFIG], 0o600),
            self.unlink_owned(self.checker, state["owned_sha256"]["/" + CHECKER], 0o700),
        ]
        policy_valid = True
        try:
            self.command("visudo", ["-cf", "/etc/sudoers"])
        except (OSError, RuntimeError, subprocess.TimeoutExpired):
            policy_valid = False
        state["phase"] = "removed" if all(results) and policy_valid else "review"
        self.write_receipt(state)
        return state["phase"]

    def prove_health(self):
        raw = self.command("runuser", ["-u", "ubuntu", "--", "sudo", "-n",
                                            "/" + CHECKER])
        expected = ["HOST_HEALTH_OK check=" + name for name in CHECKS] + ["HOST_HEALTH_READY"]
        if raw.splitlines() != expected:
            raise RuntimeError("MONITOR_HEALTH_PROOF_REJECTED")


def parser():
    command = argparse.ArgumentParser(description=__doc__)
    command.add_argument("--fixture-root", help=argparse.SUPPRESS)
    subcommands = command.add_subparsers(dest="action", required=True)
    install = subcommands.add_parser("install")
    install.add_argument("--cluster-uid", required=True)
    install.add_argument("--namespace-uid", required=True)
    install.add_argument("--public-key-file", required=True,
                         help="OpenSSH ed25519 公钥文件；使用 - 从标准输入读取")
    subcommands.add_parser("remove")
    return command


def main():
    arguments = parser().parse_args()
    try:
        boundary = Boundary(arguments.fixture_root)
        installer = Installer(boundary)
        if arguments.action == "install":
            installer.install(arguments.cluster_uid, arguments.namespace_uid, arguments.public_key_file)
            phase = "installed"
        else:
            phase = installer.remove()
        print(json.dumps({"phase": phase}, separators=(",", ":")))
        return 0
    except (KeyError, OSError, RuntimeError, subprocess.TimeoutExpired):
        print(json.dumps({"error": "MONITOR_OPERATION_FAILED"}, separators=(",", ":")))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
