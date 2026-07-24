#!/usr/bin/env python3
"""
Ahmia .onion search via Playwright Firefox with socks_remote_dns over Tor.

Reads from stdin:   {"query": "...", "limit": 10, "proxy": "socks5://127.0.0.1:9050"}
Writes to stdout:   {"results": [{"title": "...", "url": "...", "snippet": "..."}], "error": null}

On failure:         {"error": "timeout|render_failed|tor_unreachable|helper_error", "detail": "..."}
"""

from __future__ import annotations

import json
import os
import re
import sys
import socket
from urllib.parse import quote_plus, urlparse

AHMIA_ONION = "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion"
AHMIA_CLEARNET = "https://ahmia.fi"
TIMEOUT_MS = int(os.environ.get("TIMEOUT_MS", "90000"))


def main() -> None:
    raw = sys.stdin.read() if not sys.stdin.isatty() else "{}"
    try:
        req = json.loads(raw)
    except json.JSONDecodeError:
        req = {}

    query = req.get("query", "")
    limit = req.get("limit", 10)
    proxy = req.get("proxy", os.environ.get("NOLE_PROXY_URL", "socks5://127.0.0.1:9050"))

    if not query:
        write_result({"error": "helper_error", "detail": "query required"})
        return

    proxy_v = proxy.replace("socks5://", "socks5h://", 1) if proxy.startswith("socks5://") else proxy
    parsed = urlparse(proxy_v.replace("socks5h://", "socks5://"))
    proxy_host = parsed.hostname or "127.0.0.1"
    proxy_port = parsed.port or 9050

    # Check Tor SOCKS
    if not _port_open(proxy_host, proxy_port, timeout=2):
        write_result({"error": "tor_unreachable", "detail": f"{proxy_host}:{proxy_port}"})
        return

    from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

    with sync_playwright() as pw:
        browser = pw.firefox.launch(
            headless=True,
            firefox_user_prefs={
                "network.proxy.socks": proxy_host,
                "network.proxy.socks_port": proxy_port,
                "network.proxy.socks_version": 5,
                "network.proxy.socks_remote_dns": True,
                "network.proxy.type": 1,
                "network.proxy.no_proxies_on": "",
                "javascript.enabled": True,
            },
        )
        try:
            ctx = browser.new_context(
                viewport={"width": 1280, "height": 800},
                user_agent="Mozilla/5.0 (Windows NT 10.0; rv:128.0) Gecko/20100101 Firefox/128.0",
            )
            page = ctx.new_page()
            page.set_default_timeout(TIMEOUT_MS)

            # Navigate to search page - try onion first
            url = f"{AHMIA_ONION}/search/?q={quote_plus(query)}"
            try:
                page.goto(url, wait_until="domcontentloaded", timeout=TIMEOUT_MS)
            except Exception:
                # Clearnet fallback
                url = f"{AHMIA_CLEARNET}/search/?q={quote_plus(query)}"
                page.goto(url, wait_until="domcontentloaded", timeout=TIMEOUT_MS)
                proxy_v = proxy  # no socks5h for clearnet

            # Wait for JS render
            try:
                page.wait_for_selector("ol.searchResults li.result, a[href*='.onion']", timeout=30000)
            except PWTimeout:
                pass

            results = _extract_results(page, limit)

            if results:
                write_result({"results": results})
            else:
                # Try form submit if direct URL didn't work
                try:
                    qinput = page.query_selector("#id_q, input[name='q'], .search-q")
                    if qinput:
                        qinput.fill(query)
                        page.keyboard.press("Enter")
                        page.wait_for_timeout(5000)
                        results = _extract_results(page, limit)
                except Exception:
                    pass

                if results:
                    write_result({"results": results})
                else:
                    html = page.content()
                    if "non-JavaScript" in html or "non-javascript" in html.lower():
                        write_result({"error": "render_failed", "detail": "non-JS page"})
                    else:
                        write_result({"results": []})

        except PWTimeout as e:
            write_result({"error": "timeout", "detail": str(e)[:200]})
        except Exception as e:
            estr = str(e)
            if "timeout" in estr.lower():
                write_result({"error": "timeout", "detail": estr[:200]})
            else:
                write_result({"error": "render_failed", "detail": estr[:200]})
        finally:
            try:
                browser.close()
            except Exception:
                pass


def _extract_results(page, limit: int) -> list[dict]:
    results = []
    try:
        links = page.query_selector_all("a[href*='.onion']")
        for a in links[:limit]:
            href = a.get_attribute("href") or ""
            title = a.inner_text().strip()
            if title:
                # Normalize URL
                if href.startswith("//"):
                    href = "http:" + href
                results.append({
                    "title": title,
                    "url": href,
                    "snippet": "",
                })
    except Exception:
        pass
    return results


def _port_open(host: str, port: int, timeout: int) -> bool:
    try:
        sock = socket.create_connection((host, port), timeout=timeout)
        sock.close()
        return True
    except OSError:
        return False


def write_result(data: dict) -> None:
    json.dump(data, sys.stdout, ensure_ascii=False, indent=2)
    print(flush=True)


if __name__ == "__main__":
    main()
