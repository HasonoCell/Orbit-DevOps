#!/usr/bin/env python3
"""在固定私有 K3s 控制面上创建并恢复验证不可覆盖的本机备份。"""

import argparse
import ctypes
import datetime
import errno
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import uuid


SAFE_ID = re.compile(r"[a-z0-9][a-z0-9_]{0,31}\Z")
SAFE_UID = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")
BACKUP_ROOT = Path("/var/lib/orbit-devops/backups")
KUBE = (
    "/usr/local/bin/k3s",
    "kubectl",
    "--kubeconfig",
    "/etc/rancher/k3s/k3s.yaml",
    "--request-timeout=15s",
)
NAMESPACES = ("orbit-system", "orbit-apps", "orbit-builds")
PRIVATE_LABELS = {
    "app.kubernetes.io/managed-by": "orbit-devops",
    "orbit-devops.dev/environment": "private-test",
}
NAMESPACED_RESOURCES = (
    "configmaps",
    "secrets",
    "serviceaccounts",
    "services",
    "persistentvolumeclaims",
    "deployments.apps",
    "statefulsets.apps",
    "jobs.batch",
    "roles.rbac.authorization.k8s.io",
    "rolebindings.rbac.authorization.k8s.io",
    "networkpolicies.networking.k8s.io",
    "ingresses.networking.k8s.io",
    "gateways.gateway.networking.k8s.io",
    "httproutes.gateway.networking.k8s.io",
    "securitypolicies.gateway.envoyproxy.io",
    "issuers.cert-manager.io",
    "certificates.cert-manager.io",
)
CLUSTER_RESOURCES = (
    "clusterroles.rbac.authorization.k8s.io",
    "clusterrolebindings.rbac.authorization.k8s.io",
    "gatewayclasses.gateway.networking.k8s.io",
    "storageclasses.storage.k8s.io",
)
ENVOY_RESOURCES = ("envoyproxies.gateway.envoyproxy.io",)
KNOWN_TABLES = (
    "access_hosts",
    "access_routes",
    "access_secret_bindings",
    "applications",
    "audit_records",
    "auth_providers",
    "auth_rate_limits",
    "auth_sessions",
    "build_attempts",
    "build_dispatches",
    "build_operations",
    "builds",
    "delivery_pipeline_revisions",
    "delivery_pipelines",
    "delivery_runs",
    "deployment_targets",
    "dispatch_preparations",
    "external_identities",
    "idempotency_records",
    "identity_control",
    "image_artifacts",
    "internal_event_outbox",
    "legacy_project_members",
    "local_credentials",
    "oidc_transactions",
    "operation_attempts",
    "operation_dispatches",
    "project_gateway_sync",
    "project_members",
    "projects",
    "release_operations",
    "releases",
    "schema_migrations",
    "users",
    "webhook_deliveries",
)
ROLE_CONTRACT = [{
    "name": "orbitdevops",
    "can_login": True,
    "superuser": True,
    "create_role": True,
    "create_db": True,
}]


class BackupError(RuntimeError):
    """仅携带可公开的固定错误分类，不夹带外部命令或敏感输出。"""


class SafeArgumentParser(argparse.ArgumentParser):
    """命令参数错误也收敛成固定分类，避免把调用内容写入错误输出。"""

    def error(self, message):
        del message
        raise BackupError("invalid_arguments")


