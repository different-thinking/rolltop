# Code-Review & Umsetzungsplan — Rolltop

**Stand:** 2026-08-25
**Umfang:** vollständiges Repository — Go-Backend (~148.000 LOC), React/TypeScript-Frontend (~24.000 LOC), Plugin-System (~30.000 LOC), Build/CI/Deployment.
**Methode:** ausführlicher Diff-Review der letzten ~110 Commits plus sechs subsystemweise Tiefen-Reviews (Store/Suche, Sync/IMAP/SMTP, Web/Security, Frontend, Plugins, Infra/Build/Tests). Die als kritisch eingestuften Funde wurden zusätzlich manuell am Code nachverifiziert.

---

## 1. Gesamtbild

Rolltop ist überdurchschnittlich sorgfältig gebaut. Die tragenden Sicherheits- und Korrektheits-Invarianten sind konsequent umgesetzt und gut dokumentiert:

- **Tenant-Isolation:** `user_id`-Scoping zieht sich durch praktisch jede Query; keine SQL-Injection gefunden (alle konkatenierten Identifier sind Konstanten oder laufen durch `sqlident.Quote`), keine IDOR-Lücken in den geprüften Handlern.
- **Web-Sicherheit:** CSRF flächendeckend (HMAC über Session-Cookie), OAuth per PKCE mit single-use-`state`, Mail-Bodies in sandboxed iframes **ohne** `allow-scripts` plus Per-Dokument-CSP — XSS aus Mail-Inhalt ist mehrschichtig abgefangen.
- **Krypto:** AES-GCM mit Zufallsnonce, Argon2id, timing-sichere Vergleiche.
- **Protokoll-/Sync-Schicht:** UIDVALIDITY vor jeder mutierenden Operation bewiesen, crash-sichere Transfer-Claims, solide Backoff-Logik ohne Hot-Loops; die smtplog-Redaction hält ihr Versprechen (AUTH/SASL/DATA erreichen den Mitschnitt nachweislich nie).

Die Schwächen liegen an den Rändern und sind zu großen Teilen **Altlasten der SQLite→PostgreSQL-Migration** sowie **Lücken im Plugin-Grenzmodell**. Die gravierendsten sind:

1. Ein zentrales Datenschutz-Feature (Remote-Image-Blocklist) ist **derzeit vollständig wirkungslos** und meldet das nirgends.
2. Zwei am Code nachgemessene **DoS-Vektoren im MIME-Parser**, die den Sync eines Tenants dauerhaft blockieren können.
3. Ein **Datenverlust-Race** im Blob-Cleanup, dessen Sicherheitsargument nur unter SQLite galt.
4. **Fehlende FK-Indizes**, die Massenoperationen (Papierkorb leeren) auf großen Beständen quadratisch machen.
5. Ein **Autocrypt-MITM**, bei dem eine gespoofte Mail den Verschlüsselungs-Key eines Kontakts kapert.

Dazu strukturelle Prozesslücken: das Frontend (24.000 LOC) hat **null Tests**, 10 von 13 Plugin-Frontends werden **nicht typgeprüft**, und CI baut bei reinen Host-Frontend-Änderungen die Plugin-Bundles nicht.

**Findings gesamt:** 49 (7 Hoch, 20 Mittel, 22 Niedrig), verteilt über sechs Subsysteme.

---

## 2. Findings nach Subsystem

Schweregrade: **Kritisch/Hoch** = zeitnah beheben; **Mittel** = eingeplant beheben; **Niedrig** = Härtung/Aufräumen.
Aufwand: **S** < 2h · **M** ~1 Tag · **L** mehrere Tage.

