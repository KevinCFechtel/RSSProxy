# RSSProxy

Ein selbst gehosteter RSS-Proxy in Go: stabile Artikelidentitäten trotz geänderter Überschriften, aktualisierte Artikel und konfigurierbare Feed-/Bild-Ersetzungen.

## Verhalten

- Jeder `GET`- und `HEAD`-Abruf eines konfigurierten Feed-Endpunkts kontaktiert die Quelle. Kein zeitbasierter Cache, kein Polling und keine Zusammenlegung paralleler Anfragen.
- Parallele Abrufe desselben Feeds werden über eine PostgreSQL-Session-Sperre nacheinander verarbeitet. **Jeder** führt einen eigenen Quellabruf aus. Das verhindert veraltete Snapshots, ohne während des Netzwerkabrufs eine DB-Transaktion offenzuhalten.
- Quell-ETag und Last-Modified werden zur Revalidierung verwendet. Bei `304 Not Modified` wird der letzte erfolgreich verarbeitete Quellbestand verwendet.
- Reader-ETags werden **erst nach dem Quellabruf** ausgewertet. Ein passender Reader-ETag kann keinen Quellenfehler verdecken.
- Nur der aktuelle Quellbestand wird ausgegeben. Artikel werden durch Abrufe nicht verbraucht. Eine Identität erscheint innerhalb eines Feeds nur einmal; bei mehrfacher Nennung gewinnt der letzte Eintrag an der Position der ersten Nennung.
- Artikelupdates ändern Titel, Text, Links und Medien, nicht die Proxy-GUID oder das ursprüngliche Veröffentlichungsdatum. Ein zunächst fehlendes Veröffentlichungsdatum wird übernommen, sobald die Quelle erstmals eines liefert. Inhaltsänderungen erhalten einen getrennten, gespeicherten Änderungszeitpunkt (`atom:updated` in der RSS-Ausgabe). Ein erneuter Abruf unveränderter Artikel setzt diesen nicht erneut auf „jetzt“.
- Identitäten bleiben für die Lebensdauer der Feed-Konfiguration erhalten, auch wenn Artikel aus der Quelle verschwinden. Vollständige Artikel außerhalb des aktuellen Bestands werden nicht archiviert.
- Quellenfehler werden an den Reader weitergegeben, niemals durch einen alten Feed als erfolgreiche Antwort ersetzt. Gespeicherte Daten bleiben bei Fehlern erhalten.
- RSS, Atom und JSON Feed können eingelesen werden. Die Ausgabe ist RSS 2.0.

## Schnellstart mit Docker Compose

```sh
cp .env.example .env
# .env bearbeiten und beide Passwörter durch zufällige Werte ersetzen
docker compose up --build -d
```

Verwaltung: **http://localhost:8080/admin/**. Benutzer und Passwort stehen in `.env`. Dort einen Feed mit Quell-URL und öffentlichem Endpunkt anlegen, beispielsweise `/feeds/faz`. Feed-Endpunkte sind absichtlich **ohne Anmeldung** erreichbar.

Compose veröffentlicht den HTTP-Port nur auf `127.0.0.1`. Für Netzwerkzugriff einen HTTPS-Reverse-Proxy vorschalten. In diesem Fall `COOKIE_SECURE=true` setzen. Basic Auth ohne HTTPS ist nur für lokalen Entwicklungsbetrieb vorgesehen. Für externe Datenbanken TLS mit Zertifikatsprüfung verwenden.

**Wichtig:** Reverse-Proxy-Caches für Feed-Endpunkte deaktivieren. Sonst können Reader-Abrufe den Service und damit die Quelle umgehen.

## Lokal ohne Docker

Go 1.27 und eine laufende PostgreSQL-Datenbank werden benötigt:

```sh
export DATABASE_URL='postgres://rssproxy:password@localhost:5432/rssproxy?sslmode=disable'
export AUTH_USER=admin
export AUTH_PASSWORD='a-long-random-password'
go run ./cmd/rssproxy
```

Die Anwendung liest `.env` nicht selbst ein; Docker Compose verwendet die Datei für seine Variablenersetzung. Native Starts benötigen exportierte Umgebungsvariablen.

