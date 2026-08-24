# Gesammelte Browser-Konsolenmeldungen

Sammlung von Meldungen, die beim Benutzen der App in der Browser-Konsole
auftauchen. Ein Eintrag wird nicht bewertet, wenn er hier landet — er liegt
hier, bis jemand ihn sich vornimmt. Ist das geschehen, hält der Eintrag fest,
was geändert wurde, damit die Sammlung nicht als offen liest, was es nicht
mehr ist.

Format je Eintrag: Datum, Browser, Meldung im Wortlaut, ggf. eine Zeile
Kontext dazu, wo im Code sie herkommt.

## 2026-08-19 — Edge

```text
about:srcdoc:1 Blocked script execution in 'about:srcdoc' because the document's
frame is sandboxed and the 'allow-scripts' permission is not set.
```

Zweimal aufgetreten. Dazwischen der Edge-Hinweis
„[NEW] Explain Console errors by using Copilot in Edge" — der gehört zum
Browser, nicht zur App.

Kontext: `EmailFrame` in `frontend/src/features/mail/ThreadView.tsx:2594`
rendert Nachrichten-HTML in einem iframe mit
`sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"`.
`allow-scripts` fehlt dort bewusst, damit Skripte aus fremder Mail nicht
laufen. Die Meldung ist also die Folge einer Absicht, kein Absturz.

Nachgetragen am 24.08.: Die Absicht bleibt, die Meldung nicht. Ausgelöst hat
sie kein `<script>` (das wird entfernt), sondern Inline-Handler wie das
`onerror` eines Bildes, das nicht lädt. Siehe den Eintrag vom 24.08.

## 2026-08-19 — Sync aus den Einstellungen, 409

```text
api.ts:138  POST https://3703c955.eu-center.hostim.dev/api/account/sync 409 (Conflict)
postJSON @ api.ts:138
syncAccount @ api.ts:615
syncNow @ SettingsViews.tsx:1438
processDispatchQueue @ react-dom-client.production.js:12317
anonymousCallbackTo: batchedUpdates$1 @ react-dom-client.production.js:12867
batchedUpdates$1 @ react-dom-client.production.js:1498
dispatchEventForPluginEventSystem @ react-dom-client.production.js:12455
dispatchEvent @ react-dom-client.production.js:15306
dispatchDiscreteEvent @ react-dom-client.production.js:15274
```

Ausgelöst über „Jetzt synchronisieren" in den Einstellungen
(`syncNow` in `frontend/src/features/settings/SettingsViews.tsx`).

Kontext: `POST /api/account/sync` landet in `apiAccountSync`
(`backend/web/api_account.go:1085`). Wenn `syncRunner.Start` für den Benutzer
`false` zurückgibt, antwortet der Server mit 409 und
„Sync is already running for this account." — der 409 ist also der geplante
Weg, einen zweiten parallelen Sync abzulehnen. Offen bleibt für später, ob
die Oberfläche das als Fehler zeigt oder ruhig behandelt.

## 2026-08-19 — Nachricht geöffnet, Inline-Anhang 404

```text
about:srcdoc:1 Blocked script execution in 'about:srcdoc' because the document's
frame is sandboxed and the 'allow-scripts' permission is not set.
about:srcdoc:117  GET https://3703c955.eu-center.hostim.dev/attachments/19026/inline 404 (Not Found)
```

Die erste Zeile ist die schon oben gesammelte Sandbox-Meldung. Neu ist die
zweite: Der Mail-Body im iframe lädt ein eingebettetes Bild, und der Server
antwortet mit 404.

Kontext: `email_document.go:126` ersetzt `cid:`-Verweise im Nachrichten-HTML
durch `/attachments/<id>/inline`. Der Handler ist `handleAttachment`
(`backend/web/server.go:464`, Pfadauswertung ab Zeile 640). Er antwortet an
mehreren Stellen mit 404: Anhang nicht für den Benutzer gefunden, Blob-Datei
nicht zu öffnen, oder der Anhang lässt sich in der geparsten Rohnachricht
nicht wiederfinden. Welche der drei es hier war, ist aus der Konsole allein
nicht zu sehen — dafür bräuchte es das Serverlog zur Anhang-ID 19026.