class RealBoundary:
    """生产主机边界：固定路径、固定 kubeconfig，且永不转发子进程 stderr。"""

    backup_root = BACKUP_ROOT

    def euid(self):
        return os.geteuid()

    def now(self):
        return datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")

    def checkpoint_uid(self):
        return str(uuid.uuid4())

    def validate_backup_root(self, path):
        path = Path(path).absolute()
        parent = path.parent
        # 先验证整条已存在路径，再创建叶目录；sticky 目录只允许其保护的 root-owned 子项。
        for candidate in path.parents:
            try:
                info = candidate.lstat()
            except FileNotFoundError:
                continue
            if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != 0 or \
                    (info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX):
                raise BackupError("unsafe_backup_root")
        if not parent.exists():
            parent.mkdir(mode=0o700)
        if not path.exists():
            path.mkdir(mode=0o700)
        for candidate in (parent, path):
            info = candidate.lstat()
            if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != 0:
                raise BackupError("unsafe_backup_root")
        if stat.S_IMODE(path.lstat().st_mode) != 0o700:
            raise BackupError("unsafe_backup_root")

    def run(self, command, *, stdin_path=None, stdout_path=None):
        stdin_handle = None
        stdout_handle = None
        try:
            if stdin_path is not None:
                stdin_handle = open(stdin_path, "rb")
            if stdout_path is not None:
                flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
                flags |= getattr(os, "O_NOFOLLOW", 0)
                descriptor = os.open(stdout_path, flags, 0o600)
                stdout_handle = os.fdopen(descriptor, "wb")
                stdout = stdout_handle
            else:
                stdout = subprocess.PIPE
            result = subprocess.run(
                list(command),
                stdin=stdin_handle,
                stdout=stdout,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=300,
            )
            if result.returncode != 0:
                raise BackupError("external_command_failed")
            if stdout_handle is not None:
                stdout_handle.flush()
                os.fsync(stdout_handle.fileno())
                return b""
            return result.stdout
        except subprocess.TimeoutExpired as error:
            raise BackupError("external_command_timeout") from error
        finally:
            if stdout_handle is not None:
                stdout_handle.close()
            if stdin_handle is not None:
                stdin_handle.close()

    def publish(self, staging, destination):
        # Linux renameat2 的 NOREPLACE 同时满足目录原子可见与永不覆盖。
        if not sys.platform.startswith("linux"):
            raise BackupError("atomic_publish_unavailable")
        libc = ctypes.CDLL(None, use_errno=True)
        result = libc.renameat2(
            -100,
            os.fsencode(staging),
            -100,
            os.fsencode(destination),
            1,
        )
        if result != 0:
            cause = ctypes.get_errno()
            if cause == errno.EEXIST:
                raise BackupError("checkpoint_exists")
            raise BackupError("atomic_publish_failed")


def _arguments(argv):
    parser = SafeArgumentParser(description=__doc__, add_help=True)
    parser.add_argument("--confirm-private-control-plane", action="store_true", required=True)
    parser.add_argument("--cluster-uid", required=True)
    parser.add_argument("--namespace-uid", required=True)
    commands = parser.add_subparsers(dest="operation", required=True)
    create = commands.add_parser("create")
    create.add_argument("checkpoint_id")
    return parser.parse_args(argv)


def _decode_json(payload, category):
    try:
        return json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise BackupError(category) from error


def _kube_json(boundary, arguments, category="invalid_cluster_response"):
    return _decode_json(boundary.run((*KUBE, *arguments)), category)


def _metadata_identity(document):
    metadata = document.get("metadata")
    if not isinstance(metadata, dict):
        raise BackupError("cluster_identity_rejected")
    return metadata


