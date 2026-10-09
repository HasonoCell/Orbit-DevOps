"""通过公开 CLI 和非特权主机边界验证巡检；不连接云端或读取真实数据。"""
import json
import hashlib
from datetime import datetime, timedelta, timezone
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("check-host-health.py")
CLUSTER_UID = "11111111-1111-4111-8111-111111111111"
NAMESPACE_UID = "22222222-2222-4222-8222-222222222222"
WORKLOADS = ["orbit-api", "orbit-build-worker", "orbit-release-worker", "orbit-pipeline-worker", "orbit-gateway-worker"]
CERT_CONTROLLERS = ["cert-manager", "cert-manager-cainjector", "cert-manager-webhook"]


def ready_workload(name, namespace="orbit-system", kind="Deployment"):
    return {"kind": kind, "metadata": {"name": name, "namespace": namespace, "generation": 4},
            "spec": {"replicas": 1}, "status": {"observedGeneration": 4, "replicas": 1,
            "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}}


class HostHealthCLI(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="orbit-host-health-fixture-")
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        (self.root / "bin").mkdir(mode=0o700)
        (self.root / "etc/orbit-devops").mkdir(parents=True, mode=0o700)
        self.config = {"kind": "orbit-host-health", "version": 1, "cluster_uid": CLUSTER_UID,
                       "namespace_uid": NAMESPACE_UID, "certificate_name": "orbit-management-tls"}
        self.write("etc/orbit-devops/host-health.json", self.config)
        self.responses = {"namespace/kube-system": {"metadata": {"uid": CLUSTER_UID}},
                          "namespace/orbit-system": {"metadata": {"uid": NAMESPACE_UID}}}
        self.responses["nodes/-o"] = {"items": [{"status": {"conditions": [
            {"type": "Ready", "status": "True"}, {"type": "DiskPressure", "status": "False"}]}}]}
        self.responses["deployments/" + ",".join(WORKLOADS)] = {"items": [ready_workload(name) for name in WORKLOADS]}
        self.responses["statefulsets/postgres,redis"] = {"items": [ready_workload(name, kind="StatefulSet") for name in ("postgres", "redis")]}
        self.responses["statefulsets/registry"] = ready_workload("registry", "orbit-builds", "StatefulSet")
        self.responses["sql/schema"] = "26|f"
        self.responses["sql/task_lag"] = {"max_overdue_seconds": 0, "quarantined_total": 0, "attention_total": 0}
        self.responses["deployments/" + ",".join(CERT_CONTROLLERS)] = {"items": [ready_workload(name, "cert-manager") for name in CERT_CONTROLLERS]}
        self.responses["certificate/orbit-management-tls"] = {"metadata": {"name": "orbit-management-tls", "namespace": "orbit-system", "generation": 3},
            "status": {"conditions": [{"type": "Ready", "status": "True", "observedGeneration": 3}]}}
        self.backup = self.root / "var/lib/orbit-devops/backups/checkpoint"
        self.backup.mkdir(parents=True, mode=0o700)
        self.backup.parent.chmod(0o700)
        artifacts = {}
        for name in ("database.dump", "globals.sql", "role-contract.json", "runtime.json"):
            content = ("fixture-only-" + name).encode()
            (self.backup / name).write_bytes(content)
            (self.backup / name).chmod(0o600)
            artifacts[name] = {"size": len(content), "sha256": hashlib.sha256(content).hexdigest()}
        self.receipt = {"kind": "orbit-control-plane-backup", "version": 1, "checkpoint_id": "checkpoint",
                        "checkpoint_uid": "33333333-3333-4333-8333-333333333333",
                        "verified_at": (datetime.now(timezone.utc) - timedelta(hours=30)).isoformat(),
                        "source": {"cluster_uid": CLUSTER_UID, "orbit_system_uid": NAMESPACE_UID,
                                   "schema_version": 26, "schema_dirty": False},
                        "artifacts": artifacts,
                        "verification": {"status": "verified", "validation_database_dropped": True,
                                         "checks": ["restore", "schema", "known_tables", "constraints", "row_counts"]}}
        self.capacity = {path: {"blocks": 200_000_000, "bfree": 100_000_000, "bavail": 100_000_000,
                                "frsize": 4096, "files": 1_000_000, "ffree": 700_000}
                         for path in ("/", "var/lib/rancher/k3s", "var/lib/orbit-devops/backups")}
        executable = self.root / "bin/k3s"
        executable.write_text("#!" + sys.executable + "\n" + '''import json, pathlib, sys
root = pathlib.Path(__file__).resolve().parent.parent
args = sys.argv[1:]
responses = json.loads((root / "responses.json").read_text())
if "exec" in args:
    query = sys.stdin.read()
    if "BEGIN READ ONLY;" not in query or "COMMIT;" not in query:
        print("PRIVATE_RAW_SQL_ERROR", file=sys.stderr); sys.exit(1)
    if "schema_migrations" in query:
        print(responses["sql/schema"]); sys.exit(0)
    if "max_overdue_seconds" in query:
        print(json.dumps(responses["sql/task_lag"])); sys.exit(0)
    print("PRIVATE_RAW_SQL_ERROR", file=sys.stderr); sys.exit(1)
if "get" not in args:
    print("PRIVATE_RAW_COMMAND_ERROR", file=sys.stderr); sys.exit(1)
start = args.index("get")
# kubectl 接受多个独立 name 参数，不把一个 CSV 名称参数当作资源列表。
names = []
for argument in args[start + 2:]:
    if argument.startswith("-"):
        break
    names.append(argument)
if any("," in name for name in names):
    print("PRIVATE_RAW_COMMAND_ERROR", file=sys.stderr); sys.exit(1)
key = args[start + 1] + "/" + (",".join(names) if names else "-o")
if key not in responses:
    print("PRIVATE_RAW_COMMAND_ERROR", file=sys.stderr); sys.exit(1)
print(json.dumps(responses[key]))
''')
        executable.chmod(0o700)
        self.services_ok = {"k3s.service": True, "orbit-private-network.service": True, "ssh.service": True}
        executable = self.root / "bin/systemctl"
        executable.write_text("#!" + sys.executable + "\n" + '''import json, pathlib, sys
root = pathlib.Path(__file__).resolve().parent.parent
services = json.loads((root / "services.json").read_text())
args = sys.argv[1:]
if args[:2] != ["is-active", "--quiet"] or not args[2:] or any(name not in services for name in args[2:]):
    print("PRIVATE_RAW_COMMAND_ERROR", file=sys.stderr); sys.exit(1)
# systemctl 多单位 is-active 的真实语义：只要一个 active 就返回成功。
sys.exit(0 if any(services[name] for name in args[2:]) else 1)
''')
        executable.chmod(0o700)

    def tearDown(self):
        self.temporary.cleanup()

    def write(self, relative, value):
        path = self.root / relative
        path.write_text(json.dumps(value))
        path.chmod(0o600)

    def run_cli(self, *arguments):
        self.write("responses.json", self.responses)
        self.write("services.json", self.services_ok)
        self.write("var/lib/orbit-devops/backups/checkpoint/receipt.json", self.receipt)
        self.write("capacity.json", self.capacity)
        return subprocess.run([sys.executable, "-B", str(SCRIPT), "--fixture-root", str(self.root), *arguments],
                              text=True, capture_output=True, timeout=10)

    def test_pinned_identity_is_reported_without_identifiers(self):
        result = self.run_cli()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.splitlines(), ["HOST_HEALTH_OK check=identity", "HOST_HEALTH_OK check=node",
                                                     "HOST_HEALTH_OK check=system_services", "HOST_HEALTH_OK check=workloads",
                                                     "HOST_HEALTH_OK check=dependencies",
                                                     "HOST_HEALTH_OK check=backup",
                                                     "HOST_HEALTH_OK check=capacity",
                                                     "HOST_HEALTH_OK check=task_lag",
                                                     "HOST_HEALTH_OK check=certificate",
                                                     "HOST_HEALTH_READY"])
        self.assertEqual(result.stderr, "")

    def test_certificate_not_ready_fails_without_secret_reads(self):
        self.responses["certificate/orbit-management-tls"]["status"]["conditions"][0]["status"] = "False"
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=certificate")
        self.assertEqual(result.stderr, "")

    def test_due_task_stuck_for_31_minutes_fails(self):
        self.responses["sql/task_lag"]["max_overdue_seconds"] = 1860
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=task_lag")
        self.assertEqual(result.stderr, "")

    def test_backup_filesystem_below_20_gib_is_not_healthy(self):
        self.capacity["var/lib/orbit-devops/backups"]["bavail"] = 1_000_000
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=capacity")
        self.assertEqual(result.stderr, "")

    def test_backup_older_than_48_hours_fails(self):
        self.receipt["verified_at"] = (datetime.now(timezone.utc) - timedelta(hours=49)).isoformat()
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=backup")
        self.assertEqual(result.stderr, "")

    def test_unobserved_worker_rollout_is_not_healthy(self):
        self.responses["deployments/" + ",".join(WORKLOADS)]["items"][2]["status"]["observedGeneration"] = 3
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=workloads")
        self.assertNotIn("PRIVATE_RAW", result.stdout + result.stderr)

    def test_missing_or_wrong_workload_member_is_not_healthy(self):
        for wrong_member in (False, True):
            with self.subTest(wrong_member=wrong_member):
                names = WORKLOADS[:-1] + (["unexpected-worker"] if wrong_member else [])
                self.responses["deployments/" + ",".join(WORKLOADS)] = {
                    "items": [ready_workload(name) for name in names]}

                result = self.run_cli()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=workloads")
                self.assertEqual(result.stderr, "")

    def test_unready_registry_fails_dependency_health(self):
        self.responses["statefulsets/registry"]["status"]["readyReplicas"] = 0
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=dependencies")
        self.assertEqual(result.stderr, "")

    def test_inactive_private_network_is_not_healthy(self):
        self.services_ok["orbit-private-network.service"] = False
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines(), ["HOST_HEALTH_OK check=identity", "HOST_HEALTH_OK check=node",
                                                     "HOST_HEALTH_FAILED check=system_services"])
        self.assertEqual(result.stderr, "")

    def test_not_ready_node_fails_without_raw_data(self):
        self.responses["nodes/-o"]["items"][0]["status"]["conditions"][0]["status"] = "False"
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines(), ["HOST_HEALTH_OK check=identity", "HOST_HEALTH_FAILED check=node"])
        self.assertEqual(result.stderr, "")

    def test_wrong_cluster_identity_stops_before_any_health_success(self):
        self.responses["namespace/kube-system"]["metadata"]["uid"] = "44444444-4444-4444-8444-444444444444"
        result = self.run_cli()
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "HOST_HEALTH_FAILED check=identity\n", ""))

    def test_private_config_cannot_be_a_symlink_or_public_file(self):
        config = self.root / "etc/orbit-devops/host-health.json"
        for mutation in ("public", "symlink"):
            with self.subTest(mutation=mutation):
                if mutation == "public":
                    config.chmod(0o644)
                else:
                    original = self.root / "private-copy.json"
                    config.rename(original)
                    config.symlink_to(original)
                result = self.run_cli()
                self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "HOST_HEALTH_FAILED check=identity\n", ""))

    def test_modified_sealed_backup_fails(self):
        artifact = self.backup / "database.dump"
        artifact.write_bytes(b"tampered-local-test-only")
        result = self.run_cli()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=backup")
        self.assertEqual(result.stderr, "")

    def test_quarantined_or_manual_attention_tasks_fail(self):
        for key in ("quarantined_total", "attention_total"):
            with self.subTest(key=key):
                self.responses["sql/task_lag"] = {"max_overdue_seconds": 0, "quarantined_total": 0, "attention_total": 0}
                self.responses["sql/task_lag"][key] = 1
                result = self.run_cli()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=task_lag")
                self.assertEqual(result.stderr, "")

    def test_full_disk_or_inodes_fail_even_with_available_bytes(self):
        for key in ("bfree", "ffree"):
            with self.subTest(key=key):
                original = dict(self.capacity["/"])
                if key == "bfree":
                    self.capacity["/"]["bfree"] = 30_000_000
                    self.capacity["/"]["bavail"] = 30_000_000
                else:
                    self.capacity["/"]["ffree"] = 100_000
                result = self.run_cli()
                self.assertEqual(result.returncode, 1)
                self.assertEqual(result.stdout.splitlines()[-1], "HOST_HEALTH_FAILED check=capacity")
                self.capacity["/"] = original

    def test_unknown_external_output_never_reaches_monitor(self):
        del self.responses["nodes/-o"]
        result = self.run_cli()
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "HOST_HEALTH_OK check=identity\nHOST_HEALTH_FAILED check=node\n", ""))

    def test_no_production_invocation_under_nonroot(self):
        result = subprocess.run([sys.executable, "-B", str(SCRIPT)], text=True, capture_output=True, timeout=10)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "HOST_HEALTH_FAILED check=identity\n", ""))

    def test_malformed_cli_has_only_fixed_failure_output(self):
        result = self.run_cli("--fixture-root")
        self.assertEqual((result.returncode, result.stdout, result.stderr), (1, "HOST_HEALTH_FAILED check=identity\n", ""))


if __name__ == "__main__":
    unittest.main()
