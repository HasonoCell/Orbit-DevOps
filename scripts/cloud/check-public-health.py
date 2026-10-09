#!/usr/bin/env python3
"""从平台外部执行无凭据的公网 HTTP/TLS 健康检查。"""

from collections import namedtuple
from datetime import datetime, timedelta, timezone
import ipaddress
import json
import os
import re
import socket
import ssl
import sys
import time
from urllib.error import HTTPError, URLError
from urllib.request import build_opener, HTTPRedirectHandler, Request


Response = namedtuple("Response", "status content_type body")
ORIGIN = re.compile(r"https://[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.vercel\.app")
MAX_BODY = 1024 * 1024


class NetworkFailure(Exception):
    """可重试的瞬时外部网络失败，不携带响应或端点内容。"""


class HealthFailure(Exception):
    """仅保存可公开的固定检查分类。"""

    def __init__(self, check):
        super().__init__(check)
        self.check = check


class ProtocolFailure(Exception):
    """不可重试的 TLS 或响应协议失败。"""


class _RejectRedirects(HTTPRedirectHandler):
    def redirect_request(self, request, file_pointer, code, message, headers, new_url):
        del request, file_pointer, code, message, headers, new_url
        return None


class PublicBoundary:
    """标准库公网边界：无 Cookie 容器、无重定向，且响应体有界。"""

    def __init__(self, origin, timeout=10):
        self.origin = origin
        self.timeout = timeout
        self.opener = build_opener(_RejectRedirects())

    def request(self, path):
        request = Request(self.origin + path, method="GET", headers={
            "Accept": "text/html, application/json",
            "User-Agent": "orbit-public-health/1",
        })
        try:
            try:
                response = self.opener.open(request, timeout=self.timeout)
            except HTTPError as error:
                response = error
            with response:
                body = response.read(MAX_BODY + 1)
                if len(body) > MAX_BODY:
                    raise ProtocolFailure()
                content_type = response.headers.get("Content-Type", "").split(";", 1)[0].strip().lower()
                return Response(response.getcode(), content_type, body)
        except ProtocolFailure:
            raise
        except URLError as error:
            if isinstance(error.reason, (ssl.SSLCertVerificationError, ssl.CertificateError, ssl.SSLError)):
                raise ProtocolFailure() from error
            raise NetworkFailure() from error
        except (TimeoutError, OSError) as error:
            raise NetworkFailure() from error

    def certificate_expiry(self, address, hostname):
        try:
            with socket.create_connection((address, 443), timeout=self.timeout) as connection:
                context = ssl.create_default_context()
                with context.wrap_socket(connection, server_hostname=hostname) as secured:
                    not_after = secured.getpeercert().get("notAfter")
            if not isinstance(not_after, str):
                raise ProtocolFailure()
            return datetime.fromtimestamp(ssl.cert_time_to_seconds(not_after), timezone.utc)
        except ProtocolFailure:
            raise
        except (ssl.SSLCertVerificationError, ssl.CertificateError, ssl.SSLError, ValueError) as error:
            raise ProtocolFailure() from error
        except (TimeoutError, OSError) as error:
            raise NetworkFailure() from error


def _retry(operation, check, sleeper):
    """只有瞬时网络错误可以重试；TLS 与响应协议错误立即归为固定检查失败。"""
    for attempt in range(3):
        try:
            return operation()
        except NetworkFailure:
            if attempt == 2:
                raise HealthFailure(check) from None
            sleeper(attempt + 1)
        except ProtocolFailure:
            raise HealthFailure(check) from None
    raise AssertionError("unreachable")


def _write(stream, value):
    print(value, file=stream)


def _execute(environ, boundary_factory, stdout, sleeper, now):
    origin = environ.get("ORBIT_PUBLIC_URL", "")
    if not ORIGIN.fullmatch(origin):
        raise HealthFailure("configuration")
    boundary = boundary_factory(origin)

    login = _retry(lambda: boundary.request("/login"), "login_html", sleeper)
    if login.status != 200 or login.content_type != "text/html" or b"<html" not in login.body.lower():
        raise HealthFailure("login_html")
    _write(stdout, "PUBLIC_HEALTH_OK check=login_html")

    providers = _retry(lambda: boundary.request("/api/v1/auth/providers"), "providers_database", sleeper)
    try:
        provider_body = json.loads(providers.body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise HealthFailure("providers_database") from None
    if providers.status != 200 or providers.content_type != "application/json" or not isinstance(provider_body, list):
        raise HealthFailure("providers_database")
    _write(stdout, "PUBLIC_HEALTH_OK check=providers_database")

    principal = _retry(lambda: boundary.request("/api/v1/users/me"), "anonymous_auth", sleeper)
    try:
        body = json.loads(principal.body)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise HealthFailure("anonymous_auth") from None
    if principal.status != 401 or principal.content_type != "application/json" or not isinstance(body, dict) or \
            body.get("code") != "authentication_required":
        raise HealthFailure("anonymous_auth")
    _write(stdout, "PUBLIC_HEALTH_OK check=anonymous_auth")

    address = environ.get("ORBIT_ORIGIN_IP", "")
    if address:
        try:
            parsed_address = ipaddress.ip_address(address)
        except ValueError:
            raise HealthFailure("configuration") from None
        if parsed_address.version != 4 or not parsed_address.is_global or str(parsed_address) != address:
            raise HealthFailure("configuration")
        expiry = _retry(lambda: boundary.certificate_expiry(address, address), "origin_certificate", sleeper)
        current = now()
        if not isinstance(expiry, datetime) or expiry.tzinfo is None or current.tzinfo is None or \
                expiry - current <= timedelta(hours=48):
            raise HealthFailure("origin_certificate")
        _write(stdout, "PUBLIC_HEALTH_OK check=origin_certificate")
    else:
        _write(stdout, "PUBLIC_HEALTH_SKIPPED check=origin_certificate")
    _write(stdout, "PUBLIC_HEALTH_READY")


def main(*, environ=None, boundary_factory=None, stdout=None, stderr=None, sleeper=None, now=None):
    """执行公开命令契约；输出只包含固定检查分类。"""
    environ = os.environ if environ is None else environ
    stdout = sys.stdout if stdout is None else stdout
    stderr = sys.stderr if stderr is None else stderr
    sleeper = time.sleep if sleeper is None else sleeper
    now = (lambda: datetime.now(timezone.utc)) if now is None else now
    boundary_factory = PublicBoundary if boundary_factory is None else boundary_factory
    try:
        _execute(environ, boundary_factory, stdout, sleeper, now)
    except HealthFailure as failure:
        _write(stderr, "PUBLIC_HEALTH_FAILED check=" + failure.check)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
