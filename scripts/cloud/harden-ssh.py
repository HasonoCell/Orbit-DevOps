#!/usr/bin/env python3
"""在已确认 ubuntu 公钥登录后，仅增加本脚本拥有的 SSH key-only 配置。"""
import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import shlex
import tempfile
import uuid


DROPIN = "etc/ssh/sshd_config.d/00-orbit-key-only.conf"
STATE = "var/lib/orbit-devops/ssh-hardening"
CONTENT = ("# Orbit SSH 加固配置；仅由对应 rollback 命令撤销。\n"
           "PermitRootLogin no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n").encode()
EFFECTIVE_KEYS = {"permitrootlogin", "passwordauthentication", "kbdinteractiveauthentication",
                  "pubkeyauthentication", "authenticationmethods"}


class Boundary:
    """生产路径和进程均固定；非特权 fixture 只能落在本机专用临时目录。"""
    def __init__(self, fixture_root=None):
        self.owner = os.geteuid()
        self.fixture = fixture_root is not None
        if self.fixture:
            candidate = Path(fixture_root)
            if self.owner == 0 or candidate.is_symlink() or not candidate.is_dir():
                raise RuntimeError("SSH_FIXTURE_BOUNDARY_REJECTED")
            self.root = candidate.resolve()
            if self.root.parent != Path(tempfile.gettempdir()).resolve() or \
                    not self.root.name.startswith("orbit-ssh-fixture-"):
                raise RuntimeError("SSH_FIXTURE_BOUNDARY_REJECTED")
            info = self.root.stat()
            if info.st_uid != self.owner or stat.S_IMODE(info.st_mode) != 0o700:
                raise RuntimeError("SSH_FIXTURE_BOUNDARY_REJECTED")
        else:
            if self.owner != 0:
                raise RuntimeError("SSH_ROOT_REQUIRED")
            self.root = Path("/")

    def path(self, relative):
        current = self.root
        for part in Path(relative).parts:
            current = current / part
            if os.path.lexists(current):
                info = current.lstat()
                if stat.S_ISLNK(info.st_mode) or info.st_uid != self.owner or \
                        (stat.S_ISDIR(info.st_mode) and info.st_mode & 0o022):
                    raise RuntimeError("SSH_FILESYSTEM_BOUNDARY_REJECTED")
        return current

    def directory(self, relative):
        current = self.root
        for part in Path(relative).parts:
            current = current / part
            if not os.path.lexists(current):
                current.mkdir(mode=0o700)
            self.path(str(current.relative_to(self.root)))
            if not current.is_dir():
                raise RuntimeError("SSH_DIRECTORY_INVALID")
        return current

    def read(self, path, *, private=False):
        with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != self.owner or info.st_nlink != 1 or \
                    info.st_size > 1024 * 1024 or (private and stat.S_IMODE(info.st_mode) != 0o600):
                raise RuntimeError("SSH_FILE_INVALID")
            return handle.read(1024 * 1024 + 1)

    def create(self, path, content):
        """完整落盘后独占发布，避免写入失败留下部分配置或覆盖并发出现的目标。"""
        temporary = path.parent / (".orbit-ssh-create-" + uuid.uuid4().hex)
        try:
            with os.fdopen(os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600), "wb") as handle:
                handle.write(content)
                handle.flush()
                os.fsync(handle.fileno())
            os.link(temporary, path, follow_symlinks=False)
        finally:
            if os.path.lexists(temporary):
                temporary.unlink()
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)

    def execute(self, name, arguments):
        relative = "bin/" + name if self.fixture else ("usr/sbin/sshd" if name == "sshd" else "usr/bin/systemctl")
        executable = self.path(relative)
        if not executable.is_file():
            raise RuntimeError("SSH_COMMAND_UNAVAILABLE")
        result = subprocess.run([str(executable), *arguments], text=True, capture_output=True, timeout=15)
        if result.returncode:
            # 原始 sshd 输出可能包含现有配置，不进入终端、任务文档或 Git。
            raise RuntimeError("SSH_CONFIG_VALIDATION_FAILED" if name == "sshd" else "SSH_RELOAD_FAILED")
        return result.stdout


