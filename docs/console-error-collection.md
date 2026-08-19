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