Versionierte SQL-Migrationen werden beim Start unter einer DB-Sperre ausgeführt. Der Datenbankbenutzer benötigt DDL-Rechte für das Anwendungsschema. Vor produktiven Migrationen ein Backup erstellen. Tabellen tragen das Präfix `rssproxy_`; die alten Tabellen werden nicht verändert.

### Server-Konfiguration

| Variable | Standard / Bedeutung |
|---|---|
| `DATABASE_URL` | Erforderlich; PostgreSQL-URL oder pgx/libpq-Verbindungsparameter |
| `AUTH_USER`, `AUTH_PASSWORD` | Erforderlich; Zugang zur Verwaltung |
| `LISTEN_ADDR` | `:8080` |
| `COOKIE_SECURE` | `false` für lokalen HTTP-Betrieb; bei HTTPS `true` |
| `SOURCE_TIMEOUT` | `20s` einschließlich Redirects und Response-Body |
| `REQUEST_TIMEOUT` | `60s` einschließlich Warten auf Feed-Sperre und DB-Verarbeitung |
| `MAX_FEED_BYTES` | `10485760` dekomprimierte Bytes |
| `ALLOWED_NETWORKS` | Optionale, kommagetrennte CIDR-Freigaben für interne Quellen |
| `LEGACY_DATABASE_URL` | Optionale separate goRssDedup-Datenbank für den Erstimport |

Für `DATABASE_URL`, `LEGACY_DATABASE_URL`, `AUTH_USER` und `AUTH_PASSWORD` gibt es alternativ `<NAME>_FILE` zum Lesen aus Secret-Dateien. Nicht gleichzeitig Wert und Datei angeben. pgx unterstützt außerdem seine üblichen `PG*`-Umgebungsvariablen; Compose verwendet `PGPASSWORD`, damit Sonderzeichen im Passwort nicht in eine URL interpoliert werden müssen.

## Stabile Artikelidentitäten

Interner Feed-Name, Quell-GUID, Anbieter-ID und öffentliche Proxy-GUID sind getrennt. Der Reader bekommt eine dauerhafte UUID-URN mit `isPermaLink="false"`.

Pro Feed stehen vier Strategien zur Auswahl:

1. **Vollständiger Wert:** Quell-GUID oder Artikel-Link unverändert als Schlüssel verwenden.
2. **Regex:** Benannte Capture-Gruppe `(?P<id>...)` aus GUID oder Artikel-Link extrahieren. Bei HTTP(S)-URLs wird nur der Pfad geprüft, nicht Host oder Query.
3. **Query:** Einen einzelnen URL-Parameter auslesen, beispielsweise `articleId`.
4. **Legacy:** Alte Byte-Kürzungsregel für den Import.

Optionale erlaubte Artikel-Hosts werden separat geprüft. `isPermaLink=true` garantiert keine unveränderliche URL und umgeht die Extraktion deshalb nicht. Nicht passende Regeln oder fehlende Identitätswerte führen zu einem sichtbaren Fehler, **nicht** zum Fallback auf eine wechselnde GUID.

### Beispiele

**FAZ**, Quelle GUID oder Artikel-Link, Regex:

```regex
-(?P<id>[0-9]+)\.html$
```

Aus `https://www.faz.net/aktuell/sport/alte-ueberschrift-accg-201686703.html` wird `201686703`. Erlaubte Hosts beispielsweise `www.faz.net faz.net`.

**Handelsblatt**, Regex:

```regex
/(?P<id>[0-9]+)\.html$
```

Aus `https://www.handelsblatt.com/finanzen/neue-ueberschrift/100270206.html` wird `100270206`. Erlaubte Hosts beispielsweise `www.handelsblatt.com handelsblatt.com`.

In der Verwaltung lassen sich Regeln ohne Speicherung oder Netzwerkabruf prüfen: mit eigenen Beispielzeilen oder bis zu 50 Einträgen des letzten erfolgreichen Abrufs.

Bei einer Regeländerung können bekannte Quell-GUIDs/Links die bestehende Proxy-Identität über historische Aliase verbinden. Mehrdeutige Zuordnungen führen zu einem Fehler. Eine beliebige Regeländerung kann Identitäten nicht zuverlässig rekonstruieren, wenn gleichzeitig sämtliche bekannten Quellwerte wechseln. Solche Änderungen vorher mit realen Feed-Beispielen prüfen.

