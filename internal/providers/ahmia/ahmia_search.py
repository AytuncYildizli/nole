#!/usr/bin/env python3
"""
Render Ahmia search results with Playwright.

stdin:
    {"query": "...", "limit": 10, "proxy": "socks5://127.0.0.1:9050"}

stdout, success:
    {"results": [{"title": "...", "url": "http://....onion/", "snippet": "..."}]}

stdout, failure:
    {"error": "timeout|render_failed|tor_unreachable|helper_error", "detail": "..."}
"""

from __future__ import annotations

import html
import json
import os
import re
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import time
from pathlib import Path
from typing import NamedTuple
from urllib.parse import parse_qsl, unquote, urljoin, urlsplit, urlunsplit

AHMIA_ONION = (
    "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion"
)
AHMIA_CLEARNET = "https://ahmia.fi"
AHMIA_ONION_HOST = urlsplit(AHMIA_ONION).hostname
DEFAULT_PROXY = "socks5://127.0.0.1:9050"

# The Go caller kills the helper at 90 seconds. Keep a two-second margin.
MAX_RUNTIME_SECONDS = 88.0
CLEARNET_RESERVE_SECONDS = 28.0
DEADLINE_ENV = "_NOLE_AHMIA_DEADLINE"
REEXEC_ENV = "_NOLE_AHMIA_REEXEC"

FORM_SELECTOR = (
    "form#searchForm:has(input[name='q']), "
    "form[action*='/search']:has(input[name='q'])"
)
RESULT_SELECTOR = "ol.searchResults li.result"


class OverallTimeout(Exception):
    pass


class HelperError(Exception):
    def __init__(self, code: str, detail: str) -> None:
        super().__init__(detail)
        self.code = code
        self.detail = detail


class AttemptError(Exception):
    def __init__(self, kind: str) -> None:
        super().__init__(kind)
        self.kind = kind


class ProxyConfig(NamedTuple):
    server: str
    host: str
    port: int


def _load_deadline() -> float:
    now = time.monotonic()
    inherited = os.environ.get(DEADLINE_ENV)
    if inherited:
        try:
            deadline = float(inherited)
        except ValueError:
            deadline = now + MAX_RUNTIME_SECONDS
        else:
            deadline = min(deadline, now + MAX_RUNTIME_SECONDS)
    else:
        deadline = now + MAX_RUNTIME_SECONDS
    os.environ[DEADLINE_ENV] = repr(deadline)
    return deadline


def _alarm_handler(_signum, _frame) -> None:
    raise OverallTimeout()


def _arm_alarm(deadline: float) -> None:
    if not hasattr(signal, "setitimer"):
        return
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise OverallTimeout()
    signal.signal(signal.SIGALRM, _alarm_handler)
    signal.setitimer(signal.ITIMER_REAL, remaining)


def _disarm_alarm() -> None:
    if hasattr(signal, "setitimer"):
        signal.setitimer(signal.ITIMER_REAL, 0)


def _remaining(deadline: float) -> float:
    value = deadline - time.monotonic()
    if value <= 0.05:
        raise OverallTimeout()
    return value


def _timeout_ms(deadline: float, cap_seconds: float | None = None) -> int:
    value = _remaining(deadline)
    if cap_seconds is not None:
        value = min(value, cap_seconds)
    return max(1, int(value * 1000))


def _python_candidates() -> list[str]:
    candidates: list[str] = []
    def add(value: str | os.PathLike[str] | None) -> None:
        if not value:
            return
        path = str(Path(value).expanduser())
        if path == sys.executable or path in candidates:
            return
        if os.path.isfile(path) and os.access(path, os.X_OK):
            candidates.append(path)
    add(os.environ.get("NOLE_AHMIA_PYTHON"))
    add(os.environ.get("NOLE_SCRAPLING_PYTHON"))
    add(Path.home() / ".local/share/nole/scrapling-venv/bin/python")
    add(Path.home() / ".local/share/nole/venv/bin/python")
    add("/Library/Frameworks/Python.framework/Versions/3.13/bin/python3")
    playwright_cli = shutil.which("playwright")
    if playwright_cli:
        try:
            first_line = Path(playwright_cli).read_text(
                encoding="utf-8", errors="ignore",
            ).splitlines()[0]
            if first_line.startswith("#!"):
                words = shlex.split(first_line[2:].strip())
                if words and os.path.basename(words[0]) != "env":
                    add(words[0])
        except (OSError, IndexError, ValueError):
            pass
    versions = Path("/Library/Frameworks/Python.framework/Versions")
    if versions.is_dir():
        for path in sorted(versions.glob("*/bin/python3"), reverse=True):
            add(path)
    return candidates