### 2.1 Store & Suche (PostgreSQL)

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| ST-1 | Hoch | Bug | Blob-Cleanup-Recheck unter PostgreSQL nicht mehr exklusiv → Datei kann unter lebender Referenz gelöscht werden | `store/blob_cleanup_queue.go:133`, `store/mailbox_generation_blob_cleanup.go:83` | M |
| ST-2 | Hoch | Perf | FK-Spalten ohne führenden Index → Massen-Deletes quadratisch | `store/pgschema/baseline.sql` (FK-Defs), `store/messages.go:462` | S |
| ST-3 | Mittel | Perf | `ListMessagesByIDsForUser` ist N+1 und lädt volle Message-Bodies | `store/message_lists.go:780` | S |
| ST-4 | Mittel | Perf | Globale Wartungsqueries scannen die gesamte `messages`-Tabelle pro Lauf | `store/message_body.go:82,142` | S |
| ST-5 | Mittel | Robustheit | `UpsertMessageSearch` kann per Konflikt eine fremde Tenant-Zeile überschreiben | `store/message_search.go:85` | S |
| ST-6 | Niedrig | Bug | Thread-Sortierschlüssel bricht bei negativem `date_unix` | `store/message_lists.go:575` | S |
| ST-7 | Niedrig | Robustheit | `ReplaceAttachmentsForMessage` liest Bestand außerhalb der Transaktion | `store/blobs.go:309` | S |
| ST-8 | Niedrig | Robustheit | Mailbox-Rollen-Eindeutigkeit nur per Read-then-Write, kein Unique-Index | `store/mailboxes.go:243,590` | S |

**ST-1 (Detail):** Beide Cleanup-Funktionen begründen ihre Sicherheit mit „SQLite's writer lock". Unter PostgreSQL (READ COMMITTED) sperrt die Transaktion nur die Queue-Zeile; der Referenz-Recheck blockiert kein paralleles `CreateBlob`/`CreateMessage`, und `deletePath` (Dateisystem, nicht transaktional) läuft **vor** dem `DELETE FROM blobs`. Ein Generation-Rebuild, der dieselbe `.eml` neu schreibt, während der Cleanup einen alten Queue-Eintrag drainiert, kann die soeben neu geschriebene Datei löschen → Message-Zeile mit `blob_path` auf nicht-existente Datei, Body wird nie nachgeladen. **Fix:** `blobs`-Zeile in der Transaktion mit `SELECT … FOR UPDATE` sperren; Datei erst **nach** erfolgreichem `DELETE FROM blobs` + Commit löschen; SQLite-Kommentare korrigieren.

**ST-2 (Detail):** 12 Tabellen referenzieren `messages(id)` per `ON DELETE CASCADE/SET NULL`, keine hat einen Index mit führendem `message_id`; ebenso fehlt ein Index auf `messages(blob_id)`/`attachments(blob_id)`. Papierkorb mit 50.000 Nachrichten leeren = pro gelöschter Zeile ~10 index-lose Scans über teils millionenzeilige Tabellen. **Fix:** eine Migration `000x-fk-indexes` mit Indizes auf allen FK-Spalten.

### 2.2 Sync / IMAP / SMTP / Mail-Parser

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| SY-1 | Hoch | Robustheit | Quadratische Laufzeit im CSS-Stripper → präparierte Mail blockiert Sync dauerhaft | `mailparse/parser.go:518` | S |
| SY-2 | Hoch | Robustheit | Unbegrenzte MIME-Schachteltiefe → ~100-fache Speicherverstärkung, OOM-Crash-Schleife | `mailparse/parser.go:186,287` | S |
| SY-3 | Mittel | Bug | `ListMailboxes` leakt bei Abbruch dauerhaft die go-imap-Reader-Goroutine | `imapclient/fetcher.go:190` | S |
| SY-4 | Mittel | Bug | Copy-Settlement läuft auf abbrechbarem Context → abgebrochener Request sperrt Nachricht 10 min | `syncer/copy.go:318` | S |
| SY-5 | Niedrig | Robustheit | Paralleles Expunge während Batch-Fetch lässt ganzen Turn scheitern → unnötiger Account-Backoff | `imapclient/fetcher.go:1006` | M |
| SY-6 | Niedrig | Security | XOAUTH2 für IMAP ohne Cleartext-Schutz (SMTP-Seite hat ihn) | `xoauth2/xoauth2.go:56` | S |
| SY-7 | Niedrig | Bug | IMAP-Fetch-Stall wird als `Other` statt `Transient` klassifiziert (widerspricht eigener Doku) | `syncer/remote_errors.go:44` | S |

