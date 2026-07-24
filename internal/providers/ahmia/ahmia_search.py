#!/usr/bin/env python3
"""
Ahmia .onion search via Scrapling StealthyFetcher (Playwright) over Tor SOCKS5.

Reads from stdin:   {"query": "...", "limit": 10, "proxy": "socks5://127.0.0.1:9050"}
Writes to stdout:   {"results": [{"title": "...", "url": "...", "snippet": "..."}], "error": null}

On failure:         {"error": "render_failed|timeout|tor_unreachable|helper_error", "detail": "..."}
"""

from __future__ import annotations

import json
import os
import re
import sys
import time
from urllib.parse import quote_plus

AHMIA_ONION = "http://juhanurmihxlp77nkq76byazcldy2hlmovfu2epvl5ankdibsot4csyd.onion/search/?q={query}"
NON_JS_MARKERS = ["non-JavaScript", "not deploy", "non-javascript"]

TIMEOUT_MS = int(os.environ.get("TIMEOUT_MS", "60000"))


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
        write_result({"error": "helper_error", "detail": "query is required"})
        return

    # Normalize proxy to socks5h for .onion DNS resolution
    if proxy.startswith("socks5://"):
        proxy_socks5h = proxy.replace("socks5://", "socks5h://", 1)
    else:
        proxy_socks5h = proxy

    url = AHMIA_ONION.format(query=quote_plus(query))

    try:
        results = scrapling_search(url, proxy_socks5h, limit)
        write_result({"results": results})
    except ScraplingTimeout:
        write_result({"error": "timeout", "detail": "Scrapling render timeout"})
    except TorUnreachable:
        write_result({"error": "tor_unreachable", "detail": f"proxy {proxy} unreachable"})
    except RenderFailed as e:
        write_result({"error": "render_failed", "detail": str(e)})
    except ImportError as e:
        write_result({"error": "helper_error", "detail": f"Scrapling/Playwright import: {e}"})


class ScraplingTimeout(Exception):
    pass


class TorUnreachable(Exception):
    pass


class RenderFailed(Exception):
    pass


def scrapling_search(url: str, proxy: str, limit: int) -> list[dict]:
    """Use Scrapling StealthyFetcher (Playwright) to render Ahmia through Tor."""
    import scrapling
    from scrapling import StealthyFetcher

    # StealthyFetcher uses Playwright under the hood with anti-detection
    fetcher = StealthyFetcher(
        headless=True,
        proxy=proxy,
        stealth=True,
    )

    page = None
    try:
        page = fetcher.get(url, timeout=TIMEOUT_MS)
    except Exception as e:
        err = str(e).lower()
        if "timeout" in err or "timed out" in err:
            raise ScraplingTimeout()
        if "proxy" in err or "connect" in err or "dns" in err or "tor" in err:
            raise TorUnreachable()
        raise RenderFailed(f"fetch error: {e}")

    if page is None:
        raise RenderFailed("fetcher returned None")

    # Wait for JS to settle
    try:
        page.wait_for_selector("a[href*=\".onion\"]", timeout=15000)
    except Exception:
        pass  # results may not use .onion links directly

    html = page.content()
    text = page.text() or ""

    # Check for non-JS marker
    for marker in NON_JS_MARKERS:
        if marker in text:
            raise RenderFailed(f"non-JS page (JS never ran), marker: {marker}")

    # Parse results
    results = parse_ahmia_results(html, limit)

    if results:
        return results

    # Fallback: extract from JS-evaluated content
    try:
        js_results = page.evaluate("""
            () => {
                const items = [];
                const links = document.querySelectorAll('a[href*=".onion"]');
                links.forEach(a => {
                    const parent = a.closest('li, .result, [class*="result"], div');
                    if (parent) {
                        const snippet = parent.querySelector('p, .snippet, .description, .text');
                        items.push({
                            title: (a.textContent || '').trim(),
                            url: a.getAttribute('href') || '',
                            snippet: snippet ? (snippet.textContent || '').trim() : ''
                        });
                    }
                });
                if (items.length === 0 && links.length > 0) {
                    links.forEach(a => {
                        items.push({
                            title: (a.textContent || '').trim(),
                            url: a.getAttribute('href') || '',
                            snippet: ''
                        });
                    });
                }
                return items;
            }
        """)
        if js_results:
            return js_results[:limit]
    except Exception:
        pass

    # Last-resort regex for .onion links
    results = parse_onion_links(html, limit)
    if results:
        return results

    raise RenderFailed("no results found after render")


def parse_ahmia_results(html: str, limit: int) -> list[dict]:
    """Parse Ahmia HTML for search results with multiple strategies."""
    results = []
    strategies = [
        (r'<div[^>]*class="result"[^>]*>(.*?)</div>', re.DOTALL),
        (r'<li[^>]*class="result"[^>]*>(.*?)</li>', re.DOTALL),
        (r'<div[^>]*class="search-result"[^>]*>(.*?)</div>', re.DOTALL),
        (r'<li[^>]*class="search-result"[^>]*>(.*?)</li>', re.DOTALL),
    ]

    for pat, flags in strategies:
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
                    if title or url:
                        results.append({
                            "title": title,
                            "url": url,
                            "snippet": snippet[:300],
                        })
                        if len(results) >= limit:
                            return results
    return results


def parse_onion_links(html: str, limit: int) -> list[dict]:
    """Extract .onion links as last fallback."""
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
    json.dump(data, sys.stdout, ensure_ascii=False, indent=2)
    print()


if __name__ == "__main__":
    main()