Nachgetragen am 24.08.: Der Handler protokolliert den Grund jetzt selbst, und
zwei der drei Ursachen sind behoben. Siehe den Eintrag vom 24.08.

## 2026-08-19 — Event-Stream bricht ab (ERR_HTTP2_PROTOCOL_ERROR)

```text
about:srcdoc:1 Blocked script execution in 'about:srcdoc' because the document's
frame is sandboxed and the 'allow-scripts' permission is not set.
about:srcdoc:117  GET https://3703c955.eu-center.hostim.dev/attachments/19026/inline 404 (Not Found)
about:srcdoc:1 Blocked script execution in 'about:srcdoc' because ... (2x wiederholt)
/api/events:1  GET https://3703c955.eu-center.hostim.dev/api/events net::ERR_HTTP2_PROTOCOL_ERROR 200 (OK)
```

Die ersten vier Zeilen sind die bereits gesammelten (Sandbox dreimal, dazu der
404 auf Anhang 19026). Neu ist die letzte.

Kontext: `/api/events` ist der SSE-Stream. Das Frontend öffnet ihn in
`frontend/src/App.tsx:680` per `new EventSource("/api/events")`, bedient wird
er von `apiEvents` (`backend/web/api_sync.go:37`) mit
`Content-Type: text/event-stream`, `Cache-Control: no-store` und
`X-Accel-Buffering: no`.

Bemerkenswert: Der Status ist 200 (OK) und trotzdem meldet der Browser
`net::ERR_HTTP2_PROTOCOL_ERROR`. Das ist das Muster einer Verbindung, die
nach den Headern abbricht — also eine lang laufende Antwort, die
zwischendurch beendet wird, statt eines fehlgeschlagenen Requests. Ob das der
Server, ein Proxy oder ein Timeout in der Kette ist, sagt die Konsole nicht.
Zu prüfen wäre später, was zwischen Browser und App terminiert (die URL
läuft über `eu-center.hostim.dev`) und ob ein Keepalive im Stream fehlt.

## 2026-08-19 — Löschen vieler Mails, 504

```text
api.ts:138
 POST https://3703c955.eu-center.hostim.dev/api/messages/scope-trash 504 (Gateway Timeout)
postJSON	@	api.ts:138
scopeTrashMessages	@	api.ts:412
deleteWholeScope	@	MailViews.tsx:1740
anonymousFunction	@	MailViews.tsx:2733
```

Beobachtung des Users: tritt beim Löschen vieler Mails auf.

Kontext: `deleteWholeScope` (`frontend/src/features/mail/MailViews.tsx:1740`)
ruft `scopeTrashMessages` (`frontend/src/api.ts:412`) auf, das ist
`POST /api/messages/scope-trash` → `apiScopeTrashMessages`
(`backend/web/api_message_scope.go:145`). Der Handler bekommt keine IDs,
sondern nur den aktuellen Filter und löst die Auswahl serverseitig auf:
`scopeTrashPlan` ermittelt bis zu `scopeTrashMessageLimit = 20000` IDs
(bei einem Such-Scope `scopeSearchMessageLimit = 5000`, in Bleve-Seiten zu je
100) — und zwar synchron, bevor geantwortet wird. Verschoben wird danach im
Hintergrund per Sync-Runs.

Das 504 kommt von einem Gateway, nicht vom Handler selbst: Die Antwort kam
der Gegenstelle zu spät. Passend dazu, dass die Planauflösung mit der Anzahl
der Treffer wächst. Offen für später: ob das Auflösen des Plans ebenfalls in
den Hintergrund gehört (Antwort sofort, Fortschritt über den Event-Stream),
oder ob das Limit bzw. das Timeout der Gegenstelle anzupassen ist. Wie viele
Mails im konkreten Fall betroffen waren, ist nicht festgehalten.