class Hardening:
    """备份在本机受限目录保存；恢复只撤销 exact owned drop-in，不覆盖其它配置。"""
    def __init__(self, boundary):
        self.boundary = boundary
        self.target = boundary.path(DROPIN)
        self.config = boundary.path("etc/ssh/sshd_config")
        self.state_dir = boundary.directory(STATE)
        if stat.S_IMODE(self.state_dir.stat().st_mode) != 0o700:
            raise RuntimeError("SSH_STATE_PERMISSIONS_REJECTED")
        self.journal = boundary.path(STATE + "/current.json")

    @contextlib.contextmanager
    def lock(self):
        path = self.boundary.path(STATE + "/lock")
        descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            info = os.fstat(descriptor)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != self.boundary.owner or \
                    stat.S_IMODE(info.st_mode) != 0o600:
                raise RuntimeError("SSH_STATE_PERMISSIONS_REJECTED")
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError("SSH_OPERATION_BUSY") from None
            yield
        finally:
            os.close(descriptor)

    def write_journal(self, state):
        temporary = self.state_dir / (".current-" + uuid.uuid4().hex + ".json")
        self.boundary.create(temporary, json.dumps(state, sort_keys=True).encode())
        self.boundary.path(STATE + "/current.json")
        os.replace(temporary, self.journal)
        directory = os.open(self.state_dir, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)

    def validate(self):
        self.boundary.execute("sshd", ["-t", "-f", str(self.config)])

    def effective(self, user="ubuntu"):
        raw = self.boundary.execute("sshd", ["-T", "-f", str(self.config), "-C",
                                            "user=" + user + ",host=localhost,addr=127.0.0.1"])
        values = {}
        for line in raw.splitlines():
            fields = line.split(None, 1)
            if len(fields) == 2 and fields[0] in EFFECTIVE_KEYS:
                values[fields[0]] = fields[1].lower()
        if set(values) != EFFECTIVE_KEYS:
            raise RuntimeError("SSH_EFFECTIVE_POLICY_INCOMPLETE")
        return values

    def inventory(self):
        paths = [self.config, *sorted(self.boundary.path("etc/ssh/sshd_config.d").glob("*.conf"))]
        if len(paths) > 128:
            raise RuntimeError("SSH_CONFIG_SET_TOO_LARGE")
        contents = {}
        for path in paths:
            relative = str(path.relative_to(self.boundary.root))
            if relative != DROPIN:
                contents[relative] = self.boundary.read(self.boundary.path(relative))
        return contents

    def apply(self, confirmed):
        if not confirmed:
            raise RuntimeError("SSH_FRESH_KEY_LOGIN_CONFIRMATION_REQUIRED")
        if os.path.lexists(self.target):
            raise RuntimeError("SSH_EXISTING_DROPIN_REJECTED")
        if self.journal.exists() and self.load_state()["phase"] != "rolled_back":
            raise RuntimeError("SSH_RECOVERY_REQUIRED")
        self.validate()
        prior = self.effective()
        if prior["pubkeyauthentication"] != "yes":
            raise RuntimeError("SSH_PUBLIC_KEY_NOT_AVAILABLE")
        originals = self.inventory()
        # localhost 的 -T 无法穷举 Match Address/User/Host；本入口只支持常规 Ubuntu
        # 全局配置。其它策略须人工核验，不能把局部 effective 值误报为全局禁用。
        for content in originals.values():
            for line in content.decode().splitlines():
                words = shlex.split(line, comments=True)
                if words and (words[0].lower() == "match" or
                              (words[0].lower() == "include" and words[1:] != ["/etc/ssh/sshd_config.d/*.conf"])):
                    raise RuntimeError("SSH_COMPLEX_POLICY_REQUIRES_REVIEW")
        operation = str(uuid.uuid4())
        for relative, content in originals.items():
            backup = STATE + "/checkpoints/" + operation + "/config/" + relative
            self.boundary.directory(str(Path(backup).parent))
            self.boundary.create(self.boundary.path(backup), content)
        state = {"kind": "orbit-ssh-hardening", "version": 1, "operation_id": operation, "phase": "prepared",
                 "config_hashes": {path: hashlib.sha256(content).hexdigest() for path, content in originals.items()},
                 "prior_effective": prior, "dropin_sha256": hashlib.sha256(CONTENT).hexdigest(), "last_error": None}
        self.write_journal(state)
        self.boundary.create(self.target, CONTENT)
        state["phase"] = "configuration_created"
        self.write_journal(state)
        try:
            self.validate()
            effective = self.effective()
            if any(effective[key] != "no" for key in ("permitrootlogin", "passwordauthentication", "kbdinteractiveauthentication")) or \
                    effective["pubkeyauthentication"] != "yes" or \
                    not ({"any", "publickey"} & set(effective["authenticationmethods"].split())):
                raise RuntimeError("SSH_KEY_ONLY_POLICY_REJECTED")
            if self.effective("root")["permitrootlogin"] != "no":
                raise RuntimeError("SSH_ROOT_POLICY_REJECTED")
            current = {path: hashlib.sha256(content).hexdigest() for path, content in self.inventory().items()}
            if current != state["config_hashes"]:
                raise RuntimeError("SSH_CONFIGURATION_CHANGED")
            self.boundary.execute("systemctl", ["reload", "ssh"])
        except (RuntimeError, OSError, subprocess.TimeoutExpired) as failure:
            state["last_error"] = str(failure) if isinstance(failure, RuntimeError) else "SSH_APPLY_IO_FAILED"
            self.write_journal(state)
            try:
                self.rollback()
            except (RuntimeError, OSError, subprocess.TimeoutExpired):
                raise RuntimeError("SSH_APPLY_FAILED_RECOVERY_REQUIRED") from None
            raise failure
        state["phase"] = "key_only_applied"
        self.write_journal(state)
        return {"phase": state["phase"], "operation_id": operation, "requires_fresh_key_login": True}

    def load_state(self):
        state = json.loads(self.boundary.read(self.journal, private=True))
        if not isinstance(state, dict) or state.get("kind") != "orbit-ssh-hardening" or state.get("version") != 1 or \
                state.get("phase") not in {"prepared", "configuration_created", "key_only_applied", "rolled_back"} or \
                not isinstance(state.get("operation_id"), str) or \
                not isinstance(state.get("config_hashes"), dict) or not state["config_hashes"] or \
                not all(isinstance(key, str) and isinstance(value, str) and re.fullmatch(r"[a-f0-9]{64}", value)
                        for key, value in state["config_hashes"].items()) or \
                not isinstance(state.get("prior_effective"), dict) or set(state["prior_effective"]) != EFFECTIVE_KEYS or \
                not isinstance(state.get("dropin_sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", state["dropin_sha256"]):
            raise RuntimeError("SSH_OWNERSHIP_JOURNAL_INVALID")
        uuid.UUID(state["operation_id"])
        return state

    def rollback(self):
        state = self.load_state()
        operation = state["operation_id"]
        # 其它配置发生变化时只报错，不能通过重放整份备份覆盖管理员的新设置。
        current = {path: hashlib.sha256(content).hexdigest() for path, content in self.inventory().items()}
        if current != state["config_hashes"]:
            raise RuntimeError("SSH_CONFIGURATION_CHANGED")
        if not os.path.lexists(self.target):
            self.validate()
            if self.effective() != state["prior_effective"]:
                raise RuntimeError("SSH_PRIOR_POLICY_NOT_RESTORED")
            if state["phase"] != "rolled_back":
                self.boundary.execute("systemctl", ["reload", "ssh"])
                state["phase"] = "rolled_back"
                self.write_journal(state)
            return {"phase": state["phase"], "operation_id": operation, "requires_fresh_key_login": True}
        owned = self.boundary.read(self.target, private=True)
        if hashlib.sha256(owned).hexdigest() != state["dropin_sha256"]:
            raise RuntimeError("SSH_OWNED_DROPIN_CHANGED")
        self.target.unlink()
        try:
            self.validate()
            if self.effective() != state["prior_effective"]:
                raise RuntimeError("SSH_PRIOR_POLICY_NOT_RESTORED")
            self.boundary.execute("systemctl", ["reload", "ssh"])
        except (RuntimeError, OSError, subprocess.TimeoutExpired):
            # 撤销失败时重新放回已经生效的 key-only 配置；不覆盖任何其它文件。
            self.boundary.create(self.target, owned)
            self.validate()
            self.boundary.execute("systemctl", ["reload", "ssh"])
            raise RuntimeError("SSH_ROLLBACK_FAILED_POLICY_RETAINED") from None
        state["phase"] = "rolled_back"
        self.write_journal(state)
        return {"phase": state["phase"], "operation_id": operation, "requires_fresh_key_login": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture-root", help=argparse.SUPPRESS)
    actions = parser.add_subparsers(dest="action", required=True)
    apply = actions.add_parser("apply", help="先外部确认 ubuntu 公钥登录，再禁用 root/密码 SSH")
    apply.add_argument("--confirm-ubuntu-key-login", action="store_true")
    actions.add_parser("rollback", help="仅撤销本脚本拥有的配置，恢复原有效策略")
    args = parser.parse_args()
    hardening = Hardening(Boundary(args.fixture_root))
    with hardening.lock():
        result = hardening.apply(args.confirm_ubuntu_key_login) if args.action == "apply" else hardening.rollback()
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except RuntimeError as failure:
        print(json.dumps({"error": str(failure)}))
        raise SystemExit(1) from None
    except (OSError, ValueError, subprocess.TimeoutExpired):
        print(json.dumps({"error": "SSH_HARDENING_FAILED"}))
        raise SystemExit(1) from None
