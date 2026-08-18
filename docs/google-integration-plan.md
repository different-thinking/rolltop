# Umsetzungsplan: Google-Integration

Gmail-Mailversand und -empfang über mehrere Konten, Google Kalender und Google Kontakte
als führende Systeme — aufbauend auf der bestehenden IMAP/SMTP-Architektur von Rolltop.

- Stand: 2026-08-17, Basis: Code-Analyse `rolltop@main`
- Gesamtaufwand: ca. 6–8 Wochen (Nettoschätzung für eine Person inkl. Tests)

## Leitentscheidung: IMAP + XOAUTH2 statt Gmail-API

Mail läuft weiterhin über den bestehenden IMAP/SMTP-Pfad — nur die Anmeldung wechselt von
App-Passwörtern auf OAuth (XOAUTH2). Begründung:

- Rolltops Sync-Kern ist konsequent UID/UIDVALIDITY-basiert (Generationen-Subsystem,
  ~15 Dateien) und das Datenmodell kennt genau einen Ordner pro Nachricht. Die Gmail-API
  (Labels, `historyId`-Cursor) passt darauf nur mit einem großen, riskanten Umbau der
  Lese- und Sync-Pfade.
- Die Gmail-typischen Auth-Vorteile (kein App-Passwort, zentrale Kontoverwaltung,
  Widerruf) bekommt man über XOAUTH2 identisch.
- Kalender und Kontakte laufen ohnehin über eigene Google-APIs — unabhängig vom
  Mail-Transport.

Die Gmail-API als echte Mail-Quelle bleibt eine mögliche spätere Ausbaustufe, falls
Labels oder Push-Latenz konkret gebraucht werden. Sie ist bewusst **nicht** Teil dieses
Plans.

| Phase | Inhalt | Aufwand | Hängt ab von |
|---|---|---|---|
| 0 | Google-Cloud-Projekt & OAuth-Client | ½ Tag (manuell) | — |
| 1 | OAuth-Fundament: Google-Konten verbinden, Tokens, Refresh | ~1 Woche | 0 |
| 2 | Gmail über IMAP/SMTP mit XOAUTH2, Mehrkonten-UX | ~1 Woche | 1 |
| 3 | Google Kontakte (People API), führendes System | 1–2 Wochen | 1 |
| 4 | Google Kalender inkl. neuer Kalender-Ansicht | 3–4 Wochen | 1 |

Phase 2, 3 und 4 sind nach Phase 1 unabhängig voneinander und einzeln auslieferbar.
Empfohlene Reihenfolge: 1 → 2 → 3 → 4 (schneller Nutzen zuerst, größter Brocken zuletzt).

## Phase 0: Google-Cloud-Vorbereitung (½ Tag, manuell)

Einmalige Einrichtung in der Google Cloud Console (Aufgabe des Betreibers, nicht Code):

1. Projekt anlegen, OAuth-Consent-Screen konfigurieren (App-Typ „extern" bei privatem
   Gmail-Konto).
2. OAuth-Client „Webanwendung" erstellen; Redirect-URI:
   `https://<rolltop-host>/api/google/callback`.
3. APIs aktivieren: People API, Google Calendar API. (Für IMAP/SMTP-XOAUTH2 ist keine
   API-Aktivierung nötig, nur der Scope.)
4. Scopes: `openid email`, `https://mail.google.com/` (IMAP/SMTP), `.../auth/contacts`,
   `.../auth/calendar`. Kontakte/Kalender erst anfordern, wenn die jeweilige Phase live
   geht (inkrementelle Autorisierung).

> **Entschieden — Verifizierungsstatus:** Der Gmail-Scope ist bei Google „restricted".
> Im Testmodus laufen Refresh-Tokens nach 7 Tagen ab (wöchentliches Neu-Verbinden).
> Festgelegt für den Privatbetrieb: App auf **„In Produktion"** stellen und den „Nicht
> verifizierte App"-Warnhinweis einmalig bestätigen — dann bleiben Refresh-Tokens
> dauerhaft gültig. Eine echte Google-Verifizierung (inkl. Security-Assessment) wird
> erst relevant, falls Rolltop öffentlich mit Google-Login vertrieben werden soll.