## 2026-08-19 17:06 — kein Absturz, sondern ein geplanter Neustart

Frage des Users: „17:06 ist die App gecrashed. Warum?" — Der Serverlog zeigt
keinen Absturz, sondern die eingebaute Reaktion auf einen hängenden
Bleve-Schreiber. Ablauf, aufsteigend gelesen:

- bis 17:03:54: Repair des Suchindex für account_id=3, INBOX; 15.500
  Nachrichten indiziert, in Stapeln zu 25 (`explicitSearchRepairBatchSize`,
  `backend/syncer/search_batch.go:19`).
- ~17:04:09: ein Stapel (25 Dokumente, 232.613 Bytes, Dokumente 25387–25411,
  mailbox_id=158) geht in `bleve.Batch` und kommt nicht zurück.
- 17:06:09: der Wächter `watchActiveWriter`
  (`backend/search/search.go:711`, Schwelle `activeWriterStallTimeout = 2m`,
  Zeile 194) schlägt an — der Stapel steckte also seit ~17:04:09 in Bleve.
  Er schreibt einen dauerhaften Recovery-Marker, begrenzt auf genau diesen
  Dokumentbereich, protokolliert den Stack (`scorch.prepareSegment` ←
  `Scorch.Batch` ← `indexImpl.Batch` ← `commitPreparedMessageChunk`) und
  fordert einen Neustart an.
- 17:06:09: `cmd/rolltop/main.go:536` meldet „controlled restart requested"
  und bricht den App-Kontext ab — daher „mailbox maintenance ... context
  canceled" in derselben Sekunde.