**SY-1 (verifiziert, nachgemessen):** `indexedCSSRuleClose` scannt pro öffnender Klammer bis zum balancierten Schluss; bei `{{{…}}}` O(n²). Messung: 20k Paare = 1,2s, 40k = 5,0s, 80k = 19,4s. `mailparse.Parse` läuft synchron im Sync-Turn ohne Context-Check; der UID-Checkpoint rückt nie über die Nachricht hinaus → permanenter CPU-Brand + Sync-Stillstand des Ordners. **Fix:** Klammerpaare in einem O(n)-Vorlauf per Stack matchen; zusätzlich Präfix-/Tiefen-Cap.

**SY-2 (verifiziert):** Pro Schachtelebene ein `multipart.Reader` mit eigenem Puffer, alle Ebenen gleichzeitig referenziert. 15-MB-Mail mit 300.000 Ebenen = ~1,5 GB Live-Heap; `ROLLTOP_MEMORY_LIMIT` ist nur GC-Soft-Limit → OOM-Kill, nach Neustart erneuter Parse → Crash-Schleife. **Fix:** Tiefenzähler (Limit ~50) durch `parsePart`/`parseDisplayPart` reichen.

### 2.3 Web-Schicht & Sicherheit

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| WEB-1 | Mittel | Security | Kein Rate-Limiting/Bruteforce-Schutz bei Login & Passwort-Reset | `web/api_auth.go:273`, `web/api_password_reset.go:21` | M |
| WEB-2 | Mittel→Hoch | Security | Passwort-Reset-Link aus angreiferkontrollierbarem `Host`-Header | `web/api_password_reset.go:85` | S |
| WEB-3 | Mittel | Security | Session-Cookie ohne `Secure`-Default, kein HSTS | `config/config.go:169`, `web/server.go:1141,503` | S |
| WEB-4 | Niedrig | Security | Benutzer-Enumeration über Antwortzeit beim Login | `web/api_auth.go:297` | S |
| WEB-5 | Niedrig | Security | SSRF-Blockliste des Remote-Bild-Proxys mit Lücken (CGNAT `100.64/10` etc.) | `remoteimages/remoteimages.go:282` | S |
| WEB-6 | Niedrig | Security | Fehlende Clickjacking-Absicherung (`frame-ancestors`/`X-Frame-Options`) | `web/server.go:503` | S |
| WEB-7 | Niedrig | Security | App-CSP erlaubt `script-src 'unsafe-inline'` | `web/server.go:508` | M |
| WEB-8 | Niedrig | Security | Webhook-Token auch über URL-Query akzeptiert (landet in Logs) | `web/server.go:632` | S |
| WEB-9 | Niedrig | Security | Admin-Passwortänderung invalidiert bestehende Sessions nicht | `web/api_admin.go:109`, `store/users.go:123` | S |
| WEB-10 | Niedrig | Robustheit | Regex-basierte Sanitisierung der Mail-Bodies (fragil, hängt an iframe/CSP) | `web/email_document.go:29` | M |

**WEB-2 (Detail):** Der Reset-Link wird aus `r.Host` + `X-Forwarded-Proto` gebaut, ohne Allowlist — an anderer Stelle (`googleauth/config.go`) wird headerbasierte Origin-Ableitung bewusst abgelehnt, hier nicht. Angreifer stößt Reset fürs Opfer an, setzt `Host: attacker.example`; die echte Backup-Adresse erhält einen Link auf die Angreifer-Domain samt gültigem Token → Kontoübernahme. Steigt auf **Hoch**, wenn ein Reverse-Proxy den Client-`Host` durchreicht. **Fix:** Link aus konfigurierter Basis-URL (`ROLLTOP_PUBLIC_URL`) erzeugen.

