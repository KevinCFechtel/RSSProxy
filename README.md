# RSSProxy

A self-hosted RSS proxy written in Go: stable article identities despite changing headlines, updated articles, and configurable feed/image overrides.

## Behavior

- Every `GET` and `HEAD` request to a configured feed endpoint contacts the source. No time-based cache, polling, or coalescing of concurrent requests.
- Concurrent requests for the same feed are processed sequentially using a PostgreSQL session lock. **Each** performs its own source fetch. This prevents stale snapshots without keeping a database transaction open during the network request.
- Source ETag and Last-Modified headers are used for revalidation. On `304 Not Modified`, the last successfully processed source snapshot is used.
- Reader ETags are evaluated **only after fetching the source**. A matching reader ETag cannot hide a source error.
- Only the current source snapshot is returned. Requests do not consume articles. Each identity appears only once per feed; for duplicates, the last entry wins while retaining the position of the first occurrence.
- Article updates change the title, text, links, and media, but not the proxy GUID or original publication date. An initially missing publication date is adopted when the source first provides one. Content changes receive a separate, persisted modification timestamp (`atom:updated` in the RSS output). Fetching unchanged articles again does not reset it to “now”.
- Identities are retained for the lifetime of the feed configuration, even if articles disappear from the source. Complete articles outside the current snapshot are not archived.
- Source errors are passed to the reader, never replaced with an old feed as a successful response. Stored data is preserved on errors.
- RSS, Atom, and JSON Feed are supported as input. Output is RSS 2.0.

## Quick start with Docker Compose

```sh
cp .env.example .env
# Edit .env and replace both passwords with random values
docker compose up --build -d
```

Administration: **http://localhost:8080/admin/**. The username and password are set in `.env`. Add a feed with a source URL and public endpoint there. The public endpoint is the complete, freely configurable path, such as `/faz`; **no `/feeds/` prefix** is added. Feed endpoints are intentionally accessible **without authentication**.

**No home page at `/`:** A request to `http://localhost:8080/` intentionally returns `404 page not found`. The web administration is available at **`/admin/`**.

Compose exposes the HTTP port only on `127.0.0.1`. For network access, place an HTTPS reverse proxy in front of the service and set `COOKIE_SECURE=true`. Basic Auth without HTTPS is intended only for local development. Use TLS with certificate verification for external databases.

**Important:** Disable reverse-proxy caching for feed endpoints. Otherwise, reader requests may bypass the service and therefore the source.

### URLs behind a reverse proxy

All paths are appended to the public address, including any non-default port. Example for `https://rss.example.de:9443`:

| Purpose | Public URL | Expected behavior |
|---|---|---|
| Web administration | `https://rss.example.de:9443/admin/` | `401` without credentials; administration after authentication |
| Feed, if endpoint `/heise` is configured | `https://rss.example.de:9443/heise` | Exactly the configured path, without an additional prefix |
| Liveness | `https://rss.example.de:9443/healthz` | `200 OK` |
| Root path | `https://rss.example.de:9443/` | `404 page not found` – no home page |

RSSProxy does not need to know the public hostname or port. `LISTEN_ADDR` sets only the internal listening address (default `:8080`); port forwarding and the reverse proxy may use different ports. Forward paths and the original `Host` header unchanged, including any public port.

The administration shows the actual feed path as a clickable link under **Endpoint**. Legacy imports preserve `httpEndpoint` unchanged: `/heise` remains `/heise`, not `/feeds/heise`. `/feeds/…` works only when explicitly configured as part of the endpoint.

For troubleshooting, check `/healthz` both directly on the exposed container port and through the public address. If only direct access works, check reverse-proxy routing and path rewrites. Using `127.0.0.1` as the proxy target works only if the exposed port is reachable in the reverse proxy's network namespace, for example when Caddy runs directly on the same host; a separate Caddy container has its own loopback address.

## Running locally without Docker

Go 1.27 and a running PostgreSQL database are required:

```sh
export DATABASE_URL='postgres://rssproxy:password@localhost:5432/rssproxy?sslmode=disable'
export AUTH_USER=admin
export AUTH_PASSWORD='a-long-random-password'
go run ./cmd/rssproxy
```

The application does not read `.env` itself; Docker Compose uses the file for variable substitution. Native execution requires exported environment variables.

Versioned SQL migrations run at startup under a database lock. The database user needs DDL privileges for the application schema. Create a backup before production migrations. Tables use the `rssproxy_` prefix; legacy tables are not modified.

### Server configuration