## Feed- und Bild-Ersetzungen

Konfigurierbar sind Feed-Titel, Beschreibung, Website-Link, Feed-Bild/Logo, Bildtitel, Bild-Link und ein festes Artikelbild.

Modi: **Original übernehmen**, **immer ersetzen**, **nur bei fehlendem Original ergänzen**, **entfernen**. Pflichtfelder von RSS können nicht entfernt werden. Werden notwendige Unterfelder eines optionalen RSS-Bildes entfernt, entfällt das gesamte Bild.

Artikelbild-Quelle: `auto`, `image`, `enclosure` (nur Bild-Anhänge) oder `namespace:element`, beispielsweise `media:content`, `media:thumbnail` oder `itunes:image`.

- Bilder werden über URLs referenziert, nicht heruntergeladen oder gehostet.
- Ersetzungen werden erst bei der Ausgabe angewandt; Originaldaten bleiben erhalten.
- Eine gespeicherte Ersetzung benötigt keinen zusätzlichen Quellabruf. Beim nächsten Reader-Abruf wird die Quelle wie immer kontaktiert und die neue Ersetzung auch nach einem Quell-`304` angewandt.
- Audio-/Video-Enclosures bleiben unabhängig von Artikelbild-Ersetzungen erhalten.
- RSS-Enclosure-Längen sind Byteangaben, niemals Bildbreiten. Fehlende Längen werden als `0` (unbekannt) ausgegeben.
- XML-Erweiterungen, beispielsweise iTunes, Media RSS und Podcast Namespace, werden semantisch mit Namespaces, Attributen und Kindreihenfolge übernommen. Präfixe und XML-Schreibweise können sich ändern. Kein verlustfreier Format-Roundtrip: normalisierte RSS/Atom/JSON-Kernfelder werden auf RSS 2.0 abgebildet, Quell-Self-/Pagination-Links nicht übernommen.
- Manche Reader bevorzugen eigene Favicons oder cachen Bilder unabhängig vom Feed.

Feeds ohne RSS-Pflichtmetadaten benötigen passende Ersetzungen. Fehlerhafte Einträge führen zum Fehler des gesamten Abrufs, statt Nachrichten stillschweigend zu verlieren.

## Import aus goRssDedup

Beim Start prüft RSSProxy automatisch, ob in der Zieldatenbank `rssDedupConfig` existiert. Für eine andere alte Datenbank `LEGACY_DATABASE_URL` beziehungsweise `LEGACY_DATABASE_URL_FILE` setzen. Empfehlenswert ist dort ein Benutzer mit ausschließlich Leserechten.

Importiert werden interne Namen, öffentliche Endpunkte, Quell-URLs, Bild-Einstellungen und Kürzungsregeln. Die alte Quelle wird ausschließlich innerhalb einer Read-only-Transaktion gelesen. Bestehende Ziel-Endpunkte werden nicht überschrieben. Ein Importmarker verhindert Wiederholungen und das Wiederanlegen später absichtlich gelöschter Konfigurationen.

Der alte Artikelbild-Sonderwert `NoImage` wird als „Artikelbild entfernen“ übernommen. Leerzeichen am Anfang und Ende der Bildquelle werden beim Import entfernt.

Ungültige Konfigurationen lassen den Import atomar und sichtbar fehlschlagen. Ohne alte Tabelle startet eine neue Installation leer. Ist eine separate alte Datenbank explizit konfiguriert, aber nicht erreichbar oder ohne erwartete Tabelle, bricht der Start mit einem Fehler ab.

Importierte Kürzungsregeln in der Vorschau prüfen. „End“ verwendet wie bisher einen GUID-Endausschnitt mit der angegebenen Offset-Länge. „Start“ ist jetzt funktionsfähig und zählt den Offset vom Anfang. Beide Varianten sind gegen ungültige Grenzen abgesichert.

**Die alte Tabelle der bereits verbrauchten Artikel-IDs wird nicht importiert.** Sie enthält keine vollständigen ursprünglichen GUIDs oder Artikel. Durch die neuen Proxy-GUIDs können bekannte Artikel beim Umstieg einmalig erneut im Reader erscheinen.

## Fehler und Sicherheit