def _guard_private_control_plane(boundary, cluster_uid, namespace_uid):
    if boundary.euid() != 0:
        raise BackupError("root_required")
    boundary.validate_backup_root(Path(boundary.backup_root))
    boundary.run(("systemctl", "is-active", "--quiet", "orbit-private-network.service"))
    boundary.run(("systemctl", "is-active", "--quiet", "k3s.service"))

    nodes = _kube_json(boundary, ("get", "nodes", "-o", "json"))
    items = nodes.get("items")
    if not isinstance(items, list) or len(items) != 1:
        raise BackupError("cluster_identity_rejected")
    node = items[0]
    node_labels = _metadata_identity(node).get("labels", {})
    ready = any(condition.get("type") == "Ready" and condition.get("status") == "True"
                for condition in node.get("status", {}).get("conditions", []))
    if node_labels.get("orbit-devops.dev/environment") != "private-test" or not ready:
        raise BackupError("cluster_identity_rejected")

    namespaces = {}
    for name in ("kube-system", *NAMESPACES):
        namespace = _kube_json(boundary, ("get", "namespace", name, "-o", "json"))
        metadata = _metadata_identity(namespace)
        if metadata.get("name") != name:
            raise BackupError("cluster_identity_rejected")
        if name in NAMESPACES:
            labels = metadata.get("labels", {})
            if any(labels.get(key) != value for key, value in PRIVATE_LABELS.items()):
                raise BackupError("cluster_identity_rejected")
        namespaces[name] = namespace
    if _metadata_identity(namespaces["kube-system"]).get("uid") != cluster_uid:
        raise BackupError("cluster_identity_rejected")
    if _metadata_identity(namespaces["orbit-system"]).get("uid") != namespace_uid:
        raise BackupError("cluster_identity_rejected")

    identity = _kube_json(boundary, (
        "--namespace", "orbit-system", "get", "configmap", "orbit-cluster-identity", "-o", "json"))
    if identity.get("data", {}).get("ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID") != cluster_uid:
        raise BackupError("cluster_identity_rejected")

    postgres = _kube_json(boundary, (
        "--namespace", "orbit-system", "get", "statefulset", "postgres", "-o", "json"))
    try:
        spec = postgres["spec"]
        containers = spec["template"]["spec"]["containers"]
        postgres_container = next(item for item in containers if item.get("name") == "postgres")
        image = postgres_container["image"]
    except (KeyError, StopIteration, TypeError) as error:
        raise BackupError("postgres_identity_rejected") from error
    if spec.get("replicas") != 1 or not re.fullmatch(r"postgres(?:[:][^@]+)?@sha256:[0-9a-f]{64}", image):
        raise BackupError("postgres_identity_rejected")
    return namespaces


def _checkpoint_paths(root, checkpoint_id, checkpoint_uid):
    destination = root / checkpoint_id
    incomplete = list(root.glob(f".{checkpoint_id}.*.incomplete"))
    if destination.exists() or destination.is_symlink() or incomplete:
        raise BackupError("checkpoint_exists")
    staging = root / f".{checkpoint_id}.{checkpoint_uid}.incomplete"
    try:
        staging.mkdir(mode=0o700)
    except FileExistsError as error:
        raise BackupError("checkpoint_exists") from error
    if stat.S_IMODE(staging.lstat().st_mode) != 0o700 or staging.is_symlink():
        raise BackupError("unsafe_checkpoint_directory")
    return staging, destination


def _normalize_runtime(value):
    if isinstance(value, list):
        # PodSpec、RBAC subjects 等列表的先后可能携带语义；递归规范化不得重排。
        return [_normalize_runtime(item) for item in value]
    if not isinstance(value, dict):
        return value
    result = {}
    for key, item in value.items():
        if key == "status":
            continue
        if key == "metadata" and isinstance(item, dict):
            item = {metadata_key: metadata_value for metadata_key, metadata_value in item.items()
                    if metadata_key not in {
                        "creationTimestamp", "deletionGracePeriodSeconds", "deletionTimestamp",
                        "finalizers", "generation", "managedFields", "ownerReferences",
                        "resourceVersion", "selfLink", "uid",
                    }}
        result[key] = _normalize_runtime(item)
    return result


def _orbit_related(item):
    metadata = item.get("metadata", {})
    name = metadata.get("name", "")
    labels = metadata.get("labels", {})
    return (name.startswith("orbit-") or name == "local-path"
            or labels.get("app.kubernetes.io/managed-by") == "orbit-devops")


def _list_runtime_resource(boundary, resource, namespace=None, orbit_only=False):
    arguments = ["get", resource]
    if namespace is not None:
        arguments.extend(("--namespace", namespace))
    arguments.extend(("-o", "json"))
    document = _kube_json(boundary, tuple(arguments), "runtime_snapshot_failed")
    items = document.get("items")
    if not isinstance(items, list):
        raise BackupError("runtime_snapshot_failed")
    if orbit_only:
        items = [item for item in items if _orbit_related(item)]
    normalized = [_normalize_runtime(item) for item in items]
    # 仅资源查询返回的顶层 Kubernetes List 可为生成稳定快照而排序。
    normalized.sort(key=lambda item: (
        item.get("kind", ""),
        item.get("metadata", {}).get("namespace", ""),
        item.get("metadata", {}).get("name", ""),
    ))
    return normalized