| Variable | Default / Meaning |
|---|---|
| `DATABASE_URL` | Required; PostgreSQL URL or pgx/libpq connection parameters |
| `AUTH_USER`, `AUTH_PASSWORD` | Required; administration credentials |
| `LISTEN_ADDR` | `:8080` |
| `COOKIE_SECURE` | `false` for local HTTP; `true` for HTTPS |
| `SOURCE_TIMEOUT` | `20s`, including redirects and response body |
| `REQUEST_TIMEOUT` | `60s`, including waiting for the feed lock and database processing |
| `MAX_FEED_BYTES` | `10485760` decompressed bytes |
| `ALLOWED_NETWORKS` | Optional comma-separated CIDR allowlist for internal sources |
| `LEGACY_DATABASE_URL` | Optional separate goRssDedup database for the initial import |

For `DATABASE_URL`, `LEGACY_DATABASE_URL`, `AUTH_USER`, and `AUTH_PASSWORD`, `<NAME>_FILE` can be used instead to read from secret files. Do not specify both a value and a file. pgx also supports its usual `PG*` environment variables; Compose uses `PGPASSWORD` so that special characters in passwords do not need to be interpolated into a URL.

## Stable article identities

The internal feed name, source GUID, provider ID, and public proxy GUID are separate. The reader receives a permanent UUID URN with `isPermaLink="false"`.

Four strategies are available per feed:

1. **Full value:** Use the source GUID or article link unchanged as the key.
2. **Regex:** Extract the named capture group `(?P<id>...)` from the GUID or article link. For HTTP(S) URLs, only the path is checked, not the host or query.
3. **Query:** Read a single URL parameter, such as `articleId`.
4. **Legacy:** Use the old byte-truncation rule for imports.

Optional allowed article hosts are checked separately. `isPermaLink=true` does not guarantee an immutable URL and therefore does not bypass extraction. Rules that do not match or missing identity values cause a visible error, **not** a fallback to a changing GUID.

### Examples

**FAZ**, source GUID or article link, regex:

```regex
-(?P<id>[0-9]+)\.html$
```

`https://www.faz.net/aktuell/sport/alte-ueberschrift-accg-201686703.html` becomes `201686703`. Example allowed hosts: `www.faz.net faz.net`.

**Handelsblatt**, Regex:

```regex
/(?P<id>[0-9]+)\.html$
```

`https://www.handelsblatt.com/finanzen/neue-ueberschrift/100270206.html` becomes `100270206`. Example allowed hosts: `www.handelsblatt.com handelsblatt.com`.

Rules can be tested in the administration without saving or making network requests: use your own example lines or up to 50 entries from the last successful fetch.

When a rule changes, known source GUIDs/links can connect to the existing proxy identity through historical aliases. Ambiguous mappings cause an error. An arbitrary rule change cannot reliably reconstruct identities if all known source values change at the same time. Test such changes with real feed examples first.

## Grouped feeds (deduplicated merged RSS)

Create normal source feeds first in `/admin/`. Then choose **Add group**, give the group a
name and public endpoint, and select up to 8 existing regular feeds. Subscribe your RSS
reader to **the group endpoint only** rather than to every member feed.

- Every successful group request contacts every source, using each source's stored
  ETag/Last-Modified and snapshot for revalidation. There is no time-based cache.
- Group identity extraction is configurable, independently of source feed rules.
- The first listed source wins duplicate article metadata; original item categories
  from all copies are combined, retaining category domains. Items are sorted by
  publication time (newest first), falling back to update time; undated items come last.
- Artwork, enclosures and article metadata are retained with source-relative URL
  resolution. Group overrides apply to the merged output; individual sources'
  presentation overrides do not.
- Group GUIDs, original publication dates and modification timestamps are persisted.
  Switching an existing subscription to a group may show articles as new once.
  Individual source endpoints remain available and keep their own identities.
- If any source fails, the whole group request fails without returning a partial or
  stale successful response. Remaining sources are not fetched after a failure.
- Referenced sources cannot be deleted until removed from all groups. Groups cannot
  contain groups, and an existing feed cannot be converted between source and group.
- Database migration `003_feed_groups.sql` runs automatically at startup.

## Feed and image overrides

You can configure the feed title, description, website link, feed image/logo, image title, image link, and a fixed article image.

Modes: **Keep original**, **Always replace**, **Fill in only if missing**, **Remove**. Required RSS fields cannot be removed. If required subfields of an optional RSS image are removed, the entire image is omitted.

Article image source: `auto`, `image`, `enclosure` (image attachments only), or `namespace:element`, such as `media:content`, `media:thumbnail`, or `itunes:image`.

