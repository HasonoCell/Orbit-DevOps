"""匿名公网健康命令的边界测试；不访问真实公网或生产环境。"""

from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
from pathlib import Path
import threading
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("check-public-health.py")
spec = importlib.util.spec_from_file_location("public_health", SCRIPT)
public_health = importlib.util.module_from_spec(spec)
spec.loader.exec_module(public_health)


class FakeBoundary:
    """只替代公网 HTTP/TLS 系统边界，返回测试给定的公开响应。"""

    def __init__(self, responses, certificate=None):
        self.responses = {path: list(values) for path, values in responses.items()}
        self.certificate = certificate
        self.requests = []
        self.certificate_requests = []

    def request(self, path):
        self.requests.append(path)
        value = self.responses[path].pop(0)
        if isinstance(value, Exception):
            raise value
        return value

    def certificate_expiry(self, address, hostname):
        self.certificate_requests.append((address, hostname))
        if isinstance(self.certificate, Exception):
            raise self.certificate
        return self.certificate


def response(status, content_type, body):
    if not isinstance(body, bytes):
        body = json.dumps(body).encode()
    return public_health.Response(status, content_type, body)


class PublicHealthCommandTest(unittest.TestCase):
    def test_anonymous_public_contract_succeeds_without_origin_address(self):
        boundary = FakeBoundary({
            "/login": [response(200, "text/html", b"<!doctype html><html></html>")],
            "/api/v1/auth/providers": [response(200, "application/json", [])],
            "/api/v1/users/me": [response(401, "application/json", {"code": "authentication_required"})],
        })
        stdout, stderr = io.StringIO(), io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app"},
            boundary_factory=lambda _origin: boundary,
            stdout=stdout,
            stderr=stderr,
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 0)
        self.assertEqual(boundary.requests, ["/login", "/api/v1/auth/providers", "/api/v1/users/me"])
        self.assertEqual(boundary.certificate_requests, [])
        self.assertEqual(stdout.getvalue().splitlines(), [
            "PUBLIC_HEALTH_OK check=login_html",
            "PUBLIC_HEALTH_OK check=providers_database",
            "PUBLIC_HEALTH_OK check=anonymous_auth",
            "PUBLIC_HEALTH_SKIPPED check=origin_certificate",
            "PUBLIC_HEALTH_READY",
        ])
        self.assertEqual(stderr.getvalue(), "")
        self.assertNotIn("orbit-example", stdout.getvalue())

    def test_two_transient_network_errors_are_retried(self):
        boundary = FakeBoundary({
            "/login": [
                public_health.NetworkFailure(),
                public_health.NetworkFailure(),
                response(200, "text/html", b"<html></html>"),
            ],
            "/api/v1/auth/providers": [response(200, "application/json", [])],
            "/api/v1/users/me": [response(401, "application/json", {"code": "authentication_required"})],
        })

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app"},
            boundary_factory=lambda _origin: boundary,
            stdout=io.StringIO(),
            stderr=io.StringIO(),
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 0)
        self.assertEqual(boundary.requests.count("/login"), 3)

    def test_schema_failure_is_immediate_and_output_is_redacted(self):
        boundary = FakeBoundary({
            "/login": [response(200, "text/html", b"<html></html>")],
            "/api/v1/auth/providers": [
                response(200, "application/json", {"unexpected": "do-not-print-this-body"}),
                response(200, "application/json", []),
            ],
        })
        stdout, stderr = io.StringIO(), io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app"},
            boundary_factory=lambda _origin: boundary,
            stdout=stdout,
            stderr=stderr,
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 1)
        self.assertEqual(boundary.requests.count("/api/v1/auth/providers"), 1)
        self.assertEqual(stderr.getvalue(), "PUBLIC_HEALTH_FAILED check=providers_database\n")
        combined = stdout.getvalue() + stderr.getvalue()
        self.assertNotIn("orbit-example", combined)
        self.assertNotIn("do-not-print", combined)

    def test_unexpected_anonymous_status_fails_without_retry(self):
        boundary = FakeBoundary({
            "/login": [response(200, "text/html", b"<html></html>")],
            "/api/v1/auth/providers": [response(200, "application/json", [])],
            "/api/v1/users/me": [
                response(200, "application/json", {"code": "unexpected"}),
                response(401, "application/json", {"code": "authentication_required"}),
            ],
        })
        stderr = io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app"},
            boundary_factory=lambda _origin: boundary,
            stdout=io.StringIO(),
            stderr=stderr,
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 1)
        self.assertEqual(boundary.requests.count("/api/v1/users/me"), 1)
        self.assertEqual(stderr.getvalue(), "PUBLIC_HEALTH_FAILED check=anonymous_auth\n")

    def test_anonymous_error_body_must_be_a_json_object(self):
        boundary = FakeBoundary({
            "/login": [response(200, "text/html", b"<html></html>")],
            "/api/v1/auth/providers": [response(200, "application/json", [])],
            "/api/v1/users/me": [response(401, "application/json", [])],
        })
        stderr = io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app"},
            boundary_factory=lambda _origin: boundary,
            stdout=io.StringIO(),
            stderr=stderr,
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 1)
        self.assertEqual(stderr.getvalue(), "PUBLIC_HEALTH_FAILED check=anonymous_auth\n")

    def test_direct_origin_certificate_verifies_ip_san_and_accepts_more_than_48_hours(self):
        boundary = FakeBoundary({
            "/login": [response(200, "text/html", b"<html></html>")],
            "/api/v1/auth/providers": [response(200, "application/json", [])],
            "/api/v1/users/me": [response(401, "application/json", {"code": "authentication_required"})],
        }, certificate=datetime(2026, 10, 12, tzinfo=timezone.utc))
        stdout = io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app", "ORBIT_ORIGIN_IP": "8.8.8.8"},
            boundary_factory=lambda _origin: boundary,
            stdout=stdout,
            stderr=io.StringIO(),
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 0)
        self.assertEqual(boundary.certificate_requests, [("8.8.8.8", "8.8.8.8")])
        self.assertIn("PUBLIC_HEALTH_OK check=origin_certificate", stdout.getvalue().splitlines())
        self.assertNotIn("PUBLIC_HEALTH_SKIPPED check=origin_certificate", stdout.getvalue().splitlines())

    def test_certificate_at_48_hours_is_an_error(self):
        now = datetime(2026, 10, 9, tzinfo=timezone.utc)
        boundary = FakeBoundary({
            "/login": [response(200, "text/html", b"<html></html>")],
            "/api/v1/auth/providers": [response(200, "application/json", [])],
            "/api/v1/users/me": [response(401, "application/json", {"code": "authentication_required"})],
        }, certificate=datetime(2026, 10, 11, tzinfo=timezone.utc))
        stderr = io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app", "ORBIT_ORIGIN_IP": "8.8.8.8"},
            boundary_factory=lambda _origin: boundary,
            stdout=io.StringIO(),
            stderr=stderr,
            sleeper=lambda _seconds: None,
            now=lambda: now,
        )

        self.assertEqual(code, 1)
        self.assertEqual(stderr.getvalue(), "PUBLIC_HEALTH_FAILED check=origin_certificate\n")

    def test_network_failure_stops_after_two_retries_with_fixed_output(self):
        boundary = FakeBoundary({
            "/login": [public_health.NetworkFailure(), public_health.NetworkFailure(), public_health.NetworkFailure()],
        })
        stderr = io.StringIO()

        code = public_health.main(
            environ={"ORBIT_PUBLIC_URL": "https://orbit-example.vercel.app"},
            boundary_factory=lambda _origin: boundary,
            stdout=io.StringIO(),
            stderr=stderr,
            sleeper=lambda _seconds: None,
            now=lambda: datetime(2026, 10, 9, tzinfo=timezone.utc),
        )

        self.assertEqual(code, 1)
        self.assertEqual(boundary.requests, ["/login", "/login", "/login"])
        self.assertEqual(stderr.getvalue(), "PUBLIC_HEALTH_FAILED check=login_html\n")

    def test_standard_library_transport_does_not_follow_redirect_or_send_cookie(self):
        class Handler(BaseHTTPRequestHandler):
            target_requests = 0
            cookies = []

            def do_GET(self):
                type(self).cookies.append(self.headers.get("Cookie"))
                if self.path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", "/target")
                    self.send_header("Set-Cookie", "session=must-not-be-retained")
                    self.end_headers()
                    return
                type(self).target_requests += 1
                self.send_response(200)
                self.send_header("Content-Type", "text/html")
                self.end_headers()
                self.wfile.write(b"<html></html>")

            def log_message(self, _format, *_args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            boundary = public_health.PublicBoundary("http://127.0.0.1:" + str(server.server_port))
            result = boundary.request("/redirect")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

        self.assertEqual(result.status, 302)
        self.assertEqual(Handler.target_requests, 0)
        self.assertEqual(Handler.cookies, [None])

    def test_tls_transport_uses_default_verification_with_ip_server_name(self):
        observed = {}

        class Resource:
            def __init__(self, value):
                self.value = value

            def __enter__(self):
                return self.value

            def __exit__(self, _type, _value, _traceback):
                return False

        class TLSConnection:
            def getpeercert(self):
                return {"notAfter": "Oct 12 00:00:00 2026 GMT"}

        class Context:
            def wrap_socket(self, raw, *, server_hostname):
                observed["raw"] = raw
                observed["server_hostname"] = server_hostname
                return Resource(TLSConnection())

        raw = object()
        with patch.object(public_health.socket, "create_connection", return_value=Resource(raw)) as connect, \
                patch.object(public_health.ssl, "create_default_context", return_value=Context()) as default_context:
            expiry = public_health.PublicBoundary("https://orbit-example.vercel.app").certificate_expiry(
                "8.8.8.8", "8.8.8.8")

        connect.assert_called_once_with(("8.8.8.8", 443), timeout=10)
        default_context.assert_called_once_with()
        self.assertIs(observed["raw"], raw)
        self.assertEqual(observed["server_hostname"], "8.8.8.8")
        self.assertEqual(expiry, datetime(2026, 10, 12, tzinfo=timezone.utc))

    def test_frontend_certificate_verification_error_is_not_retried_as_network(self):
        class Opener:
            def open(self, _request, *, timeout):
                self.timeout = timeout
                raise public_health.URLError(public_health.ssl.SSLCertVerificationError(1, "fixture"))

        boundary = public_health.PublicBoundary("https://orbit-example.vercel.app")
        boundary.opener = Opener()

        with self.assertRaises(public_health.ProtocolFailure):
            boundary.request("/login")

    def test_production_workflow_has_minimal_main_only_schedule_contract(self):
        workflow = SCRIPT.parents[2] / ".github" / "workflows" / "production-health.yaml"
        content = workflow.read_text()

        for expected in [
            'cron: "17,47 * * * *"',
            "workflow_dispatch:",
            "notification_test:",
            "PUBLIC_HEALTH_NOTIFICATION_TEST",
            "contents: read",
            "if: github.ref == 'refs/heads/main'",
            "timeout-minutes: 5",
            "actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
            "ORBIT_PUBLIC_URL: ${{ vars.ORBIT_PUBLIC_URL }}",
            "ORBIT_ORIGIN_IP: ${{ vars.ORBIT_ORIGIN_IP }}",
            "python3 scripts/cloud/check-public-health.py",
        ]:
            self.assertIn(expected, content)
        # 公网匿名检查继续不接收凭据；专用 SSH job 的独立密钥由其自身边界验证。
        public_job = content.split("  host-health:\n", 1)[0]
        self.assertNotIn("secrets.", public_job)
        self.assertNotIn("setup-python", content)


if __name__ == "__main__":
    unittest.main()
