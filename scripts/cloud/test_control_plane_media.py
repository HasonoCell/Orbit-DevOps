"""以真实文件验证控制面 OCI 回退介质的持久化与回载契约。"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("manage-control-plane.py")
OLD_IMAGE = "registry.example/orbit/control-plane@sha256:" + "a" * 64
ARCHIVE_BYTES = b"fixture OCI archive\n"


def load_module():
    spec = importlib.util.spec_from_file_location("control_plane_media", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def rollback_state(upgrade, archive, image=OLD_IMAGE):
    return {
        "operation_id": "operation-2",
        "rollback_archive": {
            "name": archive.name,
            "sha256": hashlib.sha256(ARCHIVE_BYTES).hexdigest(),
            "size": len(ARCHIVE_BYTES),
            "image": image,
        },
        "before": {
            name: {"spec": {"template": {"spec": {"containers": [{"image": OLD_IMAGE}]}}}}
            for name in upgrade.NAMES
        },
    }


class RollbackMediaTest(unittest.TestCase):
    def test_save_persists_and_reimports_exact_digest_in_kubernetes_namespace(self):
        upgrade = load_module()
        calls = []
        synced_types = []
        real_fsync = os.fsync

        def external(command, **kwargs):
            calls.append(command)
            if "export" in command:
                Path(command[-2]).write_bytes(ARCHIVE_BYTES)
                output = ""
            elif "inspecti" in command:
                output = json.dumps({"status": {"repoDigests": [OLD_IMAGE]}})
            else:
                output = ""
            return subprocess.CompletedProcess(command, 0, output, "")

        def durable(fd):
            mode = os.fstat(fd).st_mode
            synced_types.append("file" if stat.S_ISREG(mode) else "directory" if stat.S_ISDIR(mode) else "other")
            real_fsync(fd)

        with tempfile.TemporaryDirectory() as raw_directory:
            directory = Path(raw_directory)
            with patch.object(subprocess, "run", side_effect=external), patch.object(upgrade.os, "fsync", side_effect=durable):
                saved = upgrade.save_rollback_image(directory, "operation-1", OLD_IMAGE)

            archive = directory / "operation-1-rollback.tar"
            info = archive.lstat()
            self.assertTrue(stat.S_ISREG(info.st_mode))
            self.assertEqual(info.st_uid, os.geteuid())
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o600)
            self.assertEqual(saved, {
                "name": archive.name,
                "sha256": hashlib.sha256(ARCHIVE_BYTES).hexdigest(),
                "size": len(ARCHIVE_BYTES),
                "image": OLD_IMAGE,
            })

        self.assertEqual(["export", "import", "inspecti"], [
            "export" if "export" in command else "import" if "import" in command else "inspecti"
            for command in calls
        ])
        self.assertEqual(["file", "directory"], synced_types)
        self.assertTrue(all("--namespace=k8s.io" in command for command in calls[:2]))
        self.assertFalse(any({"remove", "delete", "rm"} & set(command) for command in calls))

    def test_load_accepts_owned_exact_archive_and_expected_before_digest(self):
        upgrade = load_module()
        calls = []

        def external(command, **kwargs):
            calls.append(command)
            output = json.dumps({"status": {"repoDigests": [OLD_IMAGE]}}) if "inspecti" in command else ""
            return subprocess.CompletedProcess(command, 0, output, "")

        with tempfile.TemporaryDirectory() as raw_directory:
            directory = Path(raw_directory)
            archive = directory / "operation-2-rollback.tar"
            archive.write_bytes(ARCHIVE_BYTES)
            archive.chmod(0o600)
            state = rollback_state(upgrade, archive)
            with patch.object(subprocess, "run", side_effect=external):
                upgrade.load_rollback_image(SimpleNamespace(local_context=None), state, directory)

        self.assertEqual(["import", "inspecti"], [
            "import" if "import" in command else "inspecti" for command in calls
        ])
        self.assertIn("--namespace=k8s.io", calls[0])
        self.assertEqual(OLD_IMAGE, calls[1][-1])

    def test_load_rejects_archive_image_not_equal_to_expected_before(self):
        upgrade = load_module()
        different_image = "registry.example/orbit/control-plane@sha256:" + "b" * 64
        with tempfile.TemporaryDirectory() as raw_directory:
            archive = Path(raw_directory) / "operation-2-rollback.tar"
            archive.write_bytes(ARCHIVE_BYTES)
            archive.chmod(0o600)
            state = rollback_state(upgrade, archive, different_image)
            with patch.object(subprocess, "run") as external:
                with self.assertRaisesRegex(RuntimeError, "^ROLLBACK_ARCHIVE_REJECTED$"):
                    upgrade.load_rollback_image(SimpleNamespace(local_context=None), state, archive.parent)
            external.assert_not_called()

    def test_load_rejects_wrong_type_owner_mode_size_or_hash_before_import(self):
        cases = ("type", "owner", "mode", "size", "hash")
        for case in cases:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as raw_directory:
                upgrade = load_module()
                directory = Path(raw_directory)
                archive = directory / "operation-2-rollback.tar"
                if case == "type":
                    archive.mkdir()
                else:
                    archive.write_bytes(ARCHIVE_BYTES)
                    archive.chmod(0o644 if case == "mode" else 0o600)
                state = rollback_state(upgrade, archive)
                if case == "size":
                    state["rollback_archive"]["size"] += 1
                elif case == "hash":
                    state["rollback_archive"]["sha256"] = "0" * 64
                owner = os.geteuid() + 1 if case == "owner" else os.geteuid()
                with patch.object(subprocess, "run") as external, patch.object(upgrade.os, "geteuid", return_value=owner):
                    with self.assertRaisesRegex(RuntimeError, "^ROLLBACK_ARCHIVE_REJECTED$"):
                        upgrade.load_rollback_image(SimpleNamespace(local_context=None), state, directory)
                external.assert_not_called()

    def test_load_rejects_repo_digest_near_match_after_import(self):
        upgrade = load_module()
        calls = []

        def external(command, **kwargs):
            calls.append(command)
            output = json.dumps({"status": {"repoDigests": [OLD_IMAGE + "-suffix"]}}) if "inspecti" in command else ""
            return subprocess.CompletedProcess(command, 0, output, "")

        with tempfile.TemporaryDirectory() as raw_directory:
            directory = Path(raw_directory)
            archive = directory / "operation-2-rollback.tar"
            archive.write_bytes(ARCHIVE_BYTES)
            archive.chmod(0o600)
            with patch.object(subprocess, "run", side_effect=external):
                with self.assertRaisesRegex(RuntimeError, "^ROLLBACK_IMAGE_CACHE_REJECTED$"):
                    upgrade.load_rollback_image(
                        SimpleNamespace(local_context=None), rollback_state(upgrade, archive), directory
                    )
        self.assertEqual(2, len(calls))

    def test_save_rejects_non_regular_export_before_import(self):
        upgrade = load_module()
        calls = []

        def external(command, **kwargs):
            calls.append(command)
            Path(command[-2]).mkdir()
            return subprocess.CompletedProcess(command, 0, "", "")

        with tempfile.TemporaryDirectory() as raw_directory:
            with patch.object(subprocess, "run", side_effect=external):
                with self.assertRaisesRegex(RuntimeError, "^ROLLBACK_ARCHIVE_REJECTED$"):
                    upgrade.save_rollback_image(Path(raw_directory), "operation-3", OLD_IMAGE)
        self.assertEqual(1, len(calls))
        self.assertIn("export", calls[0])

    def test_save_rejects_repo_digest_near_match(self):
        upgrade = load_module()

        def external(command, **kwargs):
            if "export" in command:
                Path(command[-2]).write_bytes(ARCHIVE_BYTES)
                output = ""
            elif "inspecti" in command:
                output = json.dumps({"status": {"repoDigests": [OLD_IMAGE + "-suffix"]}})
            else:
                output = ""
            return subprocess.CompletedProcess(command, 0, output, "")

        with tempfile.TemporaryDirectory() as raw_directory:
            with patch.object(subprocess, "run", side_effect=external):
                with self.assertRaisesRegex(RuntimeError, "^ROLLBACK_IMAGE_CACHE_REJECTED$"):
                    upgrade.save_rollback_image(Path(raw_directory), "operation-4", OLD_IMAGE)


if __name__ == "__main__":
    unittest.main()