| Situation | Reader-Antwort |
|---|---|
| Quell-Timeout | `504` |
| Verbindung, geblocktes Ziel, ungültiger Feed oder Extraktionsfehler | `502` |
| Quell-HTTP-Fehler | Entsprechender Fehlerstatus, z.B. `429` oder `503` |
| Quell-`401` / `407` | `502`, damit keine Anmeldung am öffentlichen Proxy ausgelöst wird |
| Datenbank-/interner Fehler | `500` |
| Unbekannter Feed | `404` |

Gültiges `Retry-After` wird bei Quellenfehlern weitergereicht. Keine automatischen Wiederholungen oder versteckten Ersatzantworten.

SSRF-Schutz prüft DNS-Antworten und verbindet ausschließlich zu geprüften IP-Adressen. Private, lokale, Link-local-, Metadaten- und weitere Sondernetze sind standardmäßig gesperrt. Redirects unterliegen derselben Prüfung; HTTPS-Downgrades und Umgebungs-Proxies sind deaktiviert. Interne Feed-Ziele nur gezielt mit `ALLOWED_NETWORKS` freigeben. Ergänzend Netzwerk-Egress-Regeln einsetzen.

Die Verwaltung verwendet Constant-time-Prüfung gehashter Basic-Auth-Zugangsdaten, CSRF-Token, Cross-Origin-Prüfung, HTML-Escaping, CSP und begrenzte Formulardaten. Der Service hat HTTP-/DB-Deadlines und Größen-/XML-Strukturlimits. Vor öffentlicher Bereitstellung Login-Rate-Limits am Reverse Proxy konfigurieren.

Status wird ohne Zugangsdaten strukturiert protokolliert. Quell-URLs mit geheimen Query-Parametern sind für Administratoren in der Konfiguration sichtbar und sollten entsprechend geschützt werden.

## Betrieb

- `GET /healthz`: Prozess-Liveness.
- `GET /readyz`: Datenbank erreichbar.
- `rssproxy healthcheck`: Readiness-Prüfung ohne zusätzliche Programme, auch im Scratch-Container.
- `rssproxy version`: Build-Version.
- `SIGTERM` / `SIGINT`: laufende Requests bis zum Shutdown-Limit abschließen.

Bei hoher Last Wartezeiten beachten: Für denselben Feed werden unabhängige Quellabrufe seriell abgearbeitet; nach `REQUEST_TIMEOUT` kann ein wartender Request abbrechen. Quellfrequenz und Bandbreite entsprechen damit bewusst der Reader-Nutzung. Große Feeds und lange Identitätshistorien benötigen passende PostgreSQL-Kapazität und Backups.

## GitHub Actions, Sicherheit und Container-Releases