def _runtime_snapshot(boundary, guarded_namespaces, captured_at):
    namespaces = {}
    for namespace in NAMESPACES:
        namespaces[namespace] = {
            resource: _list_runtime_resource(boundary, resource, namespace)
            for resource in NAMESPACED_RESOURCES
        }
    # EnvoyProxy 与 GatewayClass 是入口恢复所需的唯一额外域；不导出其他租户对象。
    namespaces["envoy-gateway-system"] = {
        resource: _list_runtime_resource(boundary, resource, "envoy-gateway-system", orbit_only=True)
        for resource in ENVOY_RESOURCES
    }
    cluster = {
        resource: _list_runtime_resource(boundary, resource, orbit_only=True)
        for resource in CLUSTER_RESOURCES
    }
    namespace_identities = {
        name: _normalize_runtime(guarded_namespaces[name]) for name in NAMESPACES
    }
    return {
        "kind": "orbit-control-plane-runtime",
        "version": 1,
        "captured_at": captured_at,
        "namespace_identities": namespace_identities,
        "namespaces": namespaces,
        "cluster_dependencies": cluster,
    }


def _postgres_command(database, *arguments, attach_stdin=False):
    exec_arguments = ("exec", "-i", "postgres-0") if attach_stdin else ("exec", "postgres-0")
    return (*KUBE, "--namespace", "orbit-system", *exec_arguments, "--", *arguments,
            "--username=orbitdevops", f"--dbname={database}", "--no-password")


def _psql(boundary, database, sql):
    command = _postgres_command(database, "psql", "-X", "--set=ON_ERROR_STOP=1", "--tuples-only",
                                "--no-align", "--command", sql)
    try:
        return boundary.run(command).decode("utf-8").strip()
    except UnicodeDecodeError as error:
        raise BackupError("postgres_contract_invalid") from error


def _contract_sql(marker, body):
    return f"/* orbit-backup:{marker} */ {body}"


def _json_contract(boundary, database, marker, body):
    return _decode_json(_psql(boundary, database, _contract_sql(marker, body)).encode("utf-8"),
                        "postgres_contract_invalid")


def _database_exists(boundary, database):
    value = _psql(boundary, "orbitdevops", _contract_sql(
        "database-exists", f"SELECT count(*) FROM pg_database WHERE datname = '{database}';"))
    return value != "0"


