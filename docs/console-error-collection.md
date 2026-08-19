# Gesammelte Browser-Konsolenmeldungen

Reine Sammlung von Meldungen, die beim Benutzen der App in der
Browser-Konsole auftauchen. Hier wird nichts bewertet und nichts behoben —
die Einträge liegen hier, bis jemand sie sich vornimmt.

Format je Eintrag: Datum, Browser, Meldung im Wortlaut, ggf. eine Zeile
Kontext dazu, wo im Code sie herkommt.

## 2026-08-19 — Edge

```
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

## 2026-08-19 — Sync aus den Einstellungen, 409

```
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

```
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

## 2026-08-19 — Event-Stream bricht ab (ERR_HTTP2_PROTOCOL_ERROR)

```
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

```
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