### 2.4 Frontend

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| FE-1 | Hoch | Perf | Autocrypt-Probe lädt Roh-Quelle **jeder** Thread-Nachricht bei jedem Öffnen | `features/mail/ThreadView.tsx:1045,1214` | M |
| FE-2 | Mittel | Security | `<meta http-equiv="refresh">` überlebt Sanitization, umgeht Remote-Content-Sperre | `features/mail/ThreadView.tsx:2598`, `web/email_document.go:44` | S |
| FE-3 | Mittel | Security | Compose-Recovery schreibt entschlüsselten PGP-Klartext unverschlüsselt in localStorage | `features/compose/ComposeViews.tsx:396`, `lib/composeLocal.ts:61` | S |
| FE-4 | Mittel | Bug | SSE-Stream wird bei jedem Bootstrap-Refresh abgerissen und neu aufgebaut | `App.tsx:679,608` | S |
| FE-5 | Mittel | Perf | `getCache` in `api.ts` wächst unbegrenzt und überlebt Logout | `api.ts:101,123` | M |
| FE-6 | Niedrig | Robustheit | Kein 401-Handling: abgelaufene Session strandet in Fehler-Toasts | `api.ts:68`, `features/mail/MailViews.tsx:387` | S |
| FE-7 | Niedrig | Robustheit | `logout()` ohne Fehlerbehandlung → Zustand bei Netzfehler inkonsistent | `App.tsx:816` | S |
| FE-8 | Niedrig | Bug | EmailFrame-Höhe nach 1,6s eingefroren → spät ladende Bilder abgeschnitten | `features/mail/ThreadView.tsx:2579` | S |

**FE-1 (Detail):** Bei aktivem PGP-Plugin ruft der Effekt für **jede** Nachricht `api.messageOriginal()` (komplette RFC822-Quelle inkl. Base64-Attachments, kein ETag-Cache) ohne Gate auf `is_signed`/`is_encrypted`; `load()` verwirft die Ergebnisse, sodass jeder Reload alles erneut lädt. 15 Nachrichten × 5 MB = 75 MB pro Öffnen, nach Reply nochmal. **Fix:** Backend-Flag `has_autocrypt_header` pro `ThreadMessage`, Probe darauf gaten, Ergebnisse pro `message.id` cachen.

### 2.5 Plugin-System

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| PL-1 | Hoch | Bug | Remote-Image-Blocklist komplett wirkungslos (Fail-open) — Interface-Methode fehlt | `plugins/remote_image_blocklist/backend/main.go:17`, `web/compiled_plugin_hooks.go:27` | S |
| PL-2 | Hoch | Security | Autocrypt-Import: gespoofte Mail ersetzt bevorzugten Verschlüsselungs-Key eines Kontakts | `plugins/client_side_pgp/backend/autocrypt/hooks.go:34` | M |
| PL-3 | Mittel | Security | OIDC: fehlendes `email_verified` gilt als verifiziert; Userinfo-Fallback prüft es gar nicht | `plugins/oidc/backend/main.go:179,454` | S |
| PL-4 | Mittel | Robustheit | Plugin-Hook-Fehler und -Panics reißen Mail-Sync bzw. ganzen Prozess mit | `syncer/autocrypt.go:14`, `syncer/index.go:155` | M |
| PL-5 | Mittel | Bug | `client_side_pgp`: Substring-Heuristik erklärt normale Mails für verschlüsselt → Attachments verschwinden | `plugins/client_side_pgp/backend/security/security.go:55` | M |
| PL-6 | Mittel | Bug | `mail_mcp`: Suche macht Pagination zur Endlosschleife, sieht nur neueste 200 Mails | `plugins/mail_mcp/backend/main.go:849` | M |
| PL-7 | Mittel | Wartbarkeit | `mail_filters`: Scope-SQL dupliziert Host-Logik, Loop-Erkennung hängt an Fehlertext | `plugins/mail_filters/backend/main.go:58,707` | S |
| PL-8 | Niedrig | Security | `mail_mcp`: Consent-Token nicht an Session gebunden, Refresh-Tokens nie rotiert | `plugins/mail_mcp/backend/main.go:1061,380` | S |
| PL-9 | Niedrig | Robustheit | Blocklist-Regexes: ungültige Muster still übersprungen, Kompilierung pro Bild-Fetch | `plugins/remote_image_blocklist/backend/main.go:39`, `rules/rules.go:104` | S |
| PL-10 | Niedrig | Security | `X-Forwarded-Host`/`-Proto` ungeprüft geglaubt — in zwei driftenden Kopien | `plugins/mail_mcp/backend/main.go:1305`, `plugins/oidc/backend/main.go:570` | S |
| PL-11 | Niedrig | Wartbarkeit | Gravatar-Plugin funktioniert nur, wenn Attachment-Preview-Plugin einkompiliert ist | `web/plugin_routes.go:329` | S |
| PL-12 | Mittel | Bug | `mail_filters`: entferntes `star`-Feld macht bestehende Star-only-Regeln kommentarlos zu No-Ops | `plugins/mail_filters/backend/main.go:344` | S |