def _database_contract(boundary, database):
    schema = _psql(boundary, database, _contract_sql(
        "schema", "SELECT version, dirty FROM schema_migrations;"))
    if schema != "26|f":
        raise BackupError("schema_contract_failed")

    roles = _json_contract(boundary, database, "roles", """
        SELECT COALESCE(json_agg(json_build_object(
            'name', rolname, 'can_login', rolcanlogin, 'superuser', rolsuper,
            'create_role', rolcreaterole, 'create_db', rolcreatedb) ORDER BY rolname), '[]'::json)
        FROM pg_roles WHERE rolname !~ '^pg_';
    """)
    if roles != ROLE_CONTRACT:
        raise BackupError("role_contract_failed")

    owner = _psql(boundary, database, _contract_sql(
        "owner", "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database();"))
    if owner != "orbitdevops":
        raise BackupError("role_contract_failed")

    tables = _json_contract(boundary, database, "tables", """
        SELECT COALESCE(json_agg(tablename ORDER BY tablename), '[]'::json)
        FROM pg_tables WHERE schemaname = 'public';
    """)
    if tables != list(KNOWN_TABLES):
        raise BackupError("known_tables_failed")

    constraints = _json_contract(boundary, database, "constraints", """
        SELECT COALESCE(json_agg(format('%s:%s:%s:%s:%s', relation.relname,
            constraint_record.conname, constraint_record.contype,
            CASE WHEN constraint_record.convalidated THEN 'true' ELSE 'false' END,
            pg_get_constraintdef(constraint_record.oid, true))
            ORDER BY relation.relname, constraint_record.conname), '[]'::json)
        FROM pg_constraint AS constraint_record
        JOIN pg_class AS relation ON relation.oid = constraint_record.conrelid
        JOIN pg_namespace AS namespace_record ON namespace_record.oid = relation.relnamespace
        WHERE namespace_record.nspname = 'public';
    """)
    constraint_tables = {value.split(":", 1)[0] for value in constraints
                         if isinstance(value, str) and ":true" in value}
    if not isinstance(constraints, list) or not set(KNOWN_TABLES).issubset(constraint_tables):
        raise BackupError("constraint_contract_failed")

    count_parts = [
        f"SELECT '{table}' AS table_name, count(*)::bigint AS row_count FROM public.\"{table}\""
        for table in KNOWN_TABLES
    ]
    row_counts = _json_contract(boundary, database, "row-counts", "SELECT json_object_agg(table_name, row_count) FROM ("
                                + " UNION ALL ".join(count_parts) + ") AS counts;")
    if (not isinstance(row_counts, dict) or set(row_counts) != set(KNOWN_TABLES)
            or any(isinstance(value, bool) or not isinstance(value, int) or value < 0
                   for value in row_counts.values())):
        raise BackupError("row_count_contract_failed")
    return {
        "schema": schema,
        "roles": roles,
        "owner": owner,
        "tables": tables,
        "constraints": constraints,
        "row_counts": row_counts,
    }


def _write_exclusive(path, payload):
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    flags |= getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, 0o600)
    try:
        with os.fdopen(descriptor, "wb", closefd=False) as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
    finally:
        os.close(descriptor)


