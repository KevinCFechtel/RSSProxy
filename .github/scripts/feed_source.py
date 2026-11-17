"""Controllable, network-local RSS source for the real-container CI tests."""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PUBLICATION = "Mon, 16 Nov 2026 12:00:00 +0000"


class FeedSource(ThreadingHTTPServer):
    def __init__(self, address):
        super().__init__(address, Handler)
        self.lock = threading.Lock()
        self.version = 1
        self.mode = "healthy"
        self.requests = 0
        self.last_conditional = ""


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def respond(self, status, body=b"", headers=None):
        self.send_response(status)
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            self.respond(200, b"OK\n")
            return
        with self.server.lock:
            if self.path == "/state":
                body = json.dumps(
                    {
                        "version": self.server.version,
                        "mode": self.server.mode,
                        "requests": self.server.requests,
                        "last_conditional": self.server.last_conditional,
                    }
                ).encode()
                self.respond(200, body, {"Content-Type": "application/json"})
                return
            if self.path != "/feed.xml":
                self.respond(404)
                return
            self.server.requests += 1
            self.server.last_conditional = self.headers.get("If-None-Match", "")
            mode, version = self.server.mode, self.server.version
        if mode == "error":
            self.respond(503, b"Controlled upstream failure\n")
            return
        etag = f'"fixture-v{version}"'
        if self.headers.get("If-None-Match") == etag:
            self.respond(304, headers={"ETag": etag})
            return
        title, slug, publication = "Original article", "old-heading", PUBLICATION
        if version == 2:
            title, slug = "Updated article", "new-heading"
            publication = "Tue, 17 Nov 2026 12:00:00 +0000"
        body = f"""<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/DTDs/PodCast-1.0.dtd">
<channel><title>Original feed</title><link>https://example.org</link><description>CI news</description>
<image><url>https://example.org/old-logo.png</url><title>Old logo</title><link>https://example.org</link></image>
<item><title>{title}</title><description>{title} text</description>
<guid isPermaLink="true">https://example.org/{slug}-123.html</guid>
<link>https://example.org/{slug}-123.html</link><pubDate>{publication}</pubDate>
<itunes:duration>01:23:45</itunes:duration>
<enclosure url="https://example.org/episode.mp3" type="audio/mpeg" length="123456"/>
</item></channel></rss>""".encode()
        self.respond(200, body, {"Content-Type": "application/rss+xml", "ETag": etag})

    def do_POST(self):
        if self.path != "/state":
            self.respond(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 1024:
                raise ValueError("invalid body size")
            state = json.loads(self.rfile.read(length))
            version = state.get("version", 1)
            mode = state.get("mode", "healthy")
            if version not in (1, 2) or mode not in ("healthy", "error"):
                raise ValueError("invalid state")
        except (ValueError, TypeError, AttributeError):
            self.respond(400)
            return
        with self.server.lock:
            self.server.version, self.server.mode = version, mode
            if state.get("reset", False):
                self.server.requests = 0
                self.server.last_conditional = ""
        self.respond(204)


if __name__ == "__main__":
    with FeedSource(("0.0.0.0", 8080)) as server:
        server.serve_forever()
