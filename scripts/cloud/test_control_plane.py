"""从独立运维命令入口验证失败关闭；外部 Kubernetes 边界由确定性响应替代。"""
import contextlib
import copy
import fcntl
import hashlib
import importlib.util
import io
import json
import os
from datetime import datetime, timezone
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("manage-control-plane.py")
OLD_IMAGE = "fixture/backend@sha256:" + "a" * 64
NEW_IMAGE = "fixture/backend@sha256:" + "b" * 64
NAMES = ("orbit-api", "orbit-build-worker", "orbit-release-worker", "orbit-pipeline-worker", "orbit-gateway-worker")


class KubernetesFixture:
    """仅模拟外部命令协议，不替代升级代码或 SQL 逻辑。"""
    def __init__(self, active=0, broken=False):
        self.active, self.broken = active, broken
        self.deployments = {}
        for name in NAMES:
            role = "api" if name == "orbit-api" else name.removeprefix("orbit-")
            self.deployments[name] = {"metadata": {"uid": name, "resourceVersion": "1", "name": name, "generation": 1}, "spec": {
                "replicas": 1, "selector": {"matchLabels": {"app": name}}, "template": {"metadata": {"labels": {"app": name}}, "spec": {
                    "serviceAccountName": name, "containers": [{"name": role, "command": ["/usr/local/bin/orbit-devops-" + role],
                        "image": OLD_IMAGE, "imagePullPolicy": "Never", "envFrom": [{"configMapRef": {"name": "orbit-config"}}],
                        "env": [{"name": "UNCHANGED", "value": "keep"}], "readinessProbe": {"httpGet": {"path": "/healthz", "port": 8080}}}]}}}}
        self.calls = []

    def __call__(self, command, **kwargs):
        import json
        import subprocess
        self.calls.append(command)
        code, result = 0, {}
        verb = next((x for x in ("get", "patch", "exec", "rollout") if x in command), None)
        if verb == "get":
            kind = command[command.index("get") + 1]
            name = command[command.index("get") + 2]
            if kind == "nodes":
                result = {"items": [{"metadata": {"name": "orbit-upgrade-20261009-control-plane"}, "status": {"conditions": [{"type": "Ready", "status": "True"}]}}]}
            elif kind == "namespace":
                result = {"metadata": {"uid": "fixture-cluster" if name == "kube-system" else "fixture-namespace", "labels": {"app.kubernetes.io/managed-by": "orbit-devops"}}}
            elif kind == "deployment":
                result = self.deployments[name]
            elif kind == "configmap":
                result = {"metadata": {"uid": "fixture-config", "resourceVersion": getattr(self, "config_version", "1")}, "data": {"ORBIT_DEVOPS_MIGRATE_ON_BOOT": "false"}}
            elif kind == "pods":
                name = command[command.index("-l") + 1].removeprefix("app=")
                result = {"items": []} if self.deployments[name]["spec"]["replicas"] == 0 else {"items": [{"metadata": {"name": name}}]}
        elif verb == "exec":
            sql = kwargs.get("input", "")
            result = "26|f\n" if "schema_migrations" in sql else json.dumps({"active_total": self.active, "lease_total": 0, "categories": {"release": self.active}})
        elif verb == "patch":
            name = command[command.index("patch") + 2]
            resource = self.deployments[name]
            previous_spec = copy.deepcopy(resource["spec"])
            for item in json.loads(kwargs["input"]):
                path = item["path"].strip("/").split("/")
                parent = resource
                for part in path[:-1]:
                    parent = parent[int(part)] if isinstance(parent, list) else parent[part]
                key = int(path[-1]) if isinstance(parent, list) else path[-1]
                if item["op"] == "test":
                    if parent[key] != item["value"]:
                        code = 1
                        break
                else:
                    parent[key] = item["value"]
            resource["metadata"]["resourceVersion"] = str(int(resource["metadata"]["resourceVersion"]) + 1)
            if resource["spec"] != previous_spec:
                resource["metadata"]["generation"] += 1
        elif verb == "rollout":
            name = command[command.index("status") + 1].removeprefix("deployment/")
            if self.broken and self.deployments[name]["spec"]["template"]["spec"]["containers"][0]["image"] == NEW_IMAGE:
                code = 1
        output = result if isinstance(result, str) else json.dumps(copy.deepcopy(result))
        return subprocess.CompletedProcess(command, code, output, "")