def _write_json(path, document):
    _write_exclusive(path, (json.dumps(document, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
                            + "\n").encode("utf-8"))


def _seal_file(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or path.is_symlink():
        raise BackupError("unsafe_backup_artifact")
    os.chmod(path, 0o600)
    with path.open("rb") as handle:
        os.fsync(handle.fileno())


def _artifact_metadata(path):
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while chunk := handle.read(1024 * 1024):
            digest.update(chunk)
            size += len(chunk)
    if size == 0:
        raise BackupError("empty_backup_artifact")
    return {"sha256": digest.hexdigest(), "size": size}


def _postgres_version(boundary):
    command = (*KUBE, "--namespace", "orbit-system", "exec", "postgres-0", "--",
               "postgres", "--version")
    try:
        version = boundary.run(command).decode("utf-8").strip()
    except UnicodeDecodeError as error:
        raise BackupError("postgres_version_invalid") from error
    if not re.fullmatch(r"postgres \(PostgreSQL\) 17(?:\.[0-9]+)*", version):
        raise BackupError("postgres_version_invalid")
    return version


def _create_database(boundary, database):
    if _database_exists(boundary, database):
        raise BackupError("validation_database_exists")
    sql = _contract_sql(
        "create-database", f"CREATE DATABASE \"{database}\" WITH TEMPLATE template0 OWNER orbitdevops;")
    _psql(boundary, "orbitdevops", sql)


def _drop_database(boundary, database):
    sql = _contract_sql("drop-database", f"DROP DATABASE \"{database}\";")
    _psql(boundary, "orbitdevops", sql)


def execute(argv, boundary):
    """执行一个完整备份检查点；所有主机副作用都经由 boundary。"""
    args = _arguments(argv)
    if not SAFE_ID.fullmatch(args.checkpoint_id):
        raise BackupError("invalid_checkpoint_id")
    if not SAFE_UID.fullmatch(args.cluster_uid) or not SAFE_UID.fullmatch(args.namespace_uid):
        raise BackupError("invalid_identity")

    root = Path(boundary.backup_root)
    guarded_namespaces = _guard_private_control_plane(
        boundary, args.cluster_uid, args.namespace_uid)
    checkpoint_uid = boundary.checkpoint_uid()
    if not SAFE_UID.fullmatch(checkpoint_uid):
        raise BackupError("invalid_checkpoint_uid")
    staging, destination = _checkpoint_paths(root, args.checkpoint_id, checkpoint_uid)
    created_at = boundary.now()
    database = f"orbit_restore_{args.checkpoint_id}"
    if _database_exists(boundary, database):
        raise BackupError("validation_database_exists")

    runtime_path = staging / "runtime.json"
    database_path = staging / "database.dump"
    globals_path = staging / "globals.sql"
    role_contract_path = staging / "role-contract.json"
    receipt_path = staging / "receipt.json"

    runtime = _runtime_snapshot(boundary, guarded_namespaces, created_at)
    _write_json(runtime_path, runtime)
    postgres_version = _postgres_version(boundary)
    before = _database_contract(boundary, "orbitdevops")
    _write_json(role_contract_path, {
        "kind": "orbit-postgres-role-contract",
        "version": 1,
        "database_owner": before["owner"],
        "roles": before["roles"],
        "postgres_version": postgres_version,
        "role_passwords_included": False,
    })

    boundary.run(_postgres_command("orbitdevops", "pg_dump", "--format=custom"),
                 stdout_path=database_path)
    boundary.run((*KUBE, "--namespace", "orbit-system", "exec", "postgres-0", "--",
                  "pg_dumpall", "--globals-only", "--no-role-passwords", "--no-password",
                  "--username=orbitdevops"), stdout_path=globals_path)
    for path in (runtime_path, database_path, globals_path, role_contract_path):
        _seal_file(path)

    after = _database_contract(boundary, "orbitdevops")
    if after != before:
        raise BackupError("source_changed_during_backup")

    # 只有源数据库稳定后才创建验证库；失败时保留本次库和 incomplete 目录供人工检查。
    _create_database(boundary, database)
    boundary.run(_postgres_command(database, "pg_restore", "--exit-on-error", "--single-transaction",
                                   attach_stdin=True),
                 stdin_path=database_path)
    restored = _database_contract(boundary, database)
    if restored != before:
        raise BackupError("restore_verification_failed")
    _drop_database(boundary, database)

    artifacts = {
        path.name: _artifact_metadata(path)
        for path in (database_path, globals_path, role_contract_path, runtime_path)
    }
    verified_at = boundary.now()
    receipt = {
        "kind": "orbit-control-plane-backup",
        "version": 1,
        "checkpoint_id": args.checkpoint_id,
        "checkpoint_uid": checkpoint_uid,
        "created_at": created_at,
        "verified_at": verified_at,
        "source": {
            "cluster_uid": args.cluster_uid,
            "orbit_system_uid": args.namespace_uid,
            "postgres_version": postgres_version,
            "schema_version": 26,
            "schema_dirty": False,
        },
        "artifacts": artifacts,
        "verification": {
            "status": "verified",
            "checks": ["restore", "schema", "known_tables", "constraints", "row_counts"],
            "validation_database_dropped": True,
        },
        "claims": {
            "intact_node_only": True,
            "off_host": False,
            "disaster_recovery": False,
            "role_passwords_included": False,
            "pvc_data_included": False,
            "registry_blobs_included": False,
            "k3s_datastore_included": False,
        },
    }
    _write_json(receipt_path, receipt)
    _seal_file(receipt_path)
    directory_handle = os.open(staging, os.O_RDONLY)
    try:
        os.fsync(directory_handle)
    finally:
        os.close(directory_handle)
    boundary.publish(staging, destination)
    root_handle = os.open(root, os.O_RDONLY)
    try:
        os.fsync(root_handle)
    finally:
        os.close(root_handle)
    return f"CONTROL_PLANE_BACKUP_VERIFIED {args.checkpoint_id}"


def main(argv=None):
    try:
        print(execute(sys.argv[1:] if argv is None else argv, RealBoundary()))
        return 0
    except BackupError as error:
        print(f"CONTROL_PLANE_BACKUP_FAILED {error}", file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, TypeError):
        print("CONTROL_PLANE_BACKUP_FAILED internal_error", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
