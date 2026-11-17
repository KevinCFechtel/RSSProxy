"""Exercise the built container through HTTP, without Go or third-party packages."""

import argparse
import base64
import concurrent.futures
import http.cookiejar
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

from feed_source import PUBLICATION


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


def require(condition, message):
    if not condition:
        raise AssertionError(message)


class Smoke:
    def __init__(self):
        self.base = os.environ.get("RSSPROXY_URL", "http://127.0.0.1:18080").rstrip("/")
        self.fixture = os.environ.get("FIXTURE_URL", "http://127.0.0.1:18081").rstrip("/")
        self.source = os.environ.get("FIXTURE_SOURCE_URL", "http://fixture:8080/feed.xml")
        self.result = Path(os.environ.get("SMOKE_RESULT", ".ci-artifacts/smoke-result.json"))
        self.auth = base64.b64encode(
            f"{os.environ['AUTH_USER']}:{os.environ['AUTH_PASSWORD']}".encode()
        ).decode()
        self.cookies = http.cookiejar.CookieJar()
        self.admin = urllib.request.build_opener(
            urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(self.cookies),
            NoRedirect(),
        )

    def request(self, path, *, method="GET", data=None, headers=None, admin=False, fixture=False):
        # Public requests deliberately use independent, unauthenticated clients.
        opener = self.admin if admin else urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect()
        )
        request_headers = dict(headers or {})
        if admin:
            request_headers["Authorization"] = "Basic " + self.auth
        if fixture and data is not None:
            data = json.dumps(data).encode()
            request_headers["Content-Type"] = "application/json"
        elif data is not None:
            data = urllib.parse.urlencode(data).encode()
            request_headers["Content-Type"] = "application/x-www-form-urlencoded"
        request = urllib.request.Request(
            (self.fixture if fixture else self.base) + path,
            data=data,
            headers=request_headers,
            method=method,
        )
        try:
            response = opener.open(request, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def state(self):
        status, _, body = self.request("/state", fixture=True)
        require(status == 200, f"fixture state: HTTP {status}")
        return json.loads(body)

    def set_state(self, version, mode="healthy", reset=False):
        status, _, _ = self.request(
            "/state", method="POST", fixture=True,
            data={"version": version, "mode": mode, "reset": reset},
        )
        require(status == 204, f"fixture update: HTTP {status}")

    def article(self, response, title):
        status, headers, body = response
        require(status == 200, f"feed returned HTTP {status}: {body!r}")
        require(headers.get_content_type() == "application/rss+xml", "wrong RSS content type")
        root = ET.fromstring(body)
        require(root.tag == "rss" and root.attrib.get("version") == "2.0", "not RSS 2.0")
        require(root.findtext("channel/title") == "CI feed", "feed title override missing")
        require(
            root.findtext("channel/image/url") == "https://example.org/replacement.png",
            "feed image override missing",
        )
        items = root.findall("channel/item")
        require(len(items) == 1, "expected exactly one current article")
        item = items[0]
        require(item.findtext("title") == title, "article content is stale")
        guid = item.find("guid")
        require(guid is not None and guid.attrib.get("isPermaLink") == "false", "invalid proxy GUID")
        require(guid.text.startswith("urn:uuid:"), "missing stable proxy UUID")
        require(item.findtext("pubDate") == PUBLICATION, "original publication date changed")
        enclosure = item.find("enclosure")
        require(
            enclosure is not None and enclosure.attrib.get("url") == "https://example.org/episode.mp3",
            "podcast enclosure was damaged",
        )
        require(
            item.findtext("{http://www.itunes.com/DTDs/PodCast-1.0.dtd}duration") == "01:23:45",
            "podcast metadata was lost",
        )
        return guid.text, headers["ETag"]

    def run(self):
        self.set_state(1, reset=True)
        for path in ("/healthz", "/readyz"):
            require(self.request(path)[0] == 200, f"{path} failed")
        require(self.request("/admin/")[0] == 401, "admin accessible without authentication")
        status, _, body = self.request("/admin/feeds/new", admin=True)
        require(status == 200, "authenticated admin form unavailable")
        match = re.search(rb'name="csrf" value="([a-f0-9]+)"', body)
        require(match is not None, "CSRF token missing")
        config = {
            "name": "CI fixture", "path": "/ci-news", "url": self.source,
            "identity_source": "guid", "identity_kind": "regex",
            "identity_regex": r"-(?P<id>[0-9]+)\.html$", "identity_hosts": "example.org",
            "item_image_source": "auto", "title_mode": "replace", "title_value": "CI feed",
            "image_url_mode": "replace", "image_url_value": "https://example.org/replacement.png",
        }
        require(
            self.request("/admin/feeds/save", method="POST", data=config, admin=True)[0] == 403,
            "form without CSRF token was accepted",
        )
        config["csrf"] = match.group(1).decode()
        require(
            self.request("/admin/feeds/save", method="POST", data=config, admin=True)[0] == 303,
            "saving fixture configuration failed",
        )
        guid, etag = self.article(self.request("/ci-news"), "Original article")
        status, _, body = self.request("/ci-news", headers={"If-None-Match": etag})
        require(status == 304 and not body, "reader revalidation did not return an empty 304")
        state = self.state()
        require(state["requests"] == 2, "matching reader ETag bypassed the source")
        require(state["last_conditional"] == '"fixture-v1"', "upstream ETag was not forwarded")

        self.set_state(2)
        new_guid, updated_etag = self.article(
            self.request("/ci-news", headers={"If-None-Match": etag}), "Updated article"
        )
        require(new_guid == guid and updated_etag != etag, "article update changed GUID or kept old ETag")
        self.set_state(2, "error")
        status, _, body = self.request("/ci-news", headers={"If-None-Match": updated_etag})
        require(status == 503 and b"<rss" not in body, "source failure was hidden by stale data")
        self.set_state(2)
        require(self.article(self.request("/ci-news"), "Updated article")[0] == guid, "recovery lost identity")
        status, _, body = self.request("/ci-news", method="HEAD")
        require(status == 200 and not body, "HEAD did not preserve HTTP semantics")

        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            responses = list(pool.map(lambda _: self.request("/ci-news"), range(4)))
        for response in responses:
            require(self.article(response, "Updated article")[0] == guid, "concurrent identity mismatch")
        require(self.state()["requests"] == 10, "reader calls were cached or coalesced")
        self.result.parent.mkdir(parents=True, exist_ok=True)
        self.result.write_text(json.dumps({"guid": guid}), encoding="utf-8")
        print("Container smoke test passed: auth, CSRF, RSS, updates, 304, errors, HEAD, concurrency")

    def check_persistence(self):
        expected = json.loads(self.result.read_text(encoding="utf-8"))["guid"]
        before = self.state()["requests"]
        guid, _ = self.article(self.request("/ci-news"), "Updated article")
        require(guid == expected, "container restart lost configuration or article identity")
        require(self.state()["requests"] == before + 1, "post-restart reader call did not contact source")
        print("Container restart preserved feed configuration, source snapshot, and proxy GUID")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--check-persistence", action="store_true")
    args = parser.parse_args()
    try:
        smoke = Smoke()
        if args.check_persistence:
            smoke.check_persistence()
        else:
            smoke.run()
    except (AssertionError, OSError, ValueError, ET.ParseError) as error:
        print(f"Container smoke test failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