class ControlPlaneCommandTest(unittest.TestCase):
    def command(self, directory, operation):
        spec = importlib.util.spec_from_file_location("upgrade", SCRIPT)
        upgrade = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(upgrade)
        return upgrade.main(["--local-context", "kind-orbit-upgrade-20261009", "--namespace", "orbit-upgrade-20261009",
                             "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace", "--state-dir", directory,
                             "--confirm-private-upgrade"] + operation)

    def test_wrong_environment_is_rejected_before_workload_write(self):
        spec = importlib.util.spec_from_file_location("upgrade", SCRIPT)
        upgrade = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(upgrade)
        calls = []

        def external(command, **kwargs):
            import subprocess
            calls.append(command)
            if "nodes" in command:
                return subprocess.CompletedProcess(command, 0, '{"items": []}', "")
            return subprocess.CompletedProcess(command, 0, "{}", "")

        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(RuntimeError):
                upgrade.main(["--local-context", "kind-orbit-upgrade-20261009", "--namespace", "orbit-upgrade-20261009",
                              "--cluster-uid", "fixture-cluster", "--namespace-uid", "fixture-namespace",
                              "--state-dir", directory, "--confirm-private-upgrade", "upgrade",
                              "--image", "fixture/backend@sha256:" + "a" * 64])
        self.assertFalse(any("patch" in command or "scale" in command for command in calls))

    def test_upgrade_and_explicit_rollback_preserve_runtime_configuration(self):
        external = KubernetesFixture()
        original = copy.deepcopy(external.deployments)
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), contextlib.redirect_stdout(io.StringIO()):
            self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            for name in NAMES:
                expected = copy.deepcopy(original[name]["spec"])
                expected["template"]["spec"]["containers"][0]["image"] = NEW_IMAGE
                self.assertEqual(external.deployments[name]["spec"], expected)
            self.command(directory, ["rollback"])
            for name in NAMES:
                self.assertEqual(external.deployments[name]["spec"], original[name]["spec"])

    def test_drain_timeout_restores_admission_without_changing_workers_or_canceling_work(self):
        external = KubernetesFixture(active=1)
        original = copy.deepcopy(external.deployments)
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "DRAIN_TIMEOUT"):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE, "--drain-timeout", "1"])
            for name in NAMES:
                self.assertEqual(external.deployments[name]["spec"], original[name]["spec"])
            self.assertEqual(external.active, 1)
            self.assertFalse(any("cancel" in command for command in external.calls))

    def test_unready_candidate_restores_all_five_old_processes(self):
        external = KubernetesFixture(broken=True)
        original = copy.deepcopy(external.deployments)
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(RuntimeError):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            for name in NAMES:
                self.assertEqual(external.deployments[name]["spec"], original[name]["spec"])

    def test_failed_restore_is_retained_and_can_be_recovered_after_operator_fix(self):
        external = KubernetesFixture(broken=True)
        original = copy.deepcopy(external.deployments)
        failures = 0

        def boundary(command, **kwargs):
            import subprocess
            nonlocal failures
            if "patch" in command and "orbit-release-worker" in command:
                failures += 1
                if failures == 2:
                    return subprocess.CompletedProcess(command, 1, "", "fixture transient failure")
            return external(command, **kwargs)

        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", boundary), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaises(RuntimeError):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            external.broken = False
            self.command(directory, ["rollback"])
            for name in NAMES:
                self.assertEqual(external.deployments[name]["spec"], original[name]["spec"])

    def test_concurrent_upgrade_cannot_change_workloads(self):
        external = KubernetesFixture()
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), contextlib.redirect_stdout(io.StringIO()):
            with open(Path(directory) / "lock", "w") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaisesRegex(RuntimeError, "UPGRADE_ALREADY_RUNNING"):
                    self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            self.assertFalse(any("patch" in command for command in external.calls))

    def test_state_directory_under_replaceable_parent_is_rejected(self):
        external = KubernetesFixture()
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), contextlib.redirect_stdout(io.StringIO()):
            parent = Path(directory) / "unsafe"
            parent.mkdir(mode=0o777)
            parent.chmod(0o777)
            state = parent / "state"
            state.mkdir(mode=0o700)
            with self.assertRaisesRegex(RuntimeError, "STATE_DIRECTORY_REJECTED"):
                self.command(str(state), ["upgrade", "--image", NEW_IMAGE])
            self.assertFalse(any("patch" in command for command in external.calls))

    def test_interrupted_drain_restores_admission_without_stopping_active_workers(self):
        external = KubernetesFixture(active=1)

        def kill_during_drain(command, **kwargs):
            if "exec" in command and "schema_migrations" not in kwargs.get("input", ""):
                raise KeyboardInterrupt()
            return external(command, **kwargs)

        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            with patch("subprocess.run", kill_during_drain):
                with self.assertRaises(KeyboardInterrupt):
                    self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            external.calls.clear()
            with patch("subprocess.run", external):
                self.command(directory, ["rollback"])
            self.assertEqual(external.active, 1)
            self.assertEqual(external.deployments["orbit-api"]["spec"]["replicas"], 1)
            for command in external.calls:
                self.assertFalse("patch" in command and any(name in command for name in NAMES[1:]))

    def test_external_scaling_during_drain_is_not_silently_overwritten(self):
        external = KubernetesFixture()

        def concurrent_operator(command, **kwargs):
            if "exec" in command and "schema_migrations" not in kwargs.get("input", ""):
                external.deployments["orbit-api"]["spec"]["replicas"] = 1
            return external(command, **kwargs)

        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", concurrent_operator), contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "WORKLOAD_SPEC_CHANGED"):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            self.assertFalse(any("patch" in command and any(name in command for name in NAMES[1:]) for command in external.calls))

    def test_interrupted_candidate_with_reopened_api_drains_before_stopping_workers(self):
        external = KubernetesFixture()
        original = copy.deepcopy(external.deployments)

        def kill_after_reopening(command, **kwargs):
            result = external(command, **kwargs)
            if "rollout" in command and "deployment/orbit-api" in command and \
                    external.deployments["orbit-api"]["spec"]["template"]["spec"]["containers"][0]["image"] == NEW_IMAGE:
                external.active = 1
                raise KeyboardInterrupt()
            return result

        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            with patch("subprocess.run", kill_after_reopening):
                with self.assertRaises(KeyboardInterrupt):
                    self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            external.calls.clear()
            with patch("subprocess.run", external):
                with self.assertRaisesRegex(RuntimeError, "DRAIN_TIMEOUT"):
                    self.command(directory, ["--drain-timeout", "1", "rollback"])
                self.assertEqual(external.deployments["orbit-api"]["spec"]["replicas"], 1)
                self.assertEqual(external.active, 1)
                self.assertFalse(any("patch" in command and any(name in command for name in NAMES[1:]) for command in external.calls))
                external.active = 0
                self.command(directory, ["rollback"])
                for name in NAMES:
                    self.assertEqual(external.deployments[name]["spec"], original[name]["spec"])

    def test_configuration_changed_during_rollback_is_not_marked_restored(self):
        external = KubernetesFixture()
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
            with patch("subprocess.run", external):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE])

            def config_race(command, **kwargs):
                result = external(command, **kwargs)
                if "rollout" in command and "deployment/orbit-build-worker" in command:
                    external.config_version = "2"
                return result

            with patch("subprocess.run", config_race):
                with self.assertRaisesRegex(RuntimeError, "RUNTIME_CONFIGURATION_CHANGED"):
                    self.command(directory, ["rollback"])
            state = json.loads((Path(directory) / "current.json").read_text())
            self.assertNotEqual(state["phase"], "rolled_back")

    def test_runtime_ready_requires_fresh_delivery_and_operator_acceptance(self):
        external = KubernetesFixture()
        with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()), patch("subprocess.run", external):
            self.command(directory, ["upgrade", "--image", NEW_IMAGE])
            path = Path(directory) / "current.json"
            self.assertEqual(json.loads(path.read_text())["phase"], "runtime_ready")
            options = ["accept", "--delivery-run", "12345678-1234-1234-1234-123456789abc",
                       "--confirm-public-login", "--confirm-auth-boundaries", "--confirm-webhook-review"]
            with self.assertRaisesRegex(RuntimeError, "BUSINESS_DELIVERY_NOT_VERIFIED"):
                self.command(directory, options)
            self.assertEqual(json.loads(path.read_text())["phase"], "runtime_ready")

            def verified_delivery(command, **kwargs):
                import subprocess
                if "exec" in command and "ACCEPTANCE_PROOF" in kwargs.get("input", ""):
                    return subprocess.CompletedProcess(command, 0, '{"verified":true}', "")
                return external(command, **kwargs)

            with patch("subprocess.run", verified_delivery):
                self.command(directory, options)
            state = json.loads(path.read_text())
            self.assertEqual(state["phase"], "complete")
            self.assertEqual(state["postchecks"]["business_delivery"], "verified")