- Images are referenced by URL, not downloaded or hosted.
- Overrides are applied only when rendering the output; original data is preserved.
- Saving an override does not require an additional source fetch. The next reader request contacts the source as usual and applies the new override even after a source `304`.
- Audio/video enclosures are preserved independently of article image overrides.
- RSS enclosure lengths are byte counts, never image widths. Missing lengths are returned as `0` (unknown).
- XML extensions, such as iTunes, Media RSS, and Podcast Namespace, are preserved semantically with namespaces, attributes, and child order. Prefixes and XML syntax may change. This is not a lossless format round trip: normalized RSS/Atom/JSON core fields are mapped to RSS 2.0; source self/pagination links are not retained.
- Some readers prefer their own favicons or cache images independently of the feed.

Feeds without a title or website link require appropriate overrides. An empty or missing feed description, however, is returned as an empty `<description>` element without inventing replacement text. This allows, for example, replacing only the Capital feed's logo while leaving other metadata unchanged. Invalid entries cause the entire request to fail rather than silently dropping articles.

To replace only the feed icon, open **Edit / Preview → Overrides**, select **Always replace** for **Feed image / Logo URL**, and enter the desired image URL. Leave all other overrides set to **Keep original**; do not change article image settings.

### Error: missing feed title or website link

The message `RSS requires channel title and website link; configure overrides for missing values` means that the title or website link is empty after applying overrides. It does not indicate an error in the article identity rule, even if the outer error message says `invalid source feed or identity rule`.

Under `/admin/`, open **Edit / Preview → Overrides** for the affected feed. For missing **Feed title** or **Website link** values, select **Fill in only if missing** and enter suitable replacements. The website link is the HTTP(S) address of the provider's website, not the public proxy endpoint. Save and fetch the feed again. **Name (internal)** is not automatically used as the feed title or description.

Older versions also mention `description` in this error and reject empty descriptions. If the source feed provides an empty description, as `https://www.capital.de/feed/standard/` does, deploy a version with the corresponding fix; an additional description override is then unnecessary.

## Importing from goRssDedup

At startup, RSSProxy automatically checks whether `rssDedupConfig` exists in the target database. For a separate legacy database, set `LEGACY_DATABASE_URL` or `LEGACY_DATABASE_URL_FILE`. A read-only user is recommended for that database.

Internal names, public endpoints, source URLs, image settings, and truncation rules are imported. The legacy source is read only within a read-only transaction. Existing target endpoints are not overwritten. An import marker prevents repeated imports and the recreation of configurations intentionally deleted later.

The legacy article image special value `NoImage` is imported as “Remove article image”. Leading and trailing whitespace in the image source is trimmed during import.

Invalid configurations cause the import to fail atomically and visibly. Without a legacy table, a new installation starts empty. If a separate legacy database is explicitly configured but unreachable or missing the expected table, startup fails with an error.

Check imported truncation rules using the preview. “End” continues to use a GUID suffix of the specified offset length. “Start” now works and counts the offset from the beginning. Both variants guard against invalid bounds.

**The legacy table of already consumed article IDs is not imported.** It does not contain complete original GUIDs or articles. Because of the new proxy GUIDs, known articles may reappear once in the reader when migrating.

## Errors and security

| Situation | Reader response |
|---|---|
| Source timeout | `504` |
| Connection failure, blocked destination, invalid feed, or extraction error | `502` |
| Source HTTP error | Corresponding error status, e.g. `429` or `503` |
| Source `401` / `407` | `502`, to avoid triggering authentication on the public proxy |
| Database/internal error | `500` |
| Unknown feed | `404` |

Valid `Retry-After` headers are forwarded on source errors. No automatic retries or hidden fallback responses.

SSRF protection checks DNS responses and connects only to validated IP addresses. Private, local, link-local, metadata, and other special networks are blocked by default. Redirects undergo the same checks; HTTPS downgrades and environment proxies are disabled. Explicitly allow only specific internal feed destinations using `ALLOWED_NETWORKS`. Supplement this with network egress rules.

The administration uses constant-time comparison of hashed Basic Auth credentials, CSRF tokens, cross-origin checks, HTML escaping, CSP, and bounded form data. The service has HTTP/database deadlines and size/XML structure limits. Configure login rate limits at the reverse proxy before public deployment.

Status is logged in a structured format without credentials. Source URLs containing secret query parameters are visible to administrators in the configuration and should be protected accordingly.

## Operations

- `GET /healthz`: process liveness.
- `GET /readyz`: database connectivity.
- `rssproxy healthcheck`: readiness check without additional programs, including in the scratch container.
- `rssproxy version`: build version.
- `SIGTERM` / `SIGINT`: finish in-flight requests up to the shutdown deadline.