Der Workflow [`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) orientiert sich an [CupNotifyer](https://github.com/KevinCFechtel/CupNotifyer). Er läuft bei Pull Requests, Pushes auf `main`, `v*`-Tags, manuellen Starts und wöchentlich montags. Actions sind auf Commit-SHAs festgelegt; Berechtigungen sind pro Job auf das Nötige beschränkt.

**Bei normalen Änderungen und Pull Requests:**

- Formatierung, Race-Tests einschließlich echter PostgreSQL-Integrationstests, Coverage, `go vet`, Binary-Build und Fuzzing.
- Prüfung erreichbarer Go-Sicherheitslücken mit `govulncheck`, CodeQL und Dependency Review bei Pull Requests; Dependency Review blockiert neue Schwachstellen ab `moderate`.
- Native Container-Builds **und Ausführung** auf `linux/amd64` sowie `linux/arm64`. Anders als CupNotifyer prüft RSSProxy die Container schon vor einem Release, um die fehlende lokale Docker-Umgebung auszugleichen.
- Die CI startet das tatsächlich gebaute Scratch-Image, PostgreSQL und eine kontrollierte Feed-Quelle über Compose. Geprüft werden Non-root-/Read-only-Betrieb, Build-Version, Healthcheck, Anmeldung/CSRF, echte RSS-Abrufe, Bild-Ersetzungen, Podcast-Metadaten, Artikelupdates mit stabiler GUID, `304`, Quellenfehler und parallele Abrufe ohne Zusammenlegung. Nach einem Neustart müssen Konfiguration und Identität erhalten bleiben.
- Ein SHA256-gepinnter Trivy-Container scannt das AMD64-Image auf `HIGH`-/`CRITICAL`-Schwachstellen. ARM64 wird nativ gebaut und funktional getestet; der aus CupNotifyer übernommene Scanner-Digest ist ein einzelnes Plattform-Image.
- Coverage und Container-Diagnosen stehen 14 Tage als Workflow-Artefakte bereit. Vorhandene Trivy-Ergebnisse werden außerdem nach GitHub Security hochgeladen, sofern der PR-Kontext dies erlaubt.

Normale Commits und Pull Requests **veröffentlichen keine Images**. Der Container-Test verwendet ausschließlich eigene Test-Zugangsdaten, einen isolierten Compose-Projektnamen und temporäre Volumes. Eine lokale Docker-Installation ist für diese Prüfungen nicht erforderlich; ausgeführt werden sie auf GitHub-Runnern nach dem Push.

**Bei einem gültigen Release-Tag, beispielsweise `v1.2.3`:**

Nach erfolgreichen Go-/Sicherheitsprüfungen und Container-Tests beider Architekturen wird ein Multi-Plattform-Image mit SBOM und Provenance nach `ghcr.io/kevincfechtel/rssproxy` veröffentlicht. Als Build-Version wird der Tag in das Binary eingebettet.

- Stabile Releases erhalten Versionstags mit und ohne `v`, einen `major.minor`-Alias, einen Commit-SHA-Tag und `latest`.
- Prereleases wie `v1.3.0-rc.1` erhalten eigene Versions-/SHA-Tags, überschreiben aber **nicht** `latest` oder den stabilen `major.minor`-Alias.
- Zum Veröffentlichen reicht der automatisch bereitgestellte `GITHUB_TOKEN`; kein Docker-Hub-Passwort ist nötig.

```sh
docker pull ghcr.io/kevincfechtel/rssproxy:latest
```

Die Workflow-Dateien müssen zuerst committed und nach GitHub gepusht werden. Bei einem neu angelegten GHCR-Paket dessen Sichtbarkeit prüfen, bevor ein Pull ohne Anmeldung erwartet wird. CodeQL und Dependency Review setzen die entsprechenden GitHub-Funktionen voraus; das aktuelle RSSProxy-Repository ist öffentlich. Dependabot aktualisiert Go-Abhängigkeiten, Docker-Basisimages und Actions wöchentlich.

## Entwicklung und Tests

```sh
make build
go test -race ./...
go vet ./...
govulncheck ./...
```

Für die PostgreSQL-/HTTP-Integrationstests eine **Testdatenbank** bereitstellen:

```sh
export TEST_DATABASE_URL='postgres://rssproxy:password@localhost:5432/rssproxy_test?sslmode=disable'
go test -race -count=1 ./...
```

Jeder Integrationstest erstellt ein isoliertes, zufällig benanntes Schema und entfernt nur dieses Schema anschließend wieder. Ohne `TEST_DATABASE_URL` werden diese Tests ausdrücklich übersprungen. CI führt sie mit PostgreSQL aus, zusätzlich zu Build, Vet, Fuzzing und Vulnerability Check.

Die CI-Helfer für Release-Tags und die kontrollierte Feed-Quelle lassen sich unabhängig von Docker testen:

```sh
python3 -m unittest discover -s .github/scripts -p 'test_*.py' -v
```

```sh
go test ./internal/feed -run '^$' -fuzz '^FuzzIdentityNeverPanics$' -fuzztime=30s
go test ./internal/feed -run '^$' -fuzz '^FuzzParseNeverPanics$' -fuzztime=30s
```

Projektstruktur:

```text
cmd/rssproxy       Programmstart und Shutdown
internal/app       Server-Konfiguration, Routing, End-to-End-Tests
internal/feed      Identitätsregeln, Parser, RSS-Ausgabe und Ersetzungen
internal/fetch     HTTP-Abruf und SSRF-Schutz
internal/proxy     Reader-Request-Verarbeitung
internal/store     PostgreSQL, Migrationen, Identitäten und Altimport
internal/web       Servergerenderte Verwaltung
```

Lizenz: [BSD 3-Clause](LICENSE).