**PL-1 (verifiziert):** Das Interface `RemoteImageBlocklistHook` verlangt 5 Methoden; `remoteImageBlocklistHook{}` implementiert nur 4 (`SeedRemoteImageRules` fehlt), es gibt keine Compile-Zeit-Assertion, und die Registrierung läuft über `any` — also schlägt erst die Laufzeit-Type-Assertion in `web/compiled_plugin_hooks.go:27` und `syncer/remote_images.go:62` fehl. Folge: `AllowRemoteImageFetch` wird nie aufgerufen, jede Tracking-Pixel-URL bekommt `Allow: true`, die Admin-Seite liefert PUT 404 — alles ohne eine Log-Zeile. **Fix:** `SeedRemoteImageRules` implementieren **und** für alle kompilierten Hooks Compile-Zeit-Assertions `var _ plugins.X = xHook{}` ergänzen (Muster existiert in `experimental_spam_filter`), damit Interface-Drift zum Buildfehler statt zu Fail-open wird; Host loggen lassen, wenn ein aktiviertes Plugin keinen passenden Hook liefert.

**PL-2 (Detail):** `ImportIncomingMessage` speichert für jede gespiegelte Mail (auch Junk) den Autocrypt-Header-Key mit `IsPreferred: true`, sobald `addr` == From-Header — ohne DKIM-/Authentizitätsprüfung; `UpsertContactPublicKey` entthront dabei alle anderen Keys, auch manuell importierte. Gespoofte `From:`-Mail mit Angreifer-Key → ausgehende „verschlüsselte" Mail an den Kontakt ist für den Angreifer lesbar. **Fix:** manuelle Keys nie demoten, Autocrypt-Peer-State-Regeln umsetzen (nur ein Header, nur neuere Zustände, Junk ausnehmen).

### 2.6 Übergreifende Wartbarkeit (aus dem Diff-Review)

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| GEN-1 | Mittel | Bug | `matchAttachmentRows` Pass 3 (nur Name) kann gleichnamige Attachments unterschiedlicher Größe vertauschen → Download liefert falsche Datei | `store/blobs.go:393` | S |
| GEN-2 | Niedrig | Wartbarkeit | `Server.ArchiveMailboxID` dupliziert `syncPluginHost.ArchiveMailboxID` line-for-line | `web/backend_plugins.go:227`, `syncer/autocrypt.go:143` | S |
| GEN-3 | Niedrig | Wartbarkeit | Manifest-Dateilisten-Logik zwischen `build-plugins.mjs` und `assemble-plugin-dist.mjs` dupliziert | `scripts/build-plugins.mjs:173`, `scripts/assemble-plugin-dist.mjs` | S |

### 2.7 Build, CI, Tests, Deployment

