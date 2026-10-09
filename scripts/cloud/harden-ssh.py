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
import sys
import tempfile
import uuid


DROPIN = "etc/ssh/sshd_config.d/00-orbit-key-only.conf"
STATE = "var/lib/orbit-devops/ssh-hardening"
TRUSTED_SCRIPT = "usr/local/lib/orbit-devops/harden-ssh.py"
PENDING_PHASES = {"prepared", "configuration_created", "awaiting_key_confirmation"}
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
        commands = {"sshd": "usr/sbin/sshd", "systemctl": "usr/bin/systemctl", "systemd-run": "usr/bin/systemd-run"}
        relative = "bin/" + name if self.fixture else commands[name]
        executable = self.path(relative)
        if not executable.is_file():
            raise RuntimeError("SSH_COMMAND_UNAVAILABLE")
        result = subprocess.run([str(executable), *arguments], text=True, capture_output=True, timeout=15)
        if result.returncode:
            # 原始 sshd 输出可能包含现有配置，不进入终端、任务文档或 Git。
            failure = "SSH_CONFIG_VALIDATION_FAILED" if name == "sshd" else \
                ("SSH_SAFETY_TIMER_FAILED" if name == "systemd-run" else
                 ("SSH_TIMER_CANCEL_FAILED" if arguments[0] == "stop" else "SSH_RELOAD_FAILED"))
            raise RuntimeError(failure)
        return result.stdout

    def timer_callback(self, operation):
        """特权定时回调只能执行 root 拥有的固定脚本；测试适配器不获得特权路径。"""
        if self.fixture:
            invocation = [sys.executable, "-B", str(Path(__file__).resolve()), "--fixture-root", str(self.root)]
        else:
            script = self.path(TRUSTED_SCRIPT)
            if Path(__file__).absolute() != script or not script.is_file() or script.stat().st_mode & 0o022:
                raise RuntimeError("SSH_TRUSTED_SCRIPT_REQUIRED")
            self.read(script)
            interpreter = Path("/usr/bin/python3").resolve(strict=True)
            interpreter = self.path(str(interpreter.relative_to(self.root)))
            if not interpreter.is_file() or interpreter.stat().st_mode & 0o022:
                raise RuntimeError("SSH_TRUSTED_INTERPRETER_REQUIRED")
            invocation = [str(interpreter), "-B", str(script)]
        return [*invocation, "rollback", "--safety-timeout", "--operation-id", operation]


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
    def lock(self, *, wait=False):
        path = self.boundary.path(STATE + "/lock")
        descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        try:
            info = os.fstat(descriptor)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_uid != self.boundary.owner or \
                    stat.S_IMODE(info.st_mode) != 0o600:
                raise RuntimeError("SSH_STATE_PERMISSIONS_REJECTED")
            try:
                fcntl.flock(descriptor, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
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
        """新增自己的配置并复核 reload 前后状态；其它配置变化绝不能报告成功。"""
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
        callback = self.boundary.timer_callback(operation)
        owned_content = CONTENT + ("# 操作编号：" + operation + "\n").encode()
        for relative, backup_content in originals.items():
            backup = STATE + "/checkpoints/" + operation + "/config/" + relative
            self.boundary.directory(str(Path(backup).parent))
            self.boundary.create(self.boundary.path(backup), backup_content)
        state = {"kind": "orbit-ssh-hardening", "version": 1, "operation_id": operation, "phase": "prepared",
                 "config_hashes": {path: hashlib.sha256(content).hexdigest() for path, content in originals.items()},
                 "prior_effective": prior, "dropin_sha256": hashlib.sha256(owned_content).hexdigest(), "last_error": None,
                 "timer_registered": False}
        self.write_journal(state)
        try:
            self.boundary.execute("systemd-run", ["--unit=" + self.timer_unit(state), "--on-active=180s",
                                                  "--timer-property=AccuracySec=1s", "--collect", "--", *callback])
            state["timer_registered"] = True
            self.write_journal(state)
            self.boundary.create(self.target, owned_content)
            state["phase"] = "configuration_created"
            self.write_journal(state)
            self.verify_policy(state)
            self.boundary.execute("systemctl", ["reload", "ssh"])
            self.verify_policy(state)
        except (RuntimeError, OSError, subprocess.TimeoutExpired) as failure:
            state["last_error"] = str(failure) if isinstance(failure, RuntimeError) else "SSH_APPLY_IO_FAILED"
            self.write_journal(state)
            try:
                self.rollback()
            except RuntimeError as recovery:
                if str(recovery) == "SSH_EXTERNAL_POLICY_REQUIRES_REVIEW":
                    raise recovery
                raise RuntimeError("SSH_APPLY_FAILED_RECOVERY_REQUIRED") from None
            except (OSError, subprocess.TimeoutExpired):
                raise RuntimeError("SSH_APPLY_FAILED_RECOVERY_REQUIRED") from None
            raise failure
        state["phase"] = "awaiting_key_confirmation"
        self.write_journal(state)
        return {"phase": state["phase"], "operation_id": operation, "requires_fresh_key_login": True}

    def verify_policy(self, state):
        """effective 验证须覆盖两个用户，并绑定原配置与 owned 文件的实际内容。"""
        self.validate()
        effective = self.effective()
        if any(effective[key] != "no" for key in ("permitrootlogin", "passwordauthentication", "kbdinteractiveauthentication")) or \
                effective["pubkeyauthentication"] != "yes" or \
                not ({"any", "publickey"} & set(effective["authenticationmethods"].split())):
            raise RuntimeError("SSH_KEY_ONLY_POLICY_REJECTED")
        if self.effective("root")["permitrootlogin"] != "no":
            raise RuntimeError("SSH_ROOT_POLICY_REJECTED")
        if hashlib.sha256(self.boundary.read(self.target, private=True)).hexdigest() != state["dropin_sha256"]:
            raise RuntimeError("SSH_OWNED_DROPIN_CHANGED")
        current = {path: hashlib.sha256(content).hexdigest() for path, content in self.inventory().items()}
        if current != state["config_hashes"]:
            raise RuntimeError("SSH_CONFIGURATION_CHANGED")

    def load_state(self):
        state = json.loads(self.boundary.read(self.journal, private=True))
        if not isinstance(state, dict) or state.get("kind") != "orbit-ssh-hardening" or state.get("version") != 1 or \
                state.get("phase") not in {"prepared", "configuration_created", "awaiting_key_confirmation", "key_only_applied", "rolled_back",
                                           "key_confirmation_recorded", "owned_configuration_removed", "rollback_removed_requires_review"} or \
                not isinstance(state.get("operation_id"), str) or \
                not isinstance(state.get("config_hashes"), dict) or not state["config_hashes"] or \
                not all(isinstance(key, str) and isinstance(value, str) and re.fullmatch(r"[a-f0-9]{64}", value)
                        for key, value in state["config_hashes"].items()) or \
                not isinstance(state.get("prior_effective"), dict) or set(state["prior_effective"]) != EFFECTIVE_KEYS or \
                not isinstance(state.get("dropin_sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", state["dropin_sha256"]):
            raise RuntimeError("SSH_OWNERSHIP_JOURNAL_INVALID")
        uuid.UUID(state["operation_id"])
        if not isinstance(state.get("timer_registered", False), bool):
            raise RuntimeError("SSH_OWNERSHIP_JOURNAL_INVALID")
        state.setdefault("timer_registered", False)
        return state

    def timer_unit(self, state):
        return "orbit-ssh-safety-" + uuid.UUID(state["operation_id"]).hex

    def stop_timer(self, state):
        if state["timer_registered"]:
            self.boundary.execute("systemctl", ["stop", self.timer_unit(state) + ".timer"])
            state["timer_registered"] = False
            self.write_journal(state)

    def confirm(self, confirmed, operation_id):
        """新会话须显式绑定本轮 ID；先持久确认再取消 timer，旧确认与回调均无效。"""
        if not confirmed:
            raise RuntimeError("SSH_FRESH_KEY_CONFIRMATION_REQUIRED")
        if not operation_id:
            raise RuntimeError("SSH_CONFIRMATION_OPERATION_REQUIRED")
        state = self.load_state()
        if operation_id != state["operation_id"]:
            raise RuntimeError("SSH_CONFIRMATION_OPERATION_MISMATCH")
        if state["phase"] not in {"awaiting_key_confirmation", "key_confirmation_recorded", "key_only_applied"}:
            raise RuntimeError("SSH_CONFIRMATION_NOT_PENDING")
        try:
            self.verify_policy(state)
            # timer service 可能已 queued 并等待本 flock；先持久化非 pending 状态，才可
            # 停止 timer。旧回调不能撤销已确认或下一轮操作。
            state["phase"] = "key_confirmation_recorded"
            self.write_journal(state)
            self.stop_timer(state)
            self.verify_policy(state)
        except (RuntimeError, OSError, subprocess.TimeoutExpired) as failure:
            state["last_error"] = str(failure) if isinstance(failure, RuntimeError) else "SSH_CONFIRM_IO_FAILED"
            self.write_journal(state)
            self.rollback()
            raise failure
        state["phase"] = "key_only_applied"
        state["last_error"] = None
        self.write_journal(state)
        return {"phase": state["phase"], "operation_id": state["operation_id"], "requires_fresh_key_login": False}

    def rollback(self, *, safety_timeout=False, operation_id=None):
        """先撤销 exact owned 文件；外部策略已变时不重载未知配置，也不覆盖别人。"""
        state = self.load_state()
        operation = state["operation_id"]
        if safety_timeout and (operation_id != operation or state["phase"] not in PENDING_PHASES):
            return {"phase": state["phase"], "operation_id": operation, "safety_action": "not_pending"}
        if os.path.lexists(self.target):
            owned = self.boundary.read(self.target, private=True)
            if hashlib.sha256(owned).hexdigest() != state["dropin_sha256"]:
                raise RuntimeError("SSH_OWNED_DROPIN_CHANGED")
            self.target.unlink()
            directory = os.open(self.target.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
            state["phase"] = "owned_configuration_removed"
            self.write_journal(state)
        try:
            current = {path: hashlib.sha256(content).hexdigest() for path, content in self.inventory().items()}
            if current != state["config_hashes"]:
                raise RuntimeError("SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
            self.validate()
            if self.effective() != state["prior_effective"]:
                raise RuntimeError("SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
            if state["phase"] != "rolled_back":
                self.boundary.execute("systemctl", ["reload", "ssh"])
                current = {path: hashlib.sha256(content).hexdigest() for path, content in self.inventory().items()}
                if current != state["config_hashes"] or self.effective() != state["prior_effective"]:
                    raise RuntimeError("SSH_EXTERNAL_POLICY_REQUIRES_REVIEW")
        except (RuntimeError, OSError, subprocess.TimeoutExpired):
            # 自己的 drop-in 不再潜伏磁盘；未知外部策略必须由仍存活的会话/控制台核验。
            state["phase"] = "rollback_removed_requires_review"
            state["last_error"] = "SSH_EXTERNAL_POLICY_REQUIRES_REVIEW"
            self.write_journal(state)
            try:
                self.stop_timer(state)
            except (RuntimeError, OSError, subprocess.TimeoutExpired):
                pass
            raise RuntimeError("SSH_EXTERNAL_POLICY_REQUIRES_REVIEW") from None
        state["phase"] = "rolled_back"
        self.write_journal(state)
        self.stop_timer(state)
        return {"phase": state["phase"], "operation_id": operation, "requires_fresh_key_login": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture-root", help=argparse.SUPPRESS)
    actions = parser.add_subparsers(dest="action", required=True)
    apply = actions.add_parser("apply", help="先外部确认 ubuntu 公钥登录，再禁用 root/密码 SSH")
    apply.add_argument("--confirm-ubuntu-key-login", action="store_true")
    rollback = actions.add_parser("rollback", help="仅撤销本脚本拥有的配置，恢复原有效策略")
    rollback.add_argument("--safety-timeout", action="store_true", help=argparse.SUPPRESS)
    rollback.add_argument("--operation-id", help=argparse.SUPPRESS)
    confirm = actions.add_parser("confirm", help="从新公钥 SSH 会话确认并取消本轮自动撤回")
    confirm.add_argument("--confirm-fresh-key-login", action="store_true")
    confirm.add_argument("--operation-id", help="必须与本轮 apply 返回的 operation_id 完全一致")
    args = parser.parse_args()
    hardening = Hardening(Boundary(args.fixture_root))
    if args.action == "rollback" and bool(args.operation_id) != args.safety_timeout:
        raise RuntimeError("SSH_SAFETY_OPERATION_REQUIRED")
    with hardening.lock(wait=args.action == "rollback" and args.safety_timeout):
        if args.action == "apply":
            result = hardening.apply(args.confirm_ubuntu_key_login)
        elif args.action == "confirm":
            result = hardening.confirm(args.confirm_fresh_key_login, args.operation_id)
        else:
            result = hardening.rollback(safety_timeout=args.safety_timeout, operation_id=args.operation_id)
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