def _ensure_playwright_runtime(deadline: float) -> None:
    try:
        import playwright.sync_api
        return
    except OverallTimeout:
        raise
    except Exception:
        pass
    if os.environ.get(REEXEC_ENV) == "1":
        raise HelperError("render_failed", "Playwright is not importable")
    for candidate in _python_candidates():
        probe_timeout = min(1.5, _remaining(deadline))
        try:
            probe = subprocess.run(
                [candidate, "-c", "import playwright.sync_api"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=probe_timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            continue
        if probe.returncode != 0:
            continue
        environment = os.environ.copy()
        environment[REEXEC_ENV] = "1"
        environment[DEADLINE_ENV] = repr(deadline)
        argv = [candidate, str(Path(__file__).resolve()), *sys.argv[1:]]
        try:
            _disarm_alarm()
            os.execve(candidate, argv, environment)
        except OSError:
            _arm_alarm(deadline)
    raise HelperError("render_failed", "Playwright is not installed")


def _parse_request() -> tuple[str, int, str]:
    raw = sys.stdin.read()
    try:
        request = json.loads(raw)
    except json.JSONDecodeError as exc:
        raise HelperError("helper_error", "stdin must contain valid JSON") from exc
    if not isinstance(request, dict):
        raise HelperError("helper_error", "stdin JSON must be an object")
    query = request.get("query")
    if not isinstance(query, str) or not query.strip():
        raise HelperError("helper_error", "query is required")
    query = query.strip()
    limit = request.get("limit", 10)
    if isinstance(limit, bool) or not isinstance(limit, int):
        raise HelperError("helper_error", "limit must be an integer")
    limit = max(1, min(limit, 20))
    proxy = request.get("proxy", os.environ.get("NOLE_PROXY_URL", DEFAULT_PROXY))
    if not isinstance(proxy, str) or not proxy.strip():
        raise HelperError("helper_error", "proxy must be a SOCKS5 URL")
    return query, limit, proxy.strip()


def _parse_proxy(value: str) -> ProxyConfig:
    try:
        parsed = urlsplit(value)
        port = parsed.port or 9050
    except ValueError as exc:
        raise HelperError("helper_error", "invalid SOCKS5 proxy URL") from exc
    if parsed.scheme.lower() not in {"socks5", "socks5h"}:
        raise HelperError("helper_error", "proxy scheme must be socks5 or socks5h")
    if not parsed.hostname:
        raise HelperError("helper_error", "SOCKS5 proxy host is required")
    if parsed.username is not None or parsed.password is not None:
        raise HelperError("helper_error", "authenticated SOCKS5 not supported")
    if parsed.query or parsed.fragment or parsed.path not in {"", "/"}:
        raise HelperError("helper_error", "invalid SOCKS5 proxy URL")
    if not 1 <= port <= 65535:
        raise HelperError("helper_error", "invalid port")
    host = parsed.hostname
    display_host = f"[{host}]" if ":" in host else host
    return ProxyConfig(f"socks5://{display_host}:{port}", host, port)


def _recv_exact(sock: socket.socket, size: int) -> bytes:
    chunks: list[bytes] = []
    remaining = size
    while remaining:
        chunk = sock.recv(remaining)
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def _socks5_ready(proxy: ProxyConfig, deadline: float) -> bool:
    timeout = min(2.0, _remaining(deadline))
    try:
        with socket.create_connection((proxy.host, proxy.port), timeout=timeout) as sock:
            sock.settimeout(timeout)
            sock.sendall(b"\x05\x01\x00")
            return _recv_exact(sock, 2) == b"\x05\x00"
    except OSError:
        return False


def _clean_text(value: object, maximum: int) -> str:
    text = html.unescape(value if isinstance(value, str) else "")
    return re.sub(r"\s+", " ", text).strip()[:maximum]


def _onion_target(href: object, page_url: str) -> str | None:
    if not isinstance(href, str) or not href.strip():
        return None
    absolute = urljoin(page_url, html.unescape(href.strip()))
    try:
        wrapper = urlsplit(absolute)
        pairs = parse_qsl(wrapper.query, keep_blank_values=True)
    except ValueError:
        return None
    target = next(
        (value for key, value in pairs if key.lower() == "redirect_url"),
        absolute,
    )
    target = html.unescape(target).strip().strip("'\"")
    for _ in range(2):
        lowered = target.lower()
        if not lowered.startswith(("http%3a", "https%3a", "%68%74%74%70")):
            break
        decoded = unquote(target)
        if decoded == target:
            break
        target = decoded
    if target.startswith("//"):
        target = "http:" + target
    elif "://" not in target and ".onion" in target.lower():
        target = "http://" + target.lstrip("/")
    if any(ord(character) < 32 for character in target):
        return None
    try:
        parsed = urlsplit(target)
        host = (parsed.hostname or "").rstrip(".").lower()
    except ValueError:
        return None
    if parsed.scheme.lower() not in {"http", "https"}:
        return None
    if not host.endswith(".onion") or host == AHMIA_ONION_HOST:
        return None
    netloc = f"[{host}]" if ":" in host else host
    if parsed.port is not None:
        netloc = f"{netloc}:{parsed.port}"
    return urlunsplit((parsed.scheme.lower(), netloc, parsed.path or "/", parsed.query, parsed.fragment))


def _extract_results(page, limit: int) -> list[dict[str, str]]:
    rows = page.eval_on_selector_all(
        RESULT_SELECTOR,
        """nodes => nodes.map(node => {
            const link = node.querySelector("h4 a") || node.querySelector("a[href*='redirect_url=']");
            const snippet = node.querySelector("p");
            return {
                title: link ? (link.textContent || "") : "",
                href: link ? (link.getAttribute("href") || "") : "",
                snippet: snippet ? (snippet.textContent || "") : ""
            };
        })""",
    )
    results: list[dict[str, str]] = []
    seen: set[str] = set()
    for row in rows if isinstance(rows, list) else []:
        if not isinstance(row, dict):
            continue
        url = _onion_target(row.get("href"), page.url)
        if not url or url in seen:
            continue
        seen.add(url)
        title = _clean_text(row.get("title"), 500)
        if not title:
            title = urlsplit(url).hostname or url
        results.append({"title": title, "url": url, "snippet": _clean_text(row.get("snippet"), 2000)})
        if len(results) >= limit:
            break
    return results


def _is_no_results_page(page, deadline: float) -> bool:
    try:
        body = page.locator("body").inner_text(timeout=_timeout_ms(deadline, 3.0)).lower()
    except OverallTimeout:
        raise
    except Exception:
        return False
    return any(marker in body for marker in ("couldn't find results", "could not find results", "no search results", "no results found"))


def _is_proxy_failure(exc: BaseException) -> bool:
    text = str(exc).lower()
    return any(marker in text for marker in (
        "err_no_supported_proxies", "err_proxy_connection_failed",
        "err_socks_connection_failed", "proxy connection", "proxy server",
        "ns_error_proxy",
    ))


def _render_attempt(playwright, engine_name: str, base_url: str, query: str, limit: int, deadline: float, proxy: ProxyConfig | None) -> list[dict[str, str]]:
    from playwright.sync_api import TimeoutError as PlaywrightTimeout
    browser = None
    try:
        browser_type = getattr(playwright, engine_name)
        launch_options: dict[str, object] = {
            "headless": True,
            "timeout": _timeout_ms(deadline, 15.0),
        }
        if proxy is not None:
            launch_options["proxy"] = {"server": proxy.server}
        if engine_name == "firefox" and proxy is not None:
            launch_options["firefox_user_prefs"] = {
                "javascript.enabled": True,
                "network.dns.blockDotOnion": False,
                "network.proxy.socks_remote_dns": True,
                "network.proxy.socks5_remote_dns": True,
                "network.trr.mode": 5,
            }
        browser = browser_type.launch(**launch_options)
        context = browser.new_context(viewport={"width": 1280, "height": 800}, locale="en-US")
        page = context.new_page()
        page.goto(base_url + "/", wait_until="domcontentloaded", timeout=_timeout_ms(deadline))
        form = page.locator(FORM_SELECTOR).first
        form.wait_for(state="attached", timeout=_timeout_ms(deadline))
        query_input = form.locator("input[name='q'], input#id_q").first
        query_input.wait_for(state="visible", timeout=_timeout_ms(deadline))
        query_input.fill(query, timeout=_timeout_ms(deadline))
        try:
            form.evaluate(
                """form => {
                    if (typeof form.requestSubmit === "function") { form.requestSubmit(); }
                    else { form.submit(); }
                }""",
                timeout=_timeout_ms(deadline),
            )
        except Exception as exc:
            message = str(exc).lower()
            if "execution context was destroyed" not in message and "frame was detached" not in message:
                raise
        try:
            page.wait_for_url(re.compile(r"/search/(?:\?|$)"), wait_until="domcontentloaded", timeout=_timeout_ms(deadline, 15.0))
        except PlaywrightTimeout:
            pass
        handle = page.wait_for_function(
            """() => {
                if (document.querySelector("ol.searchResults li.result")) return true;
                const text = (document.body?.innerText || "").toLowerCase();
                return text.includes("couldn't find results") || text.includes("could not find results") || text.includes("no search results") || text.includes("no results found");
            }""",
            timeout=_timeout_ms(deadline),
        )
        handle.dispose()
        results = _extract_results(page, limit)
        if results or _is_no_results_page(page, deadline):
            return results
        raise AttemptError("render")
    except OverallTimeout:
        raise
    except AttemptError:
        raise
    except PlaywrightTimeout as exc:
        raise AttemptError("timeout") from exc
    except Exception as exc:
        kind = "proxy" if proxy is not None and _is_proxy_failure(exc) else "render"
        raise AttemptError(kind) from exc
    finally:
        if browser is not None:
            try:
                browser.close()
            except OverallTimeout:
                raise
            except Exception:
                pass


def _search(query: str, limit: int, proxy: ProxyConfig, deadline: float) -> list[dict[str, str]]:
    from playwright.sync_api import sync_playwright
    proxy_ready = _socks5_ready(proxy, deadline)
    tor_transport_failed = not proxy_ready
    onion_failures: list[str] = []
    with sync_playwright() as playwright:
        if proxy_ready:
            remaining = _remaining(deadline)
            onion_budget = max(8.0, remaining - CLEARNET_RESERVE_SECONDS)
            onion_deadline = min(deadline, time.monotonic() + onion_budget)
            for engine_name in ("chromium", "firefox"):
                if onion_deadline - time.monotonic() <= 0.5:
                    break
                try:
                    return _render_attempt(playwright, engine_name, AHMIA_ONION, query, limit, onion_deadline, proxy)
                except AttemptError as exc:
                    onion_failures.append(exc.kind)
                    if exc.kind == "proxy":
                        tor_transport_failed = True
                    if exc.kind == "timeout":
                        break
        clearnet_failures: list[str] = []
        for engine_name in ("chromium", "firefox"):
            if deadline - time.monotonic() <= 0.5:
                raise OverallTimeout()
            try:
                return _render_attempt(playwright, engine_name, AHMIA_CLEARNET, query, limit, deadline, proxy)
            except AttemptError as exc:
                clearnet_failures.append(exc.kind)
                if exc.kind == "timeout":
                    break
    if deadline - time.monotonic() <= 0.05:
        raise OverallTimeout()
    if tor_transport_failed:
        raise HelperError("tor_unreachable", "Tor SOCKS5 transport failed and the clearnet fallback also failed")
    if "timeout" in onion_failures or "timeout" in clearnet_failures:
        raise HelperError("timeout", "Ahmia rendering timed out")
    raise HelperError("render_failed", "Ahmia did not render a valid result page")


def _write_response(response: dict[str, object]) -> None:
    json.dump(response, sys.stdout, ensure_ascii=False, separators=(",", ":"))
    sys.stdout.write("\n")
    sys.stdout.flush()


def main() -> None:
    deadline = _load_deadline()
    response: dict[str, object]
    try:
        _arm_alarm(deadline)
        _ensure_playwright_runtime(deadline)
        query, limit, proxy_value = _parse_request()
        proxy = _parse_proxy(proxy_value)
        response = {"results": _search(query, limit, proxy, deadline)}
    except OverallTimeout:
        response = {"error": "timeout", "detail": "Ahmia rendering exceeded the 88-second helper deadline"}
    except HelperError as exc:
        response = {"error": exc.code, "detail": exc.detail}
    except Exception:
        response = {"error": "render_failed", "detail": "unexpected Ahmia renderer failure"}
    finally:
        _disarm_alarm()
    _write_response(response)


if __name__ == "__main__":
    main()