| ID | Schwere | Kat. | Titel | Ort | Aufw. |
|----|---------|------|-------|-----|-------|
| CI-1 | Hoch | CI | Host-Frontend-Änderungen bauen die Plugin-Bundles nicht (Bruch fällt erst beim Release-Tag auf) | `.github/workflows/pr.yml:113`, `ci.yml` | S |
| CI-2 | Hoch | Tests | Frontend: 24.322 LOC, null Tests, kein Test-Runner | `frontend/src/`, `package.json` | L (Setup S) |
| CI-3 | Hoch | CI | 10 von 13 Plugin-Frontends werden nirgends typgeprüft | `tsconfig.json:19`, `vite.plugins.config.ts:48` | S |
| CI-4 | Mittel | Bug | 14 bekannte `go vet`-Findings (echte copylocks), Gate deshalb deaktiviert | `pr.yml:183`, `plugins/client_side_pgp/backend/main.go:25` | S–M |
| CI-5 | Mittel | Tests | OIDC-Sign-in: 617 LOC handgerollte Krypto ohne einen Test | `plugins/oidc/backend/main.go`, `backend/googletoken` | M |
| CI-6 | Mittel | Build | Kein Healthcheck für den App-Container | `Dockerfile`, `compose.yml:43` | S |
| CI-7 | Mittel | CI | Doku widerspricht `verify`; Stale-Green-Merge-Loch nicht geschlossen | `ci.yml:96`, `AGENTS.md:681`, `README.md:871` | S |
| CI-8 | Niedrig | Wartbarkeit | Kern-Dependency `go-imap v1` im Maintenance-Modus (v2 aktiv) | `go.mod:7` | L (Bewertung S) |
| CI-9 | Niedrig | Security | `.env`/`.env.rolltop` fehlen in `.dockerignore` | `.dockerignore` | S |
| CI-10 | Niedrig | Security | `compose.dev.yml` bindet Superuser-Postgres auf alle Interfaces | `compose.dev.yml:26` | S |
| CI-11 | Niedrig | Build | Node-Drift: CI prüft mit Node 24, Image baut mit Node 20 | `Dockerfile:24`, `pr.yml:281` | S |
| CI-12 | Niedrig | CI | `gradle-version: 8.10.2` im Release-Job ist wirkungslos | `ci.yml:315` | S |

**Testlage (erhoben):** Go-Kern gut abgedeckt — 266 `*_test.go`, 40/65 Pakete mit Tests (`web` 67 Testdateien, `syncer` 59, `store` 46). Ungetestet u. a.: `plugins/oidc/backend` (617 LOC Auth-Krypto), `backend/googletoken`, `plugins/client_side_pgp/backend/{api,pgpmime}`. Frontend: **0 Tests, kein Runner**. `mailparse`: nur 3 Testdateien für einen sicherheitsrelevanten Parser (siehe SY-1/SY-2).

---

## 3. Umsetzungsplan

Fünf Phasen, nach Risiko/Aufwand-Verhältnis geordnet. Phase 0 ist bewusst klein gehalten (fast alles S-Aufwand, aber hohe Wirkung) und sollte zuerst und einzeln gemergt werden.

### Phase 0 — Sofortmaßnahmen (kritisch, kleiner Aufwand)
*Ziel: aktive Datenverlust-/DoS-/Datenschutz-Risiken und die schlimmste Prozesslücke schließen. Geschätzt ~2–3 Tage.*

1. **PL-1** Remote-Image-Blocklist reaktivieren: `SeedRemoteImageRules` implementieren **plus** Compile-Zeit-Assertions für **alle** kompilierten Hooks (verhindert künftige Fail-open-Drift). — *S*
2. **SY-1** CSS-Stripper auf O(n)-Klammer-Matching umstellen + Präfix/Tiefen-Cap. — *S*
3. **SY-2** MIME-Schachteltiefe begrenzen (Zähler durch `parsePart`/`parseDisplayPart`). — *S*
4. **ST-1** Blob-Cleanup: `FOR UPDATE` auf die `blobs`-Zeile, Dateilöschung nach Commit; SQLite-Kommentare korrigieren. — *M*
5. **ST-2** Migration `000x-fk-indexes` für alle `message_id`/`blob_id`-FKs. — *S*
6. **WEB-2** Passwort-Reset-Link aus konfigurierter Basis-URL statt `Host`-Header. — *S*
7. **PL-3** OIDC: `email_verified == true` verlangen (mit Opt-out-Env), Userinfo-Fallback einbeziehen. — *S*
8. **CI-1 + CI-3** `^frontend/` in den `plugin_frontend`-Filter aufnehmen; alle 13 Plugin-Frontends in `tsconfig`/Typecheck einbeziehen. — *S*

> Begleitend zu SY-1/SY-2: einen `mailparse`-Fuzz-/Tabellentest mit den Malicious-Payloads ergänzen (verhindert Regression).

