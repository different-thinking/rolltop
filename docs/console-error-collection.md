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