- 17:06:14: der Schreiber lässt sich nicht stoppen („search index writer did
  not stop before close"). Das ist erwartet: `bleve.Batch` kennt kein
  Abbrechen. Der Prozess endet mit dem Sentinel `errRestartForRecovery`
  (`cmd/rolltop/main.go:51`), der diesen Exit bewusst aus dem Crash-Report
  heraushält.
- 17:06:15/16: neuer Prozess, Marker wird eingelöst („repaired stalled search
  index ... pending_messages=25 ... index_retained=true"), „ready" um
  17:06:16. Ausfall also rund sieben Sekunden.

Warum Bleve hing, sagt der Log nicht direkt. Der stärkste Hinweis ist die
Warnung des neuen Prozesses um 17:06:15: der Index ist 836,3 MiB groß, die
Heap-Decke liegt bei 3,2 GiB von 4,0 GiB, es bleiben 819,2 MiB für alles
außerhalb des Heaps. Bleve liest seine Segmente per mmap — ein Index, der
größer ist als dieser Rest, wird bei jedem Commit ein- und ausgelagert, und
`prepareSegment` ist genau die Stelle, an der neue Segmentbytes entstehen.
Dazu passt das Tempo des Repairs: 500 Dokumente in 16 Sekunden (17:03:38 →
17:03:54), also rund 31 Dokumente/s. Gleichzeitig liefen zwei
INBOX-Reparaturen (account 3: 32.065 lokal zu 68.547 remote; account 1: 4.803
zu 13.604), Google-Kalender- und Kontaktsync und die Anhang-Indizierung.

Bewiesen ist der Speicherpfad damit nicht — der Stack zeigt nur, wo es stand,
nicht warum. Plattenlatenz auf dem Volume wäre die andere Erklärung. Die
Warnung nennt selbst die zwei Stellschrauben: dem Container mehr Speicher
geben oder `ROLLTOP_MEMORY_LIMIT` senken, damit der Index resident bleibt.
Solange der Index weiter wächst, wiederholt sich die Lage.

Offen: Der 504 beim Massenlöschen und der Abbruch von `/api/events` könnten
zu genau diesem Neustart gehören — dafür fehlen in der Sammlung die
Uhrzeiten der beiden Konsolenmeldungen.

## 2026-08-24 — Bilder einer Mail laden nicht

```text
Loading the font 'https://3703c955.eu-center.hostim.dev/messages/static/
DuplicateSans-Regular-Web-1610258950.woff2' violates the following Content
Security Policy directive: "font-src data:". The action has been blocked.
about:srcdoc:1 Loading the font 'https://3703c955.eu-center.hostim.dev/messages/
static/DuplicateSans-Regular-Web-606133164.woff' violates the following Content
Security Policy directive: "font-src data:". The action has been blocked.

5 Blocked script execution in 'about:srcdoc' because the document's frame is
sandboxed and the 'allow-scripts' permission is not set.
/attachments/48027/inline:1  Failed to load resource: the server responded with
a status of 404 ()
```

Alle drei Meldungen sind vorgenommen worden; was daran geändert wurde, steht
unten. Der Reihe nach:

**Die Schriften.** Die URL gehört Rolltop, nicht dem Absender: Ein
`about:srcdoc`-iframe erbt die URL der Leseransicht als Basis, also löst jede
*relative* Referenz einer Mail gegen die App auf. Die Mail schrieb
`url(static/DuplicateSans-Regular-Web-1610258950.woff2)`, der Browser fragte
daraufhin `/messages/static/…` bei Rolltop an — mit blockierten Bildern
verweigert das die CSP, mit erlaubten Bildern antwortet die App mit ihrem
eigenen HTML oder einem 404. Reparieren lässt sich so eine Referenz nicht, denn
die Herkunft steht nirgends in der Nachricht. `neutralizeUnresolvableRefs`
(`backend/web/email_document.go`) entfernt sie deshalb wie eine blockierte
Fremdreferenz und parkt sie unter `data-rolltop-unresolved-*` — in beiden
Modi, denn „Bilder erlauben" erlaubt den Server des Absenders und nicht die
eigenen Routen. Nachtrag 24.08.: Die erste Fassung hat dabei zu viel entfernt. Bei
erlaubten Bildern schreibt `remoteimages.ReplaceCached` Fremdbilder auf
Rolltops eigene Cache-Route `/remote-images/<hash>` um, *bevor* das
Dokument gebaut wird — und die sah wie jeder andere absolute Pfad aus, den
der Absender geschrieben haben könnte. Ergebnis: ein Newsletter ohne jedes
Bild, sichtbar im DOM als `data-rolltop-unresolved-src="/remote-images/…"`.
Die zwei Routen, die der Renderer selbst in einen Body schreibt
(`/attachments/` und `/remote-images/`), stehen jetzt als Konstanten in
`resolvableRefPrefixes` — bei den Routen, die sie bedienen, damit eine
verschobene Route diese Liste nicht ins Leere zeigen lässt.

Setzt die Mail selbst ein `<base href="https://…">`, wird es angewendet und
dann entfernt: Die Basis verschiebt nämlich nicht nur die Verweise des
Absenders, sondern auch die zwei Pfade, die der Renderer selbst schreibt —
`/remote-images/<hash>` und `/attachments/<id>/inline` wären beim Absender
gelandet. `resolveRefsAgainstDeclaredBase` löst die Absender-Verweise
deshalb selbst auf (aus `hero.png` wird `https://…/hero.png`) und nimmt das
`<base>` weg; danach ist alles Übrige entweder absolut — und damit eine
ganz normale Fremdreferenz, über die der Bilder-Modus entscheidet — oder
hatte nie eine Basis und fällt als unauflösbar weg.

**Die fünf Sandbox-Meldungen.** (Nachtrag 24.08., nachdem die Meldung
weiterhin auftrat: Der Scrubber sah zwei Schreibweisen nicht. Ein Browser
beginnt ein neues Attribut hinter einem zitierten Wert auch ohne
Leerzeichen — `<img src="x"onerror="…">` und `<img src="x"/onerror="…">`
tragen beide einen Handler —, `tagAttrRE` verlangte aber eines. Und der
Vortest, der entschied, ob der Durchlauf überhaupt lohnt, suchte nach
`\son…=` beziehungsweise nach `javascript:` in Buchstaben; eine Mail, die
es anders schreibt, sprang genau an dem Durchlauf vorbei, der für sie
gedacht war. Der Vortest ist weg, das Attributmuster kennt beide
Trennungen. Bei einem *unzitierten* Wert bleibt der Schrägstrich Teil der
URL — da hält der Browser es genauso.) Die schon am 19.08. gesammelte Meldung, hier
fünfmal. Skripte laufen nicht — der iframe ist ohne `allow-scripts`
sandboxed, und die Dokument-CSP erlaubt keine Skriptquelle —, aber ein
blockierter Versuch ist nicht still: Jedes `onerror` eines Bildes, das nicht
lädt, kostet eine Zeile. `<script>` wurde schon vorher entfernt; jetzt
entfernt `removeInlineScripting` zusätzlich Event-Handler-Attribute und
`javascript:`-URLs, aus demselben Grund.

**Der 404 auf `/attachments/48027/inline`.** Der Anhang, den der Mail-Body
per `cid:` einbindet, war über die Anhang-Route nicht zu bekommen. Am 19.08.
stand hier noch, dass aus der Konsole nicht zu sehen ist, welche der drei
404-Stellen im Handler es war. Das ist jetzt zu sehen: `handleAttachment`
protokolliert bei jedem 404 den Grund samt Anhang-ID
(`attachment unavailable … reason=…`). Zwei Ursachen sind zugleich behoben:

- Eine Anhangsdatei, die sich nicht öffnen lässt (Retention hat sie entfernt,
  oder sie hat die Platte nie erreicht), beendet die Anfrage nicht mehr mit
  404, sondern fällt auf die Rohnachricht zurück — denselben Weg, den jeder
  Anhang ohne eigene Datei ohnehin geht.
- `mailparse` hat Teile ohne Dateinamen und ohne `Content-Disposition`
  verworfen. Ein eingebettetes Bild wird aber genau so verschickt: nur
  `Content-Type: image/png` und `Content-ID`. Ohne Anhangszeile zeigte der
  `cid:`-Verweis im Body ins Leere. `isCIDReferencedPart` nimmt solche Teile
  jetzt auf (text/-Teile ausgenommen, sonst wandert der HTML-Body eines
  `multipart/related` in die Anhangsliste). Das allein hätte nur neu
  indizierte Mail geheilt, deshalb repariert `ensureInlineCIDAttachments`
  auch schon gespiegelte: Steht im Body einer geöffneten Nachricht ein
  `cid:`, das keine Anhangszeile beantwortet, und hat die Nachricht
  überhaupt keine Anhangszeile, werden die Teile einmal aus der
  Rohnachricht nachgetragen — synchron, solange die Nachricht unter
  `maxSyncAttachmentRepairBytes` bleibt, sonst im Hintergrund mit
  anschließendem Refresh.

Die wahrscheinlichste Ursache für diesen 404 ist inzwischen aber eine
dritte, die parallel auf `main` behoben wurde: Der Anhang-Index hat beim
Reindizieren alle Zeilen einer Nachricht gelöscht und neu eingefügt, also
jedem Anhang eine neue ID gegeben — während die geöffnete Mail die alte
noch im HTML stehen hatte („Keep attachment row IDs stable across
reindexing"). Seitdem werden Zeilen aktualisiert statt ersetzt. Diese
Zuordnung lief nach Position, was voraussetzt, dass dieselbe Rohnachricht
immer zu denselben Teilen in derselben Reihenfolge parst — genau das ändert
`isCIDReferencedPart` einmalig: Aus einer Zeile für die Rechnung werden ein
Bild an erster und die Rechnung an zweiter Stelle, und die alte URL läge
plötzlich auf dem Bild. Deshalb ordnet `ReplaceAttachmentsForMessage` Teile
jetzt zuerst über Content-ID und Dateiname ihren Zeilen zu und erst danach
der Reihe nach.

Offen bleibt der Fall, dass eine Anhangszeile zwar existiert, sich in der
geparsten Rohnachricht aber nicht wiederfinden lässt. Ob Anhang 48027 einer
davon war, sagt das Serverlog beim nächsten Auftreten.
