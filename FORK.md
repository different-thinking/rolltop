# Rolltop — dieser Fork

Dies ist ein Fork von [grahamsz/rolltop](https://github.com/grahamsz/rolltop).
Das Original ist ein selbst gehosteter Mail-Spiegel: ein Go-Dienst, der mehrere
IMAP-Postfächer in eigenen Speicher spiegelt und sie durchsuchbar, lesbar und
beantwortbar macht. Diese Datei beschreibt, was in diesem Fork anders ist —
damit vor der Installation klar ist, was man hier bekommt und was das Original
nicht hat. Die vollständige Bedienungs- und Betriebsdokumentation steht
unverändert in [`README.md`](README.md).

## Warum es diesen Fork gibt

Gmail wird zum Jahresende 2026 eingestellt. Bei mir hat Gmail über Jahre die
Rolle des Sammelpunkts gespielt: mehrere Adressen liefen dort zusammen, POP3-
und Weiterleitungskonten wurden eingesammelt, Kontakte und Kalender hingen
daran, und die Ablage — Wichtig, Werbung, Soziale Netzwerke — hat die Post
sortiert, bevor ich sie gesehen habe. Ein reiner IMAP-Client ersetzt davon
ungefähr die Hälfte.

Rolltop war der beste Ausgangspunkt, den ich gefunden habe: sauber gebaut,
selbst gehostet, die Daten bleiben, wo sie hingehören. Was fehlte, war genau
das, was Gmail über einen Mail-Client hinaus ausgemacht hat — Google-Konten
ohne App-Passwörter, Kontakte und Kalender, eine Ablage, die selbst sortiert,
Regeln, die aufräumen, und ein Unterbau, der ein gewachsenes Postfach von
mehreren zehntausend Nachrichten aushält, ohne beim ersten Vollsync
umzukippen. Das ist der Inhalt dieses Forks.

Er wird produktiv betrieben, nicht als Demo. Alles hier Beschriebene läuft
gegen echte Postfächer.

## Der Stand in Zahlen

| | |
| --- | --- |
| Abzweigpunkt | `7daaad2`, 4. August 2026 |
| Commits seither | 488 (352 ohne Merges) |
| Geänderte Dateien | 654 |
| Zeilen | +96.711 / −8.754 |
| Go-Testdateien | 150 → 282 |

Der Fork ist seit dem Abzweig nicht mehr mit dem Original zusammengeführt
worden. Was das Original seither gebaut hat, steht weiter unten unter
[Wo das Original weiter ist](#wo-das-original-weiter-ist).

---

## Die großen Unterschiede

### 1. Von SQLite auf PostgreSQL

Das Original speichert seine Metadaten in einer SQLite-Datei im Datenverzeichnis.
Das ist für eine Installation auf der eigenen Maschine richtig — und es ist genau
das, was bei gehosteten Deployments bricht: SQLite und ein netzwerkgebundenes
Volume vertragen sich nicht, und das Ergebnis ist `database disk image is
malformed` zu einem Zeitpunkt, an dem der Spiegel schon Wochen an Post enthält.

Dieser Fork läuft auf PostgreSQL. Eine Datenbank für die ganze Installation,
jede Zeile über `user_id` einem Nutzer zugeordnet, `user_id`-Scoping als einzige
Mandantentrennung — es gibt keine Datei pro Nutzer mehr, auf die man
zurückfallen könnte, und entsprechend streng ist die Regel im Code durchgezogen.

Was daran hängt:

- **Schema-Baseline aus dem SQLite-Schema abgeleitet** und danach inkrementelle
  Migrationen darauf geschichtet, statt zwei Schemata parallel zu pflegen.
- **Preflight-Skript und Admin-Prüfung** (`scripts/pg-preflight.sql`), die vor
  der Inbetriebnahme sagen, ob ein verwalteter Anbieter das mitbringt, was
  Rolltop braucht — Collation, Erweiterungen, Rechte.
- **Startup wartet auf die Datenbank**, statt in eine Crash-Schleife zu laufen:
  App-Container und Datenbank starten unabhängig voneinander.
- **Datenbank weg heißt nicht App weg.** Ist PostgreSQL nicht erreichbar, liefert
  Rolltop die App-Shell mit einem Banner aus, statt auf alles mit 500 zu
  antworten; die Admin-Seite bleibt erreichbar, der Sync protokolliert und
  versucht es beim nächsten Durchlauf erneut.
- **Ein Server pro Datenbank**, erzwungen über einen PostgreSQL-Advisory-Lock mit
  Liveness-Ping — zwei Deployments auf einer Datenbank würden sonst gegenseitig
  ihre Sync-Läufe abwürgen und jede Nachricht doppelt holen.
- **Backups gehen über `pg_dump`**, nicht über das Kopieren eines Volumes.
- **Zwei Container statt einem**: `compose.yml` und `compose.dev.yml` liegen im
  Repository und starten App und Datenbank zusammen.

### 2. Google-Konten als vollwertige Konten

Im Original ist ein Gmail-Postfach ein IMAP-Server wie jeder andere, mit
App-Passwort. Hier ist es ein verbundenes Konto:

- **OAuth statt App-Passwort.** Unter Einstellungen → Google wird ein Konto
  verbunden; IMAP und SMTP melden sich per XOAUTH2 daran an, in beide Richtungen
  ohne gespeichertes Passwort. Tokens liegen verschlüsselt und werden von der API
  nie zurückgegeben. Ein abgelaufener oder zurückgezogener Grant wird erkannt und
  als „muss neu autorisiert werden" markiert, statt still zu scheitern.
- **Gmail-gerechte Voreinstellungen.** All Mail, Wichtig und Markiert sind vom
  Sync ausgenommen — es sind Sichten auf Nachrichten, die schon in einem echten
  Ordner liegen, und ein Spiegel, der eine Nachricht in einem Ordner ablegt,
  würde damit das halbe Postfach doppelt holen. Dazu ein **Sync-Startdatum**, das
  als IMAP-`SINCE` durchgereicht wird, sodass alte Bodies gar nicht erst über die
  Leitung gehen.
- **Kontakte über die People API.** Alle 15 Minuten, mit Rückschreiben: Änderungen
  in Rolltop landen in Google, Google gewinnt bei Konflikten und sagt das auch,
  ein lokaler Kontakt mit derselben Adresse wird verknüpft statt verdoppelt.
  Zwei Google-Kontakte dürfen sich eine Adresse teilen (Haushalt, Rollenpostfach),
  und es ist an einer Stelle entschieden, wer für diese Adresse antwortet. Wird
  ein Konto getrennt, bleiben die Kontakte als lokale erhalten.
- **Kalender mit Wochenansicht.** Google-Kalender werden gespiegelt, inklusive
  Rückschreiben von Terminen dort, wo die Zugriffsrolle es erlaubt; ein
  Schreibkonflikt wird bei dem Lesevorgang aufgelöst, der ihn findet.
- **Der richtige Einlieferungsport.** Ausgehende Google-Post geht über 587 mit
  STARTTLS, mit 465 als Alternative für Netze, die 587 sperren — der
  Verbindungstest sagt, welcher durchkommt.

Das alles ist optional. Ohne konfigurierte Google-Credentials meldet die
Einstellungsseite den Server als nicht konfiguriert, und sonst ändert sich nichts.

### 3. Eine Ablage, die selbst sortiert

Das Original zeigt Ordner und All Mail. Hier führt die Seitenleiste mit **Inbox** —
All Mail abzüglich des Archivordners jedes Kontos, also alles, was noch nicht
weggelegt ist, über alle Konten hinweg.

Darunter die **Kategorien**, eine Liste je Kategorie, eine Kategorie je Nachricht:

| Kategorie | Woran erkannt |
| --- | --- |
| Relevant | trägt keine Listen- oder Automations-Marker — was übrig bleibt |
| Newsletters | `List-Id` oder ein Abmeldeweg, ohne Rückkanal |
| Forums | `List-Post` — die Liste lädt selbst zum Antworten ein |
| Notifications | Quittungen, Alarme, No-Reply-Roboter |
| Invoices & Contracts | Rechnungen, Belege, Verträge und deren Änderungen |

Die ersten vier werden aus den Headern entschieden, die der Absender gesetzt hat.
Das ist Absicht: die Antwort ist stabil, und ein Leser kann sie selbst
nachprüfen. Kein Header unterscheidet allerdings eine Rechnung von einer
Versandbenachrichtigung, deshalb ist **Invoices & Contracts** der einzige
Sonderfall — entschieden aus Betreff, Body und Anhangsnamen: eine strukturierte
E-Rechnung (ZUGFeRD, Factur-X, XRechnung), eine Datei, die nach dem benannt ist,
was sie ist, eine Belegnummer neben einem Betrag. Und sie wird nur aus
Notifications und Newsletters herausgeschnitten: Post von einem Menschen bleibt,
wo sie ist, egal wie sehr sie nach Papierkram klingt.

Dazu:

- **Ziehen auf eine Kategorie** oder „Absender ablegen unter …" legt diesen
  Absender dauerhaft dort ab — die Korrektur für alles, was die Regeln falsch
  gelesen haben.
- Ändern sich die Regeln, wird bereits abgelegte Post im Hintergrund
  häppchenweise neu gelesen. Der Durchlauf kann eine Antwort nur verbessern,
  nie raten: was nicht mehr vollständig vorliegt, behält, was es hat.
- **Sent, Drafts, Trash und Junk sind aus diesen Listen heraus** — sie
  beantworten „was liegt an", und die eigenen Antworten liegen nicht an. Pro
  Ordner umschaltbar, Junk immer.
- **Kontenübergreifende Dubletten werden erkannt und ausgeblendet.** Ein Konto,
  das die anderen einsammelt — genau das Gmail-Muster —, liefert eine zweite
  Zeile für dieselbe Zustellung. Ausgeblendet wird nur, wenn genau ein Konto
  tatsächlich adressiert war; bei Bcc oder Verteilerlisten bleibt jede Kopie
  sichtbar, weil der Spiegel dann nicht sagen kann, welche das Original ist.
  Die Einstellungsseite zeigt die Zahl pro Konto, kann nachträglich suchen und
  die Kopien in den Papierkorb des einsammelnden Kontos verschieben.
- **`Ältere archivieren`** räumt eine ganze Liste vor einem gewählten Tag in die
  Archivordner der jeweiligen Konten — in SQL, in wiederholten Durchläufen, und
  ohne die eigene Post anzufassen.
- **`Papierkorb leeren`** ist die einzige Stelle, an der Rolltop auf dem Server
  löscht statt verschiebt: `\Deleted` und Expunge unter geprüfter `UIDVALIDITY`,
  sodass auch Post verschwindet, die nie heruntergeladen wurde.

### 4. Regeln, die tatsächlich aufräumen

Das `mail_filters`-Plugin gibt es im Original auch. Hier ist es weitgehend neu
geschrieben:

- **Editor mit benannten Feldern** für Absender, Betreff und ein Alter in Tagen,
  die in die Rolltop-Suchsyntax geschrieben werden — die Suche selbst bleibt für
  alles andere daneben stehen. Aus einer offenen Nachricht heraus lässt sich eine
  Regel auf diesen Absender oder Betreff starten.
- **Aktionen: als gelesen markieren, weiterleiten, verschieben.** `mark_read`
  kombiniert sich mit den anderen, statt sich mit ihnen auszuschließen, und läuft
  vor dem Verschieben, weil das Verschieben das Leben der Nachricht im Quellordner
  beendet. Die frühere `star`-Aktion ist als Produktentscheidung entfallen.
- **Ziele, die der Leser benennen kann**: „Löschen" und „Archivieren" sind relativ
  zum *eigenen* Konto der Nachricht und werden pro Nachricht aufgelöst — eine
  Regel über mehrere Konten legt also jede Nachricht im Ordner ihres Kontos ab.
  Ein exakter Ordner geht auch, aber nur innerhalb eines Kontos, und der Editor
  sagt das, wenn der Geltungsbereich weiter reicht als das Ziel.
- **Weiterleiten nur für neue Post** ist umschaltbar und für neue Regeln
  voreingestellt. Der Backfill läuft trotzdem, verschiebt trotzdem, protokolliert
  trotzdem — nur die Weiterleitung unterbleibt, statt Jahre alter Post an einen
  Dritten zu schicken.
- **Die Kopie, die der Anbieter von jeder Weiterleitung behält**, wird als eigene
  ausgehende Post erkannt und aus den Listen gehalten. Ohne das griff eine
  Weiterleitungsregel wieder auf die Kopie zu, die sie selbst erzeugt hatte.
- **Ein Audit über 30 Tage**, das drei Ausgänge auseinanderhält — schon gelesen,
  eingereiht (lokal gelesen, `\Seen` wartet noch auf eine beweisbare
  Mailbox-Generation), fehlgeschlagen — und eine Seite, die beide Hälften zeigt:
  was die Filter getan haben und worauf sie noch warten.
- „Post von diesem Absender nach N Tagen löschen" ist damit eine Regel aus drei
  Feldern. Die N Tage laufen ab dem `Date` der Nachricht, nicht ab dem Anlegen der
  Regel.

### 5. Suche

- **Zwei Backends, über `ROLLTOP_SEARCH_BACKEND` umschaltbar.** Bleve wie im
  Original, oder **PostgreSQL** mit einer `tsvector`-Tabelle: kein
  memory-mapped Index, der mit dem Heap um das Container-Limit konkurriert, kein
  Indexverzeichnis auf `/data`, und die ganze Maschinerie gegen hängende
  Index-Writer bleibt ungeladen.
- **Fuzzy-Treffer bei Tippfehlern** über `pg_trgm`, und zwar nur dann — ein
  Begriff, der so existiert, wird nicht verwaschen.
- **Ranking auf Relevanz**, mit Absenderhistorie, Kontakt-Boost und einem
  Aktualitäts-Anstoß, und einer Erklärung, welcher Rang gewirkt hat.
- **Nach Datum sortierbar**, in beide Richtungen, pro Nutzer im Browser
  gespeichert; eine Datumssortierung ersetzt das Ranking, statt es nur zu
  entscheiden.
- **Trash bleibt draußen**, außer die Anfrage nennt ihn.
- Eine Seite Suchergebnisse kostet nicht mehr das ganze Postfach.
- Ein defekter Suchindex hält die Post nicht mehr auf: er wird repariert statt
  neu gebaut, Reparaturmarken werden serialisiert, und die betroffene
  Nachrichtenmenge wird eingegrenzt statt pauschal neu indiziert.

### 6. IMAP-Sync: Korrektheit, Tempo, Speicher

Ein eigener Analyse- und Umsetzungsdurchgang
([`docs/imap-sync-analysis.md`](docs/imap-sync-analysis.md)) in vier Phasen:

- **Verbindungen werden pro Durchlauf wiederverwendet** statt pro Nachricht neu
  aufgebaut; wie weit ein Abruf reicht, richtet sich nach dem erreichten
  Fortschritt; Fehlschläge laufen in einen gedeckelten Backoff.
- **Jeder Mailbox-Durchgang ist zeitbegrenzt** und hält an einer
  Nachrichtengrenze an, committet, was er gespiegelt hat, und wird sofort neu
  eingeplant. Jeder pausierte Durchgang verdoppelt das Zeitbudget des nächsten,
  bis zu zehn Minuten, damit ein Backfill seine Zeit mit Holen statt mit
  Neuplanen verbringt.
- **Speicherbudget pro Durchgang.** Fetch-Batches werden aus den Größen geplant,
  die der Server *vor* den Bodies meldet — begrenzt in Bytes, nicht nur in
  Nachrichten. Ein paar sehr große Mails in einem Ordner bestimmen damit nicht
  mehr, wie viel Speicher der Prozess braucht.
- **`ROLLTOP_MEMORY_LIMIT`** setzt dem Go-Heap eine weiche Decke, standardmäßig
  80 % des Container-Limits, und der Start sagt, welcher Wert gilt. Er warnt
  außerdem, wenn Suchindex und Heap-Decke zusammen nicht in das Limit passen —
  der Fall, in dem jeder Index-Commit zu Major Faults wird und wie ein hängender
  Writer aussieht.
- **Ein Ordner voll Post wird mit einem IMAP-Befehl verschoben** statt mit einem
  pro Nachricht, und das Melden der Arbeit fällt nicht mehr pro Nachricht an.
- **Verschieben, das ankommt:** Ergebnisse werden pro Nachricht *und* pro Lauf
  festgeschrieben, ein Umzug scheitert nicht mehr an Post, die der Quellordner
  gar nicht mehr hat, ein Ordner, den ein Konto nicht besitzt, wird übersprungen
  statt den ganzen Sync zu kippen, und „Papierkorb leeren" macht weiter,
  wenn die Verbindung abbricht.
- **Ordner, die für den Flag-Sync zu groß sind**, werden trotzdem abgeglichen.
- Kaputte Bytes und beschädigte Anzeigenamen werden repariert, bevor sie in die
  Datenbank gehen.

### 7. Bedienung

- **Gmail-Optik für die Listen**: Palette, Layout, Datumsabschnitte, zweizeilige
  Zeilen, und eine auf 1100 px begrenzte Textbreite, damit eine Nachricht auf
  einem breiten Schirm lesbar bleibt; die Seitenleiste lässt sich ausblenden.
- **Zeilenaktionen beim Überfahren**: Antworten, Archivieren, Papierkorb,
  Gelesen/Ungelesen, Snooze — dieselben rückgängig machbaren Aktionen wie in der
  Auswahlleiste. Dazu **Spam melden**, das in den Junk-Ordner des Kontos
  verschiebt und damit auch den optionalen Spamfilter trainiert.
- **Befehlsleiste im Konversationskopf** — Antworten, Allen antworten,
  Weiterleiten, Archivieren, Papierkorb, Ungelesen, Spam melden, Filter anlegen —
  und beide Leisten bleiben beim Scrollen in Reichweite.
- **Verschieben und Löschen betreffen die ganze Konversation**, über Konten
  hinweg: ein Thread mit Kopien in mehreren Konten wird aufgeteilt, und wenn ein
  Konto seinen Umzug nicht schafft, gibt es seine Zeilen zurück, während der Rest
  abgelegt bleibt.
- **`Send & archive`**: `Ctrl`/`Cmd`+`Enter` auf eine Antwort ist Senden,
  Zurückgehen und Archivieren in einem Schritt.
- **Der Antwort-Editor** öffnet sofort, ist auf 60 % der Fensterhöhe begrenzt und
  scrollt sich selbst in Sicht.
- **Kategorie-Pille** — jede Nachricht sagt, wo sie einsortiert wurde: auf der
  Kopfzeile der Konversation, und in **All Mail** zusätzlich in jeder Zeile der
  Liste. All Mail ist die einzige Liste, die alle Kategorien auf einmal hält, und
  damit die einzige, in der eine noch nicht einsortierte Nachricht — „Not sorted
  yet" — beim Durchsehen auffällt, statt gesucht werden zu müssen. Auf schmalen
  Bildschirmen bleibt von der Pille ihr Symbol, damit die Vorschauzeile daneben
  eine Vorschau bleibt; der Name steht weiter im Tooltip und im Screenreader.
  Die Pille ist eine Beschriftung und kein Link: eine Kategorieliste ist die
  noch anliegende Post einer Kategorie, und archivierte, zurückgestellte oder
  gelöschte Post steht nicht in der Liste, die ihre eigene Kategorie benennt.
- **Zahlenkürzel in der Seitenleiste**, einklappbare Ordnergruppen pro Konto,
  deren Zustand Navigation und Drag überlebt.
- **Sortierung nach Datum** in beide Richtungen in jeder Liste, pro Nutzer
  gespeichert.
- **Themes sind ein Token-Tausch** statt einer Liste von Patches pro Komponente,
  das System-Theme wird befolgt und schon der erste Seitenaufbau darin
  gezeichnet, und HTML-Post darf im Dunkelmodus die Farben ihres Absenders
  behalten. Eine CI-Prüfung hält die Paletten davon ab, wieder
  auseinanderzulaufen.
- **`Syncs & Tasks`** im Kontomenü listet alles, was im Hintergrund für den
  angemeldeten Nutzer läuft, mit der Zahl der laufenden Durchgänge — ein
  hängender Sync ist sichtbar, ohne die Ansicht zu öffnen.
- Inline-Bilder, blockierte Fernbilder und PDF-Vorschau sind an vielen Stellen
  repariert: eine Nachricht lädt die Bilder, die sie selbst mitbringt, und fragt
  Rolltop nicht mehr nach dem Rest.

### 8. Betrieb

Das Original ist als Ein-Container-App auf der eigenen Maschine gedacht. Dieser
Fork läuft auf einem Hoster, und alles Folgende kommt daher:

- **`/api/health`** antwortet `200`, sobald der Prozess das Datenverzeichnis
  besitzt und fertig gestartet ist, sonst `503` mit der Phase, auf die er wartet.
- **Geordnetes Herunterfahren**, begrenzt durch `ROLLTOP_SHUTDOWN_TIMEOUT`, mit
  festen Anteilen pro Phase, jede Phase protokolliert und in
  `/data/crash-state.json` festgehalten. Wird der Prozess vorher abgeschossen,
  sagt der nächste Start, in welchem Schritt er stand — und ob die
  Stop-Gnadenfrist der Plattform zu kurz ist.
- **Überlappende Deployments**: ein `flock` auf dem Datenverzeichnis, auf den ein
  startender Prozess bis zu `ROLLTOP_STARTUP_LOCK_WAIT` wartet, statt sich zu
  weigern. Ein verwaister Lock läuft ab, statt jeden Start zu blockieren.
- **Crash-Berichte im Datenvolumen** (`/data/crash.log`), angehängt statt
  überschrieben, mit Startmarke pro Lauf, Rotation ab 1 MiB — scharfgeschaltet,
  bevor der Listener bindet und bevor die Konfiguration gelesen wird.
- **Log-Level über `ROLLTOP_LOG_LEVEL`**; Debug-Zeilen sind standardmäßig still,
  verschluckte HTTP-Handler-Fehler werden protokolliert.
- **Admin-Seite `Database`**: Version, Größe, Replikationszustand,
  Verbindungszahl, Round-Trip-Latenz, PostgreSQL-Preflight, Backups, Reparatur,
  Migrationskonsole und die neuesten Logzeilen — auf einer gehosteten
  Installation ist das der einzige Weg, an diese Antworten zu kommen.
- **SMTP-Transkript**: Einstellungen → Mail zeigt zu jedem ausgehenden Server
  einen Verbindungstest und die letzten echten Sendeversuche, jeweils
  aufklappbar bis zur Antwort auf `AUTH` und auf die Nachricht. Zugangsdaten
  sind auf den Mechanismus geschwärzt, der Inhalt auf eine Bytezahl; nichts
  davon geht in die Datenbank.

### 9. Sicherheit

Ein vollständiger Review des Repositories mit anschließendem Umsetzungsplan in
fünf Phasen ([`docs/code-review-umsetzungsplan.md`](docs/code-review-umsetzungsplan.md),
auf Deutsch) — Datenverlust, DoS, Fail-Open und CI-Lücken zuerst, danach Auth,
Web, Plugins, Sync, Datenintegrität, Performance und Infrastruktur. Was der Fork
daraus mitbringt, teils zeitgleich und unabhängig vom Original entstanden:

- Anmeldung und Passwort-Reset werden pro Client-IP und Zieladresse mit
  exponentiellem Backoff gedrosselt, ohne eine Adresse je dauerhaft zu sperren.
- `ROLLTOP_PUBLIC_URL` als vertrauenswürdige Basis für Links, die die App auf
  sich selbst baut — sonst entscheidet ein vom Client setzbarer `Host`-Header,
  wohin ein Reset-Link oder ein OAuth-Redirect zeigt.
- Der Webhook-Token wird nur noch aus Headern angenommen, nicht mehr aus der
  Query, wo er in jedem Zugriffslog landet.
- Session- und CSRF-Cookies bekommen `Secure` automatisch auf jeder Anfrage, die
  über HTTPS ankommt, dazu HSTS.
- `script-src 'unsafe-inline'` ist aus der CSP verschwunden; die Kopplung
  zwischen Mail-Body-Sanitizer und Renderer ist festgezurrt.
- Google-Tokens verschlüsselt at rest, PKCE mit einmaligem `state`, Absicherung
  gegen entführte Consent-Rückläufe.
- Das OIDC-Plugin validiert `id_token` jetzt wirklich (mit Tests), das
  Remote-Image-Blocklist-Plugin ist gehärtet, `gravatar` entkoppelt.
- SQL-Identifier laufen über ein eigenes Quoting-Paket (`backend/sqlident`);
  Schema-Prüfsummen lesen SQL statt Layout.
- Ein Startzustand, der aus einer unlesbaren Datei stammt, spielt keinen alten
  Crash mehr nach.

### 10. Build, Tests, CI, Deployment

- **CI ist geteilt.** `pr.yml` bestimmt aus den geänderten Pfaden, welche Jobs
  überhaupt laufen müssen — Go, Frontend, Android, Docker —; ein reiner
  Dokumentations-PR kostet nur zwei Koordinationsjobs. `ci.yml` ist das Tor nach
  dem Merge und die Paketierung, letztere an `v*`-Tags gebunden statt an jeden
  Merge.
- **`go vet` als Tor**, plus Formatprüfung, `-buildmode=plugin`-Linktest für
  geänderte Plugin-Backends und Verifikation des eingecheckten Spam-Modells.
- **Vitest im Frontend**, mit Testlauf in CI — im Original zum Abzweigzeitpunkt
  nicht vorhanden.
- **282 statt 150 Go-Testdateien**, darunter Nachweise für Mandantentrennung auf
  den Listenrouten und Tests, die unter dem Race-Detector laufen.
- **Der Image-Build wurde deutlich schneller und sparsamer**:
  Layer-Reihenfolge, parallele Builds nach Speicher bemessen, der Go-Schritt
  in einem Stück gebaut und mit seinen Caches verworfen, Phosphor-Icons aus
  den Plugin-Bundles heraus.
- **Deployment nach Hostim** aus dem Merge-Tor heraus, das auf Build und Rollout
  wartet — ein fehlgeschlagenes Deployment ist ein roter Workflow-Lauf. Ohne
  gesetzte Variablen wird der Job übersprungen, damit ein Fork eine grüne
  Pipeline bekommt.

---

## Wo das Original weiter ist

Fair ist fair. Seit dem Abzweig hat das Original 24 Commits, und einer davon ist
etwas, das dieser Fork nicht hat:

- **Offline-Modus.** Zuletzt gesehene Post bleibt offline benutzbar, mit
  eingereihten Sendungen (`offlineDb`, `offlineMailCache`, `offlineOutbox`,
  `offlineSearch`). Hier gibt es weiterhin nur die begrenzte PWA-Zwischenablage
  der ersten All-Mail-Seite, die beide Zweige vom gemeinsamen Stand geerbt haben.

Ein zweiter Block dort ist **Sicherheitshärtung**, die dieser Fork unabhängig
und weitgehend gleichlautend selbst gebaut hat: Login-Drosselung, Public-URL für
Reset-Links, Webhook-Token aus Headern, `Secure`-Cookies, CSRF auf Plugin-APIs,
verifizierte OIDC-E-Mails. Wer beide Zweige vergleicht, findet dort dieselben
Antworten auf dieselben Fragen.

Drittens: **Begrenzung übergroßer Nachrichten** — MIME-Rekursionstiefe,
dekodierte Body-Größen, eine Obergrenze für gespiegelte Nachrichten mit
Header-Platzhaltern darüber. Dieser Fork begrenzt den Sync über sein
Speicherbudget pro Durchgang, nicht über eine Größenobergrenze pro Nachricht;
das ist ein anderer Ansatz auf dasselbe Problem, aber nicht dieselbe Grenze.

## Für wen das gedacht ist

**Passt**, wenn eine der folgenden Beschreibungen zutrifft:

- Mehrere Adressen sollen an einer Stelle zusammenlaufen, so wie sie es bisher in
  Gmail getan haben — inklusive der Konten, die sich gegenseitig einsammeln.
- Google-Konten sollen ohne App-Passwörter angebunden werden, mit Kontakten und
  Kalender.
- Der Posteingang soll sich selbst sortieren, statt dass jede Nachricht einzeln
  in die Hand genommen wird.
- Es läuft auf einem Hoster mit verwalteter PostgreSQL-Instanz, nicht auf einem
  Laptop.
- Ein gewachsenes Postfach mit Jahren an Post soll gespiegelt werden.

**Passt nicht**, wenn:

- Eine Ein-Datei-Installation ohne Datenbank daneben gewünscht ist — dann ist das
  Original mit seinem SQLite-Aufbau die einfachere Wahl.
- Offline-Nutzung wichtig ist (siehe oben).
- Es ein fertiges Produkt sein soll. Es gibt keine Releases, keine Migrationshilfe
  vom Original und keine Zusage, dass ein Update nichts kaputt macht.

## Betrieb dieses Forks

Die vollständige Anleitung steht in [`README.md`](README.md); zwei Dinge sind
beim Fork zu beachten:

1. **`compose.yml` zeigt auf das Image des Originals**
   (`ghcr.io/grahamsz/rolltop:latest`).
   Das enthält diesen Code nicht — und da das Original auf SQLite läuft, passt es
   auch nicht zu der PostgreSQL-Konfiguration daneben. Entweder das Image aus
   diesem Repository bauen (`docker build -t rolltop:local .`) und den Eintrag
   darauf zeigen lassen, oder `ghcr.io/different-thinking/rolltop` verwenden,
   sobald hier ein `v*`-Tag veröffentlicht wurde. Bisher gibt es keinen.
2. **`ROLLTOP_DATABASE_URL` ist Pflicht.** Ohne sie beendet sich der Container
   sofort, was `--restart unless-stopped` in eine Crash-Schleife verwandelt.

## Verhältnis zum Original

Dieser Fork ist keine Konkurrenz und kein Ersatzangebot: Rolltop ist Grahams
Projekt, und der Unterbau, auf dem hier alles aufsetzt, ist seine Arbeit —
die Mandantentrennung, das Plugin-System, der Spiegel selbst. Was hier
dazugekommen ist, ist auf einen bestimmten Anwendungsfall zugeschnitten (der
Wegfall von Gmail als Sammelpunkt) und nicht auf allgemeine Nützlichkeit hin
entworfen. Wer das Original sucht, ist bei
[grahamsz/rolltop](https://github.com/grahamsz/rolltop) richtig.

Lizenz unverändert: AGPL-3.0-or-later.