### Phase 1 — Sicherheit härten
*Ziel: verbleibende Auth-/Web-/Plugin-Sicherheitslücken. Geschätzt ~1 Woche.*

- **PL-2** Autocrypt-Import gegen Spoofing absichern (manuelle Keys nie demoten, Peer-State-Regeln, Junk ausnehmen). — *M*
- **WEB-1** Rate-Limiting/Backoff für Login **und** `password-reset/request` (Muster `reserveSMTPTest` wiederverwenden). — *M*
- **WEB-3** `Secure`-Cookie als Default (bzw. auto bei TLS/`X-Forwarded-Proto=https`) + HSTS-Header. — *S*
- **WEB-9** Admin-Passwortänderung invalidiert alle Sessions des Nutzers. — *S*
- **FE-2** `<meta>` (mind. `http-equiv=refresh`) im Body-Sanitizer entfernen. — *S*
- **FE-3** Compose-Recovery für Replies auf verschlüsselte Mails überspringen bzw. verschlüsseln. — *S*
- **PL-8** Consent-Token an `UserID` binden; Refresh-Token bei Nutzung rotieren. — *S*
- **WEB-8** Webhook-Token nur per Header akzeptieren. — *S*
- **WEB-4** Dummy-Hash-Verify bei unbekanntem Nutzer (konstante Login-Zeit). — *S*
- **WEB-5** `privateIP` um CGNAT/IANA-Special-Purpose erweitern. — *S*
- **WEB-6** `frame-ancestors 'none'` / `X-Frame-Options: DENY`. — *S*
- **SY-6** XOAUTH2-IMAP: Cleartext-Schutz spiegelbildlich zur SMTP-Seite. — *S*
- **PL-10** Gemeinsame host-seitige `requestBaseURL`-Hilfe mit kanonischer Basis-URL für `mail_mcp`/`oidc`. — *S*

### Phase 2 — Datenintegrität & Korrektheit
*Ziel: stille Datenkorruption, hängende Zustände, Robustheit. Geschätzt ~1 Woche.*

- **PL-4** Gemeinsamer Dispatch-Wrapper mit `recover()` + Logging um jeden Plugin-Hook; advisory Hooks loggen statt Import zu failen. — *M*
- **PL-5** PGP-Erkennung aus geparster MIME-Struktur statt Substring-Heuristik. — *M*
- **PL-12** Migration/Hinweis für bestehende Star-only-Filterregeln. — *S*
- **PL-6** `mail_mcp`-Suche: Pagination korrigieren / an Host-Suchdienst delegieren. — *M*
- **GEN-1** `matchAttachmentRows`: Name-only-Pass durch positionalen Fallback ersetzen (Größe/Content-Type berücksichtigen). — *S*
- **SY-4** Copy-Settlement auf `context.WithoutCancel(ctx)` (wie Move-Pfad). — *S*
- **SY-3** `ListMailboxes`/`FetchMessage`: Kanal bei Abbruch async leeren (Goroutine-Leak). — *S*
- **ST-5** `UpsertMessageSearch`: Cross-Tenant-Guard im `DO UPDATE`. — *S*
- **ST-7 / ST-8** Attachment-Reparse in Transaktion + `FOR UPDATE`; partieller Unique-Index auf Mailbox-Rolle. — *S*
- **FE-4** SSE-/Push-Effekt-Deps auf `bootstrap?.user?.id`, `notifyNewMail` per Ref. — *S*
- **SY-7** Stall-Fehler als `Transient` klassifizieren. — *S*

### Phase 3 — Performance
*Ziel: spürbare Last-/Latenz-Verbesserungen. Geschätzt ~3–4 Tage.*

- **FE-1** Autocrypt-Probe per `has_autocrypt_header`-Flag gaten + Ergebnis-Cache. — *M*
- **ST-3** `ListMessagesByIDsForUser` auf gechunkte `IN`-Query + body-lose Projektion für den Move-Pfad. — *S*
- **ST-4** Partielle Indizes für die Wartungsqueries. — *S* (mit ST-2 kombinierbar)
- **FE-5** LRU-Cap für `getCache`; bei Nutzerwechsel nicht-gescopte Keys verwerfen. — *M*
- **SY-5** Fehlende UIDs bei unverändertem UIDVALIDITY als „vanished" überspringen. — *M*
- **FE-8** `ResizeObserver` statt Timeout-Kaskade für EmailFrame-Höhe. — *S*