Consider wait times under high load: independent source fetches for the same feed are processed sequentially; a waiting request may time out after `REQUEST_TIMEOUT`. Source request frequency and bandwidth intentionally reflect reader usage. Large feeds and long identity histories require adequate PostgreSQL capacity and backups.

## GitHub Actions, security, and container releases

The workflow [`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) is based on [CupNotifyer](https://github.com/KevinCFechtel/CupNotifyer). It runs on pull requests, pushes to `main`, `v*` tags, manual triggers, and weekly on Mondays. Actions are pinned to commit SHAs; permissions are restricted to the minimum required per job.

**For regular changes and pull requests:**

- Formatting, race tests including real PostgreSQL integration tests, coverage, `go vet`, binary build, and fuzzing.
- Reachable Go vulnerability checks with `govulncheck`, CodeQL, and Dependency Review on pull requests; Dependency Review blocks new vulnerabilities rated `moderate` or higher.
- Native container builds **and execution** on both `linux/amd64` and `linux/arm64`. Unlike CupNotifyer, RSSProxy tests containers before a release to compensate for the lack of a local Docker environment.
- CI starts the actual built scratch image, PostgreSQL, and a controlled feed source using Compose. It checks non-root/read-only operation, build version, healthcheck, authentication/CSRF, real RSS requests, image overrides, podcast metadata, article updates with stable GUIDs, `304`, source errors, and concurrent requests without coalescing. Configuration and identity must survive a restart.
- A SHA256-pinned Trivy container scans the AMD64 image for `HIGH`/`CRITICAL` vulnerabilities. ARM64 is built natively and functionally tested; the scanner digest inherited from CupNotifyer is a single-platform image.
- Coverage and container diagnostics are available as workflow artifacts for 14 days. Existing Trivy results are also uploaded to GitHub Security when the PR context allows it.

Regular commits and pull requests **do not publish images**. The container test uses only dedicated test credentials, an isolated Compose project name, and temporary volumes. These checks do not require a local Docker installation; they run on GitHub runners after pushing.

**For a valid release tag, such as `v1.2.3`:**

After successful Go/security checks and container tests on both architectures, a multi-platform image with SBOM and provenance is published to `ghcr.io/kevincfechtel/rssproxy`. The tag is embedded in the binary as the build version.

- Stable releases receive version tags with and without `v`, a `major.minor` alias, a commit SHA tag, and `latest`.
- Prereleases such as `v1.3.0-rc.1` receive their own version/SHA tags but **do not** overwrite `latest` or the stable `major.minor` alias.
- The automatically provided `GITHUB_TOKEN` is sufficient for publishing; no Docker Hub password is needed.

```sh
docker pull ghcr.io/kevincfechtel/rssproxy:latest
```

The workflow files must first be committed and pushed to GitHub. For a newly created GHCR package, check its visibility before expecting unauthenticated pulls to work. CodeQL and Dependency Review require the corresponding GitHub features; the current RSSProxy repository is public. Dependabot updates Go dependencies, Docker base images, and Actions weekly.

## Development and testing

```sh
make build
go test -race ./...
go vet ./...
govulncheck ./...
```

Provide a **test database** for the PostgreSQL/HTTP integration tests:

```sh
export TEST_DATABASE_URL='postgres://rssproxy:password@localhost:5432/rssproxy_test?sslmode=disable'
go test -race -count=1 ./...
```

Each integration test creates an isolated, randomly named schema and removes only that schema afterward. Without `TEST_DATABASE_URL`, these tests are explicitly skipped. CI runs them with PostgreSQL, in addition to build, vet, fuzzing, and vulnerability checks.

The CI helpers for release tags and the controlled feed source can be tested independently of Docker:

```sh
python3 -m unittest discover -s .github/scripts -p 'test_*.py' -v
```

```sh
go test ./internal/feed -run '^$' -fuzz '^FuzzIdentityNeverPanics$' -fuzztime=30s
go test ./internal/feed -run '^$' -fuzz '^FuzzParseNeverPanics$' -fuzztime=30s
```

Project structure:

```text
cmd/rssproxy       Application startup and shutdown
internal/app       Server configuration, routing, end-to-end tests
internal/feed      Identity rules, parser, RSS output, and overrides
internal/fetch     HTTP fetching and SSRF protection
internal/proxy     Reader request processing
internal/store     PostgreSQL, migrations, identities, and legacy import
internal/web       Server-rendered administration
```

License: [BSD 3-Clause](LICENSE).