## Phase 1: OAuth-Fundament — Google-Konten verbinden (~1 Woche)

Ein zentrales Modul, an dem Mail, Kontakte und Kalender gemeinsam hängen. Ein
Rolltop-Nutzer kann **mehrere Google-Konten** verbinden; jede Verbindung hält eigene
Tokens und Scopes.

### Datenmodell

Neue Tabelle `google_connections` in der Per-User-Datenbank (Migration nach dem Muster
`migration_user_028.go`):

```
google_connections(
  id, user_id, google_email,
  encrypted_refresh_token,      -- AES-GCM via bestehendem crypto-Paket
  encrypted_access_token, access_token_expires_at,
  granted_scopes, status,       -- ok | reauth_required
  created_at, updated_at
)
```

### Backend `backend/googleauth`

- Authorization-Code-Flow mit PKCE, `access_type=offline` und `prompt=consent` (sonst
  liefert Google keinen Refresh-Token). Das OIDC-Plugin (`plugins/oidc/backend/main.go`)
  liefert ~60 % der Flow-Logik als Vorlage, hat aber weder Refresh-Token-Handling noch
  Persistenz — beides kommt neu.
- `TokenSource(connectionID)`: liefert gültigen Access-Token, refresht bei < 5 min
  Restlaufzeit; Singleflight pro Verbindung, damit parallele Sync-Worker nicht
  konkurrierend refreshen; persistiert neue Tokens verschlüsselt.
- Fehlerpfad `invalid_grant` (Widerruf, Passwortwechsel): Verbindung auf
  `reauth_required` setzen, abhängige Konten pausieren, Hinweis in der UI.
- Routen: `GET /api/google/connect` (Redirect mit state+nonce),
  `GET /api/google/callback`, `GET/DELETE /api/google/connections` — Session/
  CSRF-Anbindung wie bestehende API-Routen.

### Frontend

- Neue Settings-Sektion „Verbundene Google-Konten": Liste mit E-Mail, Scope-Badges,
  Status; Buttons „Google-Konto verbinden" / „Trennen" / „Neu autorisieren".

> **Empfehlung — Abhängigkeiten:** `golang.org/x/oauth2` als einzige neue Dependency
> (klein, löst Refresh-Semantik korrekt). Für People- und Calendar-API schlanke,
> handgeschriebene REST-Clients statt `google.golang.org/api` — es werden nur je 4–5
> Endpunkte gebraucht, und das passt zum Stil des Projekts (eigener vCard-Parser,
> eigenes OIDC).

Berührt: neu `backend/googleauth/`, `backend/store/migration_user_029.go`,
`backend/web/api_google.go`, Settings-UI. Vorlagen: `plugins/oidc/backend/main.go`,
`backend/crypto/secret.go`.

## Phase 2: Gmail über IMAP/SMTP mit XOAUTH2 (~1 Woche)

Ergebnis: „Gmail-Konto hinzufügen" mit zwei Klicks statt App-Passwort — für beliebig
viele Gmail-Konten. Der gesamte Sync-, Such- und Sende-Stack bleibt unverändert.

### Arbeitsschritte

1. **SASL-Mechanismus XOAUTH2** (je ~25 Zeilen): für go-imap als
   `sasl.Client`-Implementierung, für `net/smtp` als `smtp.Auth`-Implementierung
   (Payload: `user=…\x01auth=Bearer …\x01\x01`).
2. **Schema:** `mail_accounts` + `auth_type` (`password` | `google_oauth`) und
   `google_connection_id`; Passwortfelder bleiben für Bestandskonten unberührt.
3. **Login-Verzweigung:** im IMAP-Fetcher (`fetcher.go:1262`, heute plain `LOGIN`) und
   im SMTP-Sender (`sender.go:168`, heute `PlainAuth`): bei `google_oauth` Access-Token
   via `googleauth.TokenSource` holen und per XOAUTH2 authentifizieren. Bei Auth-Fehler
   einmalig Token force-refreshen und erneut versuchen.
