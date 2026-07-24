#!/usr/bin/env python3
"""
Ahmia .onion search via Scrapling (Playwright) over Tor SOCKS5.

Reads from stdin:   {"query": "...", "limit": 10, "proxy": "socks5://127.0.0.1:9050"}
Writes to stdout:   {"results": [{"title": "...", "url": "...", "snippet": "..."}], "error": null}

On failure:         {"error": "render_failed|timeout|tor_unreachable", "detail": "..."}
"""

from __future__ import annotations

import json
import os
import re
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import quote_plus

AHMIA_ONION = "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion/search/?q={query}"
AHMIA_CLEARNET = "https://ahmia.fi/search/?q={query}"

TIMEOUT_MS = int(os.environ.get("TIMEOUT_MS", "30000"))
BROWSER_HEADLESS = os.environ.get("BROWSER_HEADLESS", "1") == "1"


def main():
    raw = sys.stdin.read() if not sys.stdin.isatty() else "{}"
    try:
        req = json.loads(raw)
    except json.JSONDecodeError:
        req = {}

    query = req.get("query", "")
    limit = req.get("limit", 10)
    proxy = req.get("proxy", os.environ.get("NOLE_PROXY_URL", "socks5://127.0.0.1:9050"))

    if not query:
        write_result({"error": "no_query", "detail": "query is required"})
        return

    # Build Ahmia onion URL
    url = AHMIA_ONION.format(query=quote_plus(query))

    try:
        results = render_and_extract(url, proxy, limit)
        write_result({"results": results})
    except ScraplingTimeout:
        write_result({"error": "timeout", "detail": "render did not complete within timeout"})
    except TorUnreachable:
        write_result({"error": "tor_unreachable", "detail": f"cannot connect to proxy {proxy}"})
    except RenderFailed as e:
        write_result({"error": "render_failed", "detail": str(e)})
    except ScraplingNotFound:
        write_result({"error": "scrapling_not_found", "detail": "scrapling or playwright not installed"})


class ScraplingTimeout(Exception):
    pass


class TorUnreachable(Exception):
    pass


class RenderFailed(Exception):
    pass


class ScraplingNotFound(Exception):
    pass


def render_and_extract(url: str, proxy: str, limit: int) -> list[dict]:
    """Use Scrapling to render Ahmia search page through Tor, extract results."""
    try:
        import scrapling
        from scrapling import Fetcher
    except ImportError:
        raise ScraplingNotFound()

    fetcher = Fetcher(
        headless=BROWSER_HEADLESS,
        proxy=proxy,
        stealth=True,
    )

    try:
        page = fetcher.get(url, timeout_ms=TIMEOUT_MS)
    except ConnectionError:
        raise TorUnreachable()
    except Exception as e:
        if "timeout" in str(e).lower():
            raise ScraplingTimeout()
        raise RenderFailed(f"navigation error: {e}")

    if page is None:
        raise RenderFailed("fetcher returned None")

    # Check for non-JS banner
    body_text = page.text()
    if page.status == 200 and ("non-JavaScript" in body_text or "not deploy" in body_text):
        if len(body_text) < 2000:
            raise RenderFailed("non-JS page (JS never ran)")

    # Extract results - look for result-like elements
    results = []
    non_js_marker_found = "non-JavaScript" in body_text or "not deploy" in body_text

    # Try HTML parsing directly from the page content
    html = page.content()
    if html:
        results = parse_html_results(html, limit)

    if results:
        return results

    # Fallback: use page.evaluate for JS-rendered content
    try:
        js_results = page.evaluate("""
            () => {
                const items = [];
                document.querySelectorAll('a[href*=".onion"]').forEach(a => {
                    const parent = a.closest('li, .result, [class*="result"]');
                    if (parent) {
                        const snippet = parent.querySelector('p, .snippet, .description');
                        items.push({
                            title: a.textContent.trim(),
                            url: a.getAttribute('href'),
                            snippet: snippet ? snippet.textContent.trim() : ''
                        });
                    }
                });
                return items.slice(0, %d);
            }
        """ % limit)
        if js_results:
            return js_results
    except Exception:
        pass

    # Last resort: regex search for .onion links
    results = parse_onion_links(html, limit)
    if results and not non_js_marker_found:
        return results

    if non_js_marker_found:
        raise RenderFailed("non-JS page (JS never ran)")
    return results


def parse_html_results(html: str, limit: int) -> list[dict]:
    """Parse Ahmia HTML for result items."""
    results = []
    # Look for common patterns in search engine result pages
    patterns = [
        (r'<div[^>]*class="result"[^>]*>(.*?)</div>', re.DOTALL),
        (r'<li[^>]*class="result"[^>]*>(.*?)</li>', re.DOTALL),
        (r'<div[^>]*class="search-result"[^>]*>(.*?)</div>', re.DOTALL),
    ]

    for pat, flags in patterns:
        items = re.findall(pat, html, flags)
        if items:
            for item_html in items:
                title_m = re.search(r'<a[^>]*>(.*?)</a>', item_html, re.DOTALL)
                url_m = re.search(r'href="([^"]+)"', item_html)
                snippet_m = re.search(r'<p[^>]*>(.*?)</p>', item_html, re.DOTALL)
                if title_m and url_m:
                    title = re.sub(r'<[^>]+>', '', title_m.group(1)).strip()
                    url = url_m.group(1)
                    snippet = ""
                    if snippet_m:
                        snippet = re.sub(r'<[^>]+>', '', snippet_m.group(1)).strip()
                    results.append({
                        "title": title,
                        "url": url,
                        "snippet": snippet[:300],
                    })
                    if len(results) >= limit:
                        return results
    return results


def parse_onion_links(html: str, limit: int) -> list[dict]:
    """Extract .onion links as fallback."""
    onion_links = re.findall(
        r'<a[^>]*href="(https?://[^"]*\.onion[^"]*)"[^>]*>(.*?)</a>',
        html,
        re.DOTALL,
    )
    results = []
    for url, title_html in onion_links:
        title = re.sub(r'<[^>]+>', '', title_html).strip()
        if title:
            results.append({
                "title": title,
                "url": url,
                "snippet": "",
            })
            if len(results) >= limit:
                break
    return results


def write_result(data: dict):
    """Write JSON result to stdout."""
    json.dump(data, sys.stdout, ensure_ascii=False, indent=2)
    print()


if __name__ == "__main__":
    main()
