import json
import threading
import unittest
import urllib.error
import urllib.request

from feed_source import FeedSource
from release_version import validate


class ReleaseVersionTests(unittest.TestCase):
    def test_stable_releases(self):
        for tag in ("v0.0.0", "v1.2.3", "v12.34.56+build.007", "v1.2.3+build-with-hyphen"):
            with self.subTest(tag=tag):
                self.assertTrue(validate(tag))

    def test_prereleases(self):
        for tag in ("v1.2.3-rc.1", "v0.1.0-0", "v1.2.3-alpha-1+build.7"):
            with self.subTest(tag=tag):
                self.assertFalse(validate(tag))

    def test_invalid_tags(self):
        for tag in (
            "", "1.2.3", "v1.2", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-01",
            "v1.2.3-.", "v1.2.3-a..b", "v1.2.3+", "v1.2.3+build..1",
            "v1.2.3\n", "v1.2.3; echo unsafe",
        ):
            with self.subTest(tag=tag):
                with self.assertRaises(ValueError):
                    validate(tag)


class FeedSourceTests(unittest.TestCase):
    def setUp(self):
        self.server = FeedSource(("127.0.0.1", 0))
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"
        self.client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def request(self, path, **kwargs):
        try:
            response = self.client.open(urllib.request.Request(self.base + path, **kwargs), timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.headers, response.read()

    def test_conditional_requests_and_error_priority(self):
        status, headers, body = self.request("/feed.xml")
        self.assertEqual(status, 200)
        self.assertIn(b"Original article", body)
        status, _, body = self.request("/feed.xml", headers={"If-None-Match": headers["ETag"]})
        self.assertEqual((status, body), (304, b""))
        self.request("/state", method="POST", data=b'{"version":2,"mode":"error"}')
        self.assertEqual(self.request("/feed.xml", headers={"If-None-Match": '"fixture-v2"'})[0], 503)
        state = json.loads(self.request("/state")[2])
        self.assertEqual(state["requests"], 3)
        self.assertEqual(state["last_conditional"], '"fixture-v2"')

    def test_health_checks_do_not_count_as_source_requests(self):
        self.assertEqual(self.request("/healthz")[0], 200)
        self.assertEqual(json.loads(self.request("/state")[2])["requests"], 0)
        self.assertEqual(self.request("/state", method="POST", data=b'{"version":3}')[0], 400)


if __name__ == "__main__":
    unittest.main()
