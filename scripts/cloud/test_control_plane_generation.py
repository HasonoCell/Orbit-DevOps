"""从公开命令入口验证 Deployment generation 的并发变更围栏。"""

import contextlib
import copy
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("manage-control-plane.py")
BASE_TEST = Path(__file__).with_name("test_control_plane.py")
NEW_IMAGE = "fixture/backend@sha256:" + "b" * 64


def load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


BASE = load(BASE_TEST, "control_plane_test_fixture")


class GenerationFixture(BASE.KubernetesFixture):
    """Kubernetes 边界按真实语义区分 spec generation 与 status resourceVersion。"""

    def __init__(self, *, roundtrip=False, status_updates=False, interrupt_patch=None):
        super().__init__()
        self.roundtrip = roundtrip
        self.status_updates = status_updates
        self.interrupt_patch = interrupt_patch
        self.interrupted = False
        self.roundtrip_done = False
        for resource in self.deployments.values():
            resource["metadata"]["generation"] = 1

    def __call__(self, command, **kwargs):
        before = None
        name = None
        if self.status_updates and "get" in command and command[command.index("get") + 1] == "deployment":
            observed = command[command.index("get") + 2]
            resource = self.deployments[observed]
            resource["metadata"]["resourceVersion"] = str(int(resource["metadata"]["resourceVersion"]) + 1)
        if "patch" in command:
            name = command[command.index("patch") + 2]
            before = copy.deepcopy(self.deployments[name]["spec"])
            if self.status_updates:
                resource = self.deployments[name]
                resource["metadata"]["resourceVersion"] = str(int(resource["metadata"]["resourceVersion"]) + 1)
            if self.interrupt_patch == "before" and not self.interrupted:
                self.interrupted = True
                raise KeyboardInterrupt()
        result = super().__call__(command, **kwargs)
        if before is not None and self.interrupt_patch == "after" and not self.interrupted:
            self.interrupted = True
            raise KeyboardInterrupt()
        if self.roundtrip and not self.roundtrip_done and "exec" in command and \
                "schema_migrations" not in kwargs.get("input", ""):
            # 外部操作者把 API spec 改开又改回；最终 spec 相同，但 generation 留下证据。
            api = self.deployments["orbit-api"]
            api["spec"]["replicas"] = 1
            api["metadata"]["generation"] += 1
            api["spec"]["replicas"] = 0
            api["metadata"]["generation"] += 1
            self.roundtrip_done = True
        return result


class GenerationFenceTest(unittest.TestCase):
    def command(self, directory, operation):
        upgrade = load(SCRIPT, "control_plane_generation")
        return upgrade.main([
            "--local-context", "kind-orbit-upgrade-20261009",
            "--namespace", "orbit-upgrade-20261009",
            "--cluster-uid", "fixture-cluster",
            "--namespace-uid", "fixture-namespace",
            "--state-dir", directory,
            "--confirm-private-upgrade",
        ] + operation)

    def test_same_spec_roundtrip_by_external_operator_is_rejected(self):
        external = GenerationFixture(roundtrip=True)
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "WORKLOAD_SPEC_CHANGED"):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE])
        self.assertFalse(any("patch" in command and any(name in command for name in BASE.NAMES[1:])
                             for command in external.calls))

    def test_status_only_resource_version_changes_do_not_trip_the_spec_fence(self):
        external = GenerationFixture(status_updates=True)
        output = io.StringIO()
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), \
                contextlib.redirect_stdout(output):
            self.command(directory, ["upgrade", "--image", NEW_IMAGE])
        self.assertIn("CONTROL_PLANE_UPGRADE_READY", output.getvalue())
        self.assertTrue(all(resource["spec"]["template"]["spec"]["containers"][0]["image"] == NEW_IMAGE
                            for resource in external.deployments.values()))

    def test_missing_generation_is_rejected_in_local_context(self):
        external = GenerationFixture()
        external.deployments["orbit-api"]["metadata"].pop("generation")
        with tempfile.TemporaryDirectory() as directory, patch("subprocess.run", external), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(RuntimeError, "WORKLOAD_SPEC_CHANGED"):
                self.command(directory, ["upgrade", "--image", NEW_IMAGE])
        self.assertFalse(any("patch" in command for command in external.calls))

    def test_pending_patch_recovers_at_both_generation_boundaries(self):
        for boundary in ("before", "after"):
            with self.subTest(boundary=boundary):
                external = GenerationFixture(interrupt_patch=boundary)
                with tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()):
                    with patch("subprocess.run", external):
                        with self.assertRaises(KeyboardInterrupt):
                            self.command(directory, ["upgrade", "--image", NEW_IMAGE])
                        self.command(directory, ["rollback"])
                self.assertEqual(external.deployments["orbit-api"]["spec"]["replicas"], 1)
                self.assertFalse(any("patch" in command and any(name in command for name in BASE.NAMES[1:])
                                     for command in external.calls))


if __name__ == "__main__":
    unittest.main()