4. **Konto-Anlage-UX:** Im Account-Formular Weg „Mit Google verbinden" → Auswahl/
   Neuverbindung eines Google-Kontos → `imap.gmail.com:993` / `smtp.gmail.com:465` und
   Nutzername werden automatisch gesetzt, Passwortfelder entfallen.
5. **Gmail-Ordner-Defaults:** Beim Anlegen eines Gmail-Kontos `[Gmail]/All Mail` und
   `[Gmail]/Important` standardmäßig vom Sync ausnehmen (Duplikat-Vermeidung — jede
   Mail liegt sonst doppelt vor, da das Datenmodell einen Ordner pro Nachricht hat).
   Spam/Trash auf „manuell".
6. **Tests:** Fake-Token-Server + Fake-IMAP für Auth-Verzweigung;
   Tenant-Isolation-Tests gemäß `AGENTS.md` aktuell halten.

> **Empfehlung — Kombination mit Sync-Startdatum:** Das geplante „Sync ab
> Datum"-Feature (Seek-Variante) hier direkt mit ausliefern und im Gmail-Anlage-Dialog
> prominent vorschlagen (z. B. Default „letzte 2 Jahre"). Das verhindert bei großen
> Gmail-Postfächern den mehrstündigen Initial-Sync und hält die Datenbank aus dem
> kritischen Größenbereich.

Berührt: `backend/imapclient/fetcher.go`, `backend/smtpclient/sender.go`,
`backend/store/` (Migration + 3 duplizierte Spaltenlisten in `mailboxes.go`),
`backend/web/api_account.go`, `frontend/src/features/settings/SettingsViews.tsx`,
`frontend/src/lib/accountForm.ts`.

## Phase 3: Google Kontakte als führendes System (1–2 Wochen)

Rolltops Kontaktmodell ist bereits vCard-förmig (Namen, E-Mails, Telefone, Adressen,
Organisation, Geburtstag, URLs, Foto) und besitzt fertige Merge-Logik — das passt fast
1:1 auf die People API. Was fehlt, ist Herkunft und Sync-Zustand.

### Datenmodell

```
contacts        + source ('local'|'google'), google_connection_id,
                  external_id (People resourceName), etag, remote_updated_at
google_people_sync(connection_id, sync_token, last_sync_at, status)
```

### Sync-Loop (Polling, da die People API kein Push bietet)

1. **Initial:** `people.connections.list` mit `requestSyncToken=true`, paginiert;
   Mapping in das bestehende Modell, Fotos in `contact_icons`.
2. **Verknüpfung Bestand:** vorhandene lokale Kontakte per normalisierter E-Mail matchen
   (vorhandenes `findImportMergeTarget` wiederverwenden) und zu Google-Kontakten
   hochstufen statt Duplikate anzulegen.
3. **Inkrementell:** alle 15 min + manuell per Button, mit `syncToken`; gelöschte
   Einträge kommen als `metadata.deleted`. Bei HTTP 410 (abgelaufener Token)
   Voll-Resync.
4. **Rückschreiben:** lokale Bearbeitung → `people.updateContact` (mit `etag`, bei
   Konflikt Remote-Stand übernehmen und Hinweis zeigen); Neuanlage → `createContact`.
   Google gewinnt bei Konflikten — es ist das führende System.

### UI

- Kontaktliste: Quelle-Badge (Google-Konto) und Filter; Sync-Status und „Jetzt
  synchronisieren" in den Einstellungen.
- Autocomplete beim Verfassen und Such-Ranking profitieren automatisch — beide speisen
  sich aus der Kontakttabelle.

> **Entschieden — Löschpolitik:** Voll bidirektional: Löschen eines Google-Kontakts in
> Rolltop **löscht ihn auch bei Google, mit Bestätigungsdialog**. Alles andere würde bei
> einem führenden System schwer erklärbare Zombie-Zustände erzeugen.

Berührt: `backend/store/contacts.go` (924 Zeilen, gut wiederverwendbar),
`backend/web/api_contacts.go`, neu `backend/googlepeople/`,
`frontend/src/features/contacts/ContactsView.tsx`.

## Phase 4: Google Kalender (3–4 Wochen)

Komplett neues Feature — es existiert null Kalender-Code (kein ICS-Parsing, keine
Ansicht, kein Datenmodell). Der größte Einzelposten ist die Frontend-Kalenderansicht.

> **Stand:** Der MVP-Schnitt ist umgesetzt — Datenmodell (`user-033`),
> `backend/googlecalendar` (REST-Client, Sync, Rückschreiben), `/api/calendar`,
> die Wochenansicht mit Mehrkalender-Overlay unter `/calendar` und der
> Termin-Dialog inkl. Einladungsantwort. Polling alle 5 Minuten. Offen bleiben
> die als „zweiter Schritt" markierten Monats- und Agenda-Ansicht, Drag&Drop und
> die Ausbaustufe „Einladungen aus E-Mails".
>
> Zwei Festlegungen, die im Plan noch offen waren:
> - **Ganztägige Termine** werden auf Mitternacht **UTC** verankert. Ein
>   Datum ohne Zeitpunkt an die Zone des Betrachters zu binden verschiebt einen
>   Feiertag für alle westlich davon auf den Vortag.
> - **Beim Trennen eines Google-Kontos werden Kalender gelöscht**, anders als
>   Kontakte. Ein Kalender ist ein reiner Spiegel ohne lokalen Editor; ein
>   zurückgelassener könnte nie wieder synchronisieren, nie bearbeitet und nie
>   wieder ausgeblendet werden.

### Datenmodell

```
calendars(id, google_connection_id, google_calendar_id, name,
          color, access_role, selected, sync_token, last_sync_at)
calendar_events(id, calendar_id, external_id, etag, ical_uid,
          summary, description, location, status,
          start_unix, end_unix, all_day, timezone,
          recurring_event_id, organizer, attendees_json,
          my_response, updated_remote_at)
```

### Sync

1. `calendarList.list` → alle Kalender aller verbundenen Konten; Nutzer wählt sichtbare
   Kalender ab.
2. Pro Kalender: Initial-Sync `events.list` mit `singleEvents=true` und Zeitfenster
   (z. B. −1 Jahr), danach inkrementell per `syncToken` (das Fenster ist im Token
   kodiert); 410 → Voll-Resync. Wiederholungstermine kommen dank `singleEvents` als
   fertige Instanzen — kein RRULE-Expander nötig.
3. Polling alle 5–15 min; optional später Push via `events.watch`-Channels (braucht
   öffentlich erreichbaren HTTPS-Endpunkt; Muster `ROLLTOP_WEBHOOK_TOKEN` existiert).
4. Schreiben: Termin anlegen/ändern/löschen und Einladungsantwort (eigener
   `attendee.responseStatus` per `events.patch`) direkt gegen die API, lokale Kopie
   optimistisch aktualisieren.

### Frontend (der Hauptaufwand, ~2 Wochen davon)

- **Mehrkalender ist Kernanforderung, kein Extra:** alle Kalender aller verbundenen
  Google-Konten werden gleichzeitig überlagert dargestellt — Kalenderfarben aus Google,
  Sichtbarkeits-Schalter je Kalender in der Seitenleiste, Zielkalender-Auswahl in jedem
  Termin-Dialog. Das Datenmodell ist von Anfang an mehrkalenderfähig.
- **MVP-Schnitt (festgelegt): Wochenansicht zuerst.** Zeitraster-Spaltenlayout mit
  Überlappungs-Layout paralleler Termine, Ganztägig-Leiste, „Jetzt"-Linie,
  Mehrkalender-Overlay ab dem ersten Tag. Die Wochenansicht ist die aufwendigste der
  drei Ansichten — sie zuerst zu bauen zieht den Hauptaufwand nach vorn, danach sind
  Monats- und Agenda-Ansicht vergleichsweise klein.
- Termin-Dialog: Titel, Zeit/ganztägig, Ort, Beschreibung, Teilnehmer (mit
  Kontakt-Autocomplete aus Phase 3), Zielkalender.
- Zweiter Schritt: Monats- und Agenda-Ansicht, dann Drag&Drop in der Wochenansicht.

### Ausbaustufe: Einladungen aus E-Mails (+3–4 Tage)

- `text/calendar`-MIME-Teile beim Parsen erkennen (`backend/mailparse`),
  Einladungskarte im Thread anzeigen (Termin, Teilnehmer, Konfliktanzeige gegen lokale
  Events).
- Annehmen/Ablehnen: Event per `ical_uid` in den gesyncten Kalendern finden und RSVP
  setzen; Gmail legt eingehende Einladungen ohnehin automatisch im Kalender an, daher
  reicht das Matching.

Berührt: neu `backend/googlecalendar/`, `backend/store/migration_user_03x.go`,
`backend/web/api_calendar.go`, neu `frontend/src/features/calendar/`,
`frontend/src/RouteView.tsx`; Ausbaustufe zusätzlich `backend/mailparse/parser.go`,
`ThreadView`.

## Querschnittsthemen

- **Sicherheit:** Refresh-/Access-Tokens ausschließlich verschlüsselt (bestehendes
  AES-GCM mit `ROLLTOP_MASTER_KEY`); Tokens nie loggen (bestehende Regel für Passwörter
  erweitern); Scopes minimal und inkrementell anfordern; alle neuen Routen hinter
  Session + CSRF wie gehabt.
- **Mandantentrennung:** alle neuen Tabellen tragen `user_id` und leben in der
  Per-User-DB; Isolation-Tests gemäß `AGENTS.md` für jede neue Route und jeden
  Sync-Pfad.
- **Fehler-UX:** ein gemeinsamer „Google-Verbindung benötigt Aufmerksamkeit"-Zustand
  (Banner + Settings-Badge), der Mail-Sync, Kontakte und Kalender einer Verbindung
  gleichzeitig pausiert und gezielt zur Re-Autorisierung führt.
- **Rate Limits:** People/Calendar-Quotas sind für Einzelnutzer unkritisch; trotzdem
  Backoff bei 429/5xx in die REST-Clients einbauen (einmal zentral im
  `googleauth`-HTTP-Client).
- **AGENTS.md aktualisieren:** die Datei widerspricht dem Ist-Zustand (verbietet
  SMTP/Moves, die längst existieren) — vor Beginn korrigieren, damit Agent-Sessions
  nicht fehlgeleitet werden.

## Entscheidungen im Überblick

| Frage | Optionen | Status |
|---|---|---|
| Verifizierung | Testmodus / Produktion unverifiziert / Voll-Verifizierung | **Entschieden:** Produktion unverifiziert (Privatbetrieb) |
| Kontakte löschen | bidirektional / nur lokal | **Entschieden:** bidirektional mit Bestätigung |
| Kalender-MVP | Wochenansicht zuerst / Monat+Agenda zuerst | **Entschieden:** Wochenansicht + Mehrkalender-Overlay zuerst, Monat/Agenda danach — Wochenansicht ausgeliefert |
| Ganztägige Termine | Zone des Betrachters / UTC-Anker | **Entschieden:** UTC-Mitternacht, überall in UTC formatiert |
| Kalender beim Trennen | behalten wie Kontakte / löschen | **Entschieden:** löschen (reiner Spiegel ohne lokalen Editor) |
| Dependencies | `x/oauth2` + eigene REST-Clients / `google.golang.org/api` | Empfehlung: `x/oauth2` + eigene schlanke Clients |
| Kalender-Push | Polling / `events.watch`-Webhooks | Empfehlung: Polling zuerst, Webhooks später |
| Einbau-Ort | Core / Plugin-System | Empfehlung: Core (Auth & Mail zwingend; Kontakte/Kalender der Konsistenz wegen ebenfalls) |