### Phase 4 — CI, Tests, Infra, Aufräumen
*Ziel: die Prozesslücken schließen, die Regressionen entstehen lassen. Laufend / geschätzt ~1–2 Wochen.*

> **Status: umgesetzt.** Alle Punkte sind im Code umgesetzt. Zwei Restpunkte
> sind bewusst als Nicht-Code-Aufgaben dokumentiert statt erzwungen:
> **CI-7** — die „Require branches to be up to date"/Merge-Queue-Einstellung ist
> eine Repo-Admin-Aktion (in AGENTS.md/README dokumentiert; der Workflow kann sie
> nicht selbst setzen); **CI-8** — die go-imap-v2-Migration ist als Entscheidung
> plus Plan (`docs/go-imap-v2-migration.md`) eingelagert, bewusst zurückgestellt.
> **CI-2** startet wie geplant mit reinen Funktionen (Vitest-Runner + erste
> Suite); der Ausbau der Frontend-Abdeckung bleibt laufende Arbeit.

- **CI-2** Vitest einführen; mit den in AGENTS.md dokumentierten Invarianten (Dismissal-Lebenszyklus, `waitForChromeEvent`, Keepalive-Budgets) als reine Funktionen beginnen. — *L (Setup S)*
- **CI-4** 14 `go vet`-Findings fixen (Pointer-Receiver, `defer cancel`), dann `go vet ./...` als Pflicht-Gate. — *S–M*
- **CI-5** Tabellengetriebene Tests für OIDC (falsche Signatur, `aud`/`iss`/`exp`, `alg`-Verwechslung) und `googletoken`. — *M*
- **CI-6** Healthcheck auf `/api/health` in `compose.yml` (großzügiges `start_period`). — *S*
- **CI-7** Branch-Protection „up to date"/Merge-Queue aktivieren; AGENTS.md/README auf Ist-Stand korrigieren. — *S*
- **CI-9 / CI-10 / CI-11 / CI-12** `.env*` in `.dockerignore`; `127.0.0.1:5432` in `compose.dev.yml`; Node-Version vereinheitlichen (Image → 24); wirkungslosen `gradle-version`-Parameter entfernen. — *je S*
- **FE-6 / FE-7** Zentrales 401-Handling in `parse()`; `logout()` in try/finally mit garantiertem Aufräumen. — *je S*
- **GEN-2 / GEN-3 / PL-7 / PL-9 / PL-11 / WEB-7 / ST-6 / WEB-10** Duplikation entflechten (Store-Helfer, geteiltes Skript-Modul, Host-Capability für Scope + Sentinel-Fehler), Regex-Validierung, `unsafe-inline` ablösen, `date_unix` klemmen, Sanitizer-Kopplung dokumentieren/testen. — *je S–M*
- **CI-8** Entscheidung zur `go-imap` v2-Migration dokumentieren und als geplanten Brocken einlagern. — *L*

---

## 4. Nicht behandeln (bewusst)

Folgendes wurde geprüft und ist **korrekt** — kein Handlungsbedarf, hier nur zur Abgrenzung: smtplog-Redaction (alle Pfade), Blob-Pfad-/Traversal-Sicherheit, Move-Batching inkl. Claim-Settlement, Empty-Trash-Retry/Backoff, `pgbind`-Placeholder-Rewriter (inkl. Mixed-Refusal), Instance-Lock, Google-OAuth-Flow (`state` single-use, PKCE S256), Web-Push-SSRF-Prüfung, iframe-Sandbox-Kombination (`allow-same-origin` ohne `allow-scripts`), Service-Worker-Cache-Bounding, mailSnapshot-Validierung, `subtle.ConstantTimeCompare` in PKCE/Consent, JWT-`alg=RS256`-Enforcement. Das Non-Root-Image ohne Secret-Leaks und die aus Manifesten abgeleiteten Plugin-Listen sind vorbildlich.