class BackupReceiptProtocolTest(unittest.TestCase):
    def test_missing_or_empty_artifact_receipt_cannot_authorize_maintenance(self):
        spec = importlib.util.spec_from_file_location("upgrade", SCRIPT)
        upgrade = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(upgrade)
        receipt = {"kind": "orbit-control-plane-backup", "version": 1, "checkpoint_id": "fixture",
                   "verified_at": datetime.now(timezone.utc).isoformat(), "artifacts": {},
                   "source": {"cluster_uid": "cluster", "orbit_system_uid": "namespace", "schema_version": 26, "schema_dirty": False},
                   "verification": {"status": "verified", "validation_database_dropped": True,
                                    "checks": ["restore", "schema", "known_tables", "constraints", "row_counts"]}}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "receipt.json"
            path.write_text(json.dumps(receipt))
            path.chmod(0o600)
            with self.assertRaisesRegex(RuntimeError, "VERIFIED_BACKUP_REJECTED"):
                upgrade.verify_backup(Path(directory), "cluster", "namespace", "fixture", owner=os.geteuid())

    def test_complete_backup_receipt_accepts_only_matching_untampered_artifacts(self):
        spec = importlib.util.spec_from_file_location("upgrade", SCRIPT)
        upgrade = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(upgrade)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            artifacts = {}
            for name in ("database.dump", "globals.sql", "role-contract.json", "runtime.json"):
                payload = ("synthetic " + name).encode()
                file = root / name
                file.write_bytes(payload)
                file.chmod(0o600)
                artifacts[name] = {"size": len(payload), "sha256": hashlib.sha256(payload).hexdigest()}
            receipt = {"kind": "orbit-control-plane-backup", "version": 1, "checkpoint_id": "fixture",
                       "verified_at": datetime.now(timezone.utc).isoformat(), "artifacts": artifacts,
                       "source": {"cluster_uid": "cluster", "orbit_system_uid": "namespace", "schema_version": 26, "schema_dirty": False},
                       "verification": {"status": "verified", "validation_database_dropped": True,
                                        "checks": ["restore", "schema", "known_tables", "constraints", "row_counts"]}}
            path = root / "receipt.json"
            path.write_text(json.dumps(receipt))
            path.chmod(0o600)
            upgrade.verify_backup(root, "cluster", "namespace", "fixture", owner=os.geteuid())
            with self.assertRaisesRegex(RuntimeError, "VERIFIED_BACKUP_REJECTED"):
                upgrade.verify_backup(root, "cluster", "namespace", "different", owner=os.geteuid())
            file.write_bytes(b"tampered")
            with self.assertRaisesRegex(RuntimeError, "BACKUP_ARTIFACT_REJECTED"):
                upgrade.verify_backup(root, "cluster", "namespace", "fixture", owner=os.geteuid())


if __name__ == "__main__":
    unittest.main()
