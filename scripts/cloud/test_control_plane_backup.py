"""从公开命令函数验证控制面备份的失败关闭与恢复验收契约。"""

import contextlib
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
import uuid


SCRIPT = Path(__file__).with_name("backup-control-plane.py")
KNOWN_TABLES = (
    "access_hosts", "access_routes", "access_secret_bindings", "applications", "audit_records",
    "auth_providers", "auth_rate_limits", "auth_sessions", "build_attempts", "build_dispatches",
    "build_operations", "builds", "delivery_pipeline_revisions", "delivery_pipelines", "delivery_runs",
    "deployment_targets", "dispatch_preparations", "external_identities", "idempotency_records",
    "identity_control", "image_artifacts", "internal_event_outbox", "legacy_project_members",
    "local_credentials", "oidc_transactions", "operation_attempts", "operation_dispatches",
    "project_gateway_sync", "project_members", "projects", "release_operations", "releases",
    "schema_migrations", "users", "webhook_deliveries",
)


def load_script():
    spec = importlib.util.spec_from_file_location("backup_control_plane", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class UnusedBoundary:
    """无效参数必须在触碰主机或集群边界前失败。"""

    def __init__(self, root):
        self.backup_root = Path(root)

    def __getattr__(self, name):
        raise AssertionError(f"unexpected boundary access: {name}")


class BackupBoundaryFixture:
    """模拟命令执行边界，文件权限与原子发布仍由公开流程真实执行。"""

    def __init__(self, root, *, cluster_uid="cluster-fixture", database_exists=False,
                 changed_row_count_call=None, expected_statefulset=None,
                 ordered_rolebinding=False):
        self.backup_root = Path(root)
        self.calls = []
        self.cluster_uid = cluster_uid
        self.database_exists = database_exists
        self.changed_row_count_call = changed_row_count_call
        self.expected_statefulset = expected_statefulset
        self.ordered_rolebinding = ordered_rolebinding
        self.row_count_calls = 0

    def euid(self):
        return 0

    def now(self):
        return "2026-10-09T08:30:00Z"

    def checkpoint_uid(self):
        return "018f0000-0000-7000-8000-000000000001"

    def validate_backup_root(self, path):
        self.assert_path = path

    def publish(self, staging, destination):
        os.rename(staging, destination)

    def run(self, command, *, stdin_path=None, stdout_path=None):
        self.calls.append((list(command), stdin_path, stdout_path))
        joined = " ".join(command)
        payload = b""
        if "systemctl is-active" in joined:
            payload = b""
        elif "get nodes" in joined:
            payload = json.dumps({"items": [{"metadata": {"name": "private-node", "labels": {
                "orbit-devops.dev/environment": "private-test"}}, "status": {"conditions": [
                    {"type": "Ready", "status": "True"}]}}]}).encode()
        elif "get namespace" in joined:
            name = command[command.index("namespace") + 1]
            uid = self.cluster_uid if name == "kube-system" else (
                "namespace-fixture" if name == "orbit-system" else f"{name}-uid")
            payload = json.dumps({"metadata": {"name": name, "uid": uid, "labels": {
                "app.kubernetes.io/managed-by": "orbit-devops",
                "orbit-devops.dev/environment": "private-test"}}}).encode()
        elif "get configmap orbit-cluster-identity" in joined:
            payload = json.dumps({"data": {
                "ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID": "cluster-fixture"}}).encode()
        elif "get" in command and command[command.index("get") + 1] == "statefulset":
            name = command[command.index("statefulset") + 1]
            if self.expected_statefulset is not None:
                self.assert_equal(name, self.expected_statefulset)
            payload = json.dumps({"metadata": {"uid": "postgres-fixture"}, "spec": {
                "replicas": 1, "template": {"spec": {"containers": [{"name": "postgres",
                    "image": "postgres@sha256:" + "a" * 64}]}}}}).encode()
        elif "get configmaps" in joined or "get secrets" in joined:
            resource = "configmaps" if "get configmaps" in joined else "secrets"
            namespace = command[command.index("--namespace") + 1]
            kind = "ConfigMap" if resource == "configmaps" else "Secret"
            payload = json.dumps({"apiVersion": "v1", "items": [{"apiVersion": "v1", "kind": kind,
                "metadata": {"name": f"fixture-{resource}", "namespace": namespace,
                    "uid": "volatile", "resourceVersion": "9"},
                "data": {"opaque": "fixture-secret"} if kind == "Secret" else {"MODE": "fixture"}}]}).encode()
        elif "get rolebindings.rbac.authorization.k8s.io" in joined and self.ordered_rolebinding:
            namespace = command[command.index("--namespace") + 1]
            payload = json.dumps({"apiVersion": "v1", "items": [{
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "RoleBinding",
                "metadata": {"name": "fixture-order", "namespace": namespace},
                "subjects": [
                    {"kind": "User", "name": "first-subject"},
                    {"kind": "ServiceAccount", "name": "second-subject", "namespace": namespace},
                ],
            }]}).encode()
        elif "get " in joined and "-o json" in joined:
            payload = json.dumps({"apiVersion": "v1", "items": []}).encode()
        elif "postgres --version" in joined:
            payload = b"postgres (PostgreSQL) 17.2\n"
        elif "pg_dumpall" in joined:
            payload = b"-- PostgreSQL globals without passwords\n"
        elif "pg_dump " in joined:
            payload = b"PGDMPfixture"
        elif "pg_restore" in joined:
            payload = b""
        elif "orbit-backup:database-exists" in joined:
            payload = b"1\n" if self.database_exists else b"0\n"
        elif "orbit-backup:schema" in joined:
            payload = b"26|f\n"
        elif "orbit-backup:roles" in joined:
            payload = json.dumps([{"name": "orbitdevops", "can_login": True,
                                   "superuser": True, "create_role": True,
                                   "create_db": True}]).encode()
        elif "orbit-backup:owner" in joined:
            payload = b"orbitdevops\n"
        elif "orbit-backup:tables" in joined:
            payload = json.dumps(list(KNOWN_TABLES)).encode()
        elif "orbit-backup:constraints" in joined:
            payload = json.dumps([f"{name}:{name}_contract:p:true" for name in KNOWN_TABLES]).encode()
        elif "orbit-backup:row-counts" in joined:
            self.row_count_calls += 1
            projects = 3 if self.row_count_calls == self.changed_row_count_call else 2
            payload = json.dumps({name: (projects if name == "projects" else 1)
                                  for name in KNOWN_TABLES}).encode()
        elif "orbit-backup:create-database" in joined or "orbit-backup:drop-database" in joined:
            payload = b""
        else:
            raise AssertionError(f"unexpected external command: {joined}")
        if stdout_path is not None:
            Path(stdout_path).write_bytes(payload)
            os.chmod(stdout_path, 0o600)
            return b""
        return payload

    @staticmethod
    def assert_equal(actual, expected):
        if actual != expected:
            raise AssertionError(f"expected {expected}, got {actual}")


class PostgreSQL17Boundary(BackupBoundaryFixture):
    """Kubernetes 库存使用 fixture，数据库命令交给真实 PostgreSQL 17 容器。"""

    def __init__(self, root, container):
        super().__init__(root, expected_statefulset="postgres")
        self.container = container

    def run(self, command, *, stdin_path=None, stdout_path=None):
        if "exec" not in command or "postgres-0" not in command:
            return super().run(command, stdin_path=stdin_path, stdout_path=stdout_path)
        separator = command.index("--")
        if stdin_path is not None and "-i" not in command[:separator]:
            raise AssertionError("kubectl exec did not attach stdin")
        postgres_command = list(command[separator + 1:])
        docker_command = ["docker", "exec"]
        if stdin_path is not None:
            docker_command.append("-i")
        docker_command.extend((self.container, *postgres_command))
        input_handle = open(stdin_path, "rb") if stdin_path is not None else None
        output_handle = open(stdout_path, "xb") if stdout_path is not None else None
        try:
            result = subprocess.run(
                docker_command,
                stdin=input_handle,
                stdout=output_handle if output_handle is not None else subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=300,
            )
            if result.returncode != 0:
                raise AssertionError("PostgreSQL 17 fixture command failed")
            if output_handle is not None:
                output_handle.flush()
                os.fsync(output_handle.fileno())
                os.chmod(stdout_path, 0o600)
                return b""
            return result.stdout
        finally:
            if output_handle is not None:
                output_handle.close()
            if input_handle is not None:
                input_handle.close()


@contextlib.contextmanager
def migrated_postgres17(test_case):
    """启动无真实数据、无端口暴露的 PG17，并执行仓库全部迁移。"""
    image = subprocess.run(
        ["docker", "image", "inspect", "postgres:17-alpine"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if image.returncode != 0:
        test_case.skipTest("postgres:17-alpine is not available locally")
    name = f"orbit-backup-smoke-{os.getpid()}-{uuid.uuid4().hex[:8]}"
    started = subprocess.run(
        ["docker", "run", "--rm", "--detach", "--name", name,
         "--env", "POSTGRES_USER=orbitdevops", "--env", "POSTGRES_DB=orbitdevops",
         "--env", "POSTGRES_HOST_AUTH_METHOD=trust", "postgres:17-alpine"],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if started.returncode != 0:
        test_case.fail("could not start the PostgreSQL 17 fixture")
    try:
        psql = ["docker", "exec", "-i", name, "psql", "-X", "--set=ON_ERROR_STOP=1",
                "--username=orbitdevops", "--dbname=orbitdevops", "--no-password"]
        for _ in range(100):
            ready = subprocess.run(
                psql,
                input=b"SELECT 1;\n",
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            if ready.returncode == 0:
                break
            time.sleep(0.1)
        else:
            test_case.fail("PostgreSQL 17 fixture did not become ready")

        schema_table = subprocess.run(
            psql,
            input=b"CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL);\n",
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if schema_table.returncode != 0:
            test_case.fail("could not create the migration fixture")
        migrations = SCRIPT.parents[2] / "internal/platform/database/migrations"
        for migration in sorted(migrations.glob("*.up.sql")):
            applied = subprocess.run(
                psql,
                input=migration.read_bytes(),
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            if applied.returncode != 0:
                test_case.fail(f"migration fixture failed at {migration.name}")
        version = subprocess.run(
            psql,
            input=b"INSERT INTO schema_migrations (version, dirty) VALUES (26, false);\n",
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if version.returncode != 0:
            test_case.fail("could not finalize the migration fixture")
        yield name
    finally:
        subprocess.run(
            ["docker", "container", "rm", "--force", name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )


class ControlPlaneBackupTest(unittest.TestCase):
    @staticmethod
    def argv(checkpoint_id="before_upgrade"):
        return [
            "--confirm-private-control-plane",
            "--cluster-uid",
            "cluster-fixture",
            "--namespace-uid",
            "namespace-fixture",
            "create",
            checkpoint_id,
        ]

    def test_invalid_checkpoint_id_is_rejected_without_external_effects(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(backup.BackupError, "invalid_checkpoint_id"):
                backup.execute(
                    [
                        "--confirm-private-control-plane",
                        "--cluster-uid",
                        "cluster-fixture",
                        "--namespace-uid",
                        "namespace-fixture",
                        "create",
                        "../escape",
                    ],
                    UnusedBoundary(directory),
                )

    def test_verified_backup_is_published_with_minimal_runtime_and_receipt(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory)
            result = backup.execute(self.argv(), boundary)

            checkpoint = Path(directory) / "before_upgrade"
            self.assertEqual(result, "CONTROL_PLANE_BACKUP_VERIFIED before_upgrade")
            self.assertTrue(checkpoint.is_dir())
            self.assertEqual(checkpoint.stat().st_mode & 0o777, 0o700)
            for name in ("database.dump", "globals.sql", "role-contract.json", "runtime.json", "receipt.json"):
                self.assertEqual((checkpoint / name).stat().st_mode & 0o777, 0o600)

            runtime = json.loads((checkpoint / "runtime.json").read_text())
            for namespace in ("orbit-system", "orbit-apps", "orbit-builds"):
                self.assertIn("configmaps", runtime["namespaces"][namespace])
                self.assertIn("secrets", runtime["namespaces"][namespace])
            self.assertNotIn("resourceVersion", (checkpoint / "runtime.json").read_text())

            receipt = json.loads((checkpoint / "receipt.json").read_text())
            self.assertEqual(receipt["kind"], "orbit-control-plane-backup")
            self.assertEqual(receipt["version"], 1)
            self.assertEqual(receipt["checkpoint_id"], "before_upgrade")
            self.assertEqual(receipt["source"]["cluster_uid"], "cluster-fixture")
            self.assertEqual(receipt["source"]["orbit_system_uid"], "namespace-fixture")
            self.assertEqual(receipt["source"]["schema_version"], 26)
            self.assertFalse(receipt["source"]["schema_dirty"])
            self.assertEqual(receipt["verification"]["status"], "verified")
            self.assertTrue(receipt["verification"]["validation_database_dropped"])
            self.assertFalse(receipt["claims"]["off_host"])
            self.assertFalse(receipt["claims"]["disaster_recovery"])
            self.assertFalse(receipt["claims"]["role_passwords_included"])
            self.assertFalse(receipt["claims"]["pvc_data_included"])
            self.assertFalse(receipt["claims"]["registry_blobs_included"])
            self.assertFalse(receipt["claims"]["k3s_datastore_included"])
            self.assertNotIn("DATABASE_URL", (checkpoint / "receipt.json").read_text())
            self.assertEqual(set(receipt["artifacts"]), {
                "database.dump", "globals.sql", "role-contract.json", "runtime.json"})
            for artifact in receipt["artifacts"].values():
                self.assertRegex(artifact["sha256"], r"^[0-9a-f]{64}$")
                self.assertGreater(artifact["size"], 0)
            for name, artifact in receipt["artifacts"].items():
                payload = (checkpoint / name).read_bytes()
                self.assertEqual(artifact["size"], len(payload))
                self.assertEqual(artifact["sha256"], hashlib.sha256(payload).hexdigest())

            flattened = [" ".join(call[0]) for call in boundary.calls]
            self.assertTrue(any("pg_dumpall" in command and "--no-role-passwords" in command
                                for command in flattened))
            self.assertTrue(any("orbit-backup:create-database" in command and "TEMPLATE template0" in command
                                for command in flattened))
            self.assertTrue(any("pg_restore" in command for command in flattened))
            self.assertTrue(any("orbit-backup:drop-database" in command for command in flattened))

    def test_explicit_cluster_uid_mismatch_fails_before_checkpoint_or_dump(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory, cluster_uid="different-cluster")
            with self.assertRaisesRegex(backup.BackupError, "cluster_identity_rejected"):
                backup.execute(self.argv(), boundary)
            self.assertEqual(list(Path(directory).iterdir()), [])
            self.assertFalse(any("pg_dump" in " ".join(call[0]) for call in boundary.calls))

    def test_guard_uses_the_deployed_postgres_statefulset_name(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory, expected_statefulset="postgres")
            self.assertEqual(
                backup.execute(self.argv(), boundary),
                "CONTROL_PLANE_BACKUP_VERIFIED before_upgrade",
            )

    def test_real_postgres17_dump_restore_and_schema_contract(self):
        backup = load_script()
        with migrated_postgres17(self) as container, tempfile.TemporaryDirectory() as directory:
            boundary = PostgreSQL17Boundary(directory, container)
            self.assertEqual(
                backup.execute(self.argv("pg17_smoke"), boundary),
                "CONTROL_PLANE_BACKUP_VERIFIED pg17_smoke",
            )
            checkpoint = Path(directory) / "pg17_smoke"
            self.assertTrue((checkpoint / "database.dump").is_file())
            receipt = json.loads((checkpoint / "receipt.json").read_text())
            self.assertEqual(receipt["verification"]["status"], "verified")
            self.assertTrue(receipt["verification"]["validation_database_dropped"])

    def test_runtime_normalization_preserves_nested_array_order(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory, ordered_rolebinding=True)
            backup.execute(self.argv("ordered_runtime"), boundary)
            runtime = json.loads((Path(directory) / "ordered_runtime" / "runtime.json").read_text())
            rolebinding = runtime["namespaces"]["orbit-system"][
                "rolebindings.rbac.authorization.k8s.io"][0]
            self.assertEqual(
                [subject["name"] for subject in rolebinding["subjects"]],
                ["first-subject", "second-subject"],
            )

    def test_existing_checkpoint_is_never_reused_or_overwritten(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            checkpoint = Path(directory) / "before_upgrade"
            checkpoint.mkdir()
            marker = checkpoint / "operator-owned"
            marker.write_text("preserve")
            boundary = BackupBoundaryFixture(directory)
            with self.assertRaisesRegex(backup.BackupError, "checkpoint_exists"):
                backup.execute(self.argv(), boundary)
            self.assertEqual(marker.read_text(), "preserve")
            self.assertFalse(any("pg_dump" in " ".join(call[0]) for call in boundary.calls))

    def test_online_row_count_change_aborts_before_validation_database(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory, changed_row_count_call=2)
            with self.assertRaisesRegex(backup.BackupError, "source_changed_during_backup"):
                backup.execute(self.argv(), boundary)
            commands = [" ".join(call[0]) for call in boundary.calls]
            self.assertFalse(any("orbit-backup:create-database" in command for command in commands))
            self.assertFalse(any("orbit-backup:drop-database" in command for command in commands))
            self.assertFalse((Path(directory) / "before_upgrade").exists())
            self.assertEqual(len(list(Path(directory).glob(".before_upgrade.*.incomplete"))), 1)

    def test_restore_mismatch_never_drops_validation_database_or_publishes(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory, changed_row_count_call=3)
            with self.assertRaisesRegex(backup.BackupError, "restore_verification_failed"):
                backup.execute(self.argv(), boundary)
            commands = [" ".join(call[0]) for call in boundary.calls]
            self.assertTrue(any("orbit-backup:create-database" in command for command in commands))
            self.assertFalse(any("orbit-backup:drop-database" in command for command in commands))
            self.assertFalse((Path(directory) / "before_upgrade").exists())

    def test_existing_validation_database_is_not_modified_or_deleted(self):
        backup = load_script()
        with tempfile.TemporaryDirectory() as directory:
            boundary = BackupBoundaryFixture(directory, database_exists=True)
            with self.assertRaisesRegex(backup.BackupError, "validation_database_exists"):
                backup.execute(self.argv(), boundary)
            commands = [" ".join(call[0]) for call in boundary.calls]
            self.assertFalse(any("pg_dump" in command for command in commands))
            self.assertFalse(any("orbit-backup:drop-database" in command for command in commands))

    def test_cli_failure_is_sanitized(self):
        result = subprocess.run(
            ["python3", str(SCRIPT), "--confirm-private-control-plane", "--cluster-uid", "secret/value",
             "--namespace-uid", "namespace-fixture", "create", "before_upgrade"],
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")
        self.assertEqual(result.stderr, "CONTROL_PLANE_BACKUP_FAILED invalid_identity\n")
        self.assertNotIn("secret/value", result.stderr)


if __name__ == "__main__":
    unittest.main()
