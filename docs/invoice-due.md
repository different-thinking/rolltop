# Rechnungs-Fälligkeiten

Stand: 2026-08-28. Umgesetzt; dieses Dokument beschreibt, was gebaut wurde und
warum es so entschieden ist. Die Untersuchung, aus der es hervorging, steht
weiter unten unverändert — sie erklärt den Bauplan, dem der Code folgt.

Die Frage: Pakete werden aus der Post mitgelesen und als eigene Übersicht mit
Erinnerung ("kommt heute") geführt. Lässt sich derselbe Bau für Rechnungen
wiederholen — mit dem Ziel, erinnert zu werden, wenn eine Rechnung fällig ist,
und zwar **nur** für Rechnungen, die noch zu überweisen sind? Bereits bezahlte
(PayPal, Lastschrift, Kreditkarte) dürfen nicht erinnern, auch wenn trotzdem
noch eine Rechnung als PDF kommt. Zahlungserinnerungen und Mahnungen gehören
mit dazu — als das dringlichste, was die Liste kennt, und auch dann, wenn die
ursprüngliche Rechnung nie als Mail ankam.

Kurzantwort: Ja, der Bauplan der Pakete passt fast eins zu eins. Zwei Dinge
sind wirklich neu — die Bezahlt-Erkennung und das Lesen des PDF-Anhangs — und
das zweite ist billiger als es aussieht, weil die Infrastruktur dafür schon im
Haus ist: `Attachment.SearchableText()` jagt PDF-Anhänge für die Suche bereits
durch `pdftotext` (poppler-utils sind im Image).

## Was tatsächlich gebaut wurde

| Datei | Rolle |
| --- | --- |
| `backend/mailparse/dates.go` | Die Datums-Maschinerie, aus `delivery.go` herausgezogen und jetzt geteilt. Pakete und Rechnungen lesen dieselben Schreibweisen; nur die Ankerwörter und die Fensterbreite gehören dem jeweiligen Feature. Das Verhalten der Paket-Extraktion ist dabei unverändert geblieben. |
| `backend/mailparse/invoice_due.go` | Der Extraktor: Fälligkeit, Betrag, Nummer, Zahlungsweg, Mahnstufe, Status. |
| `backend/mailparse/invoice_structured.go` | XRechnung und ZUGFeRD/Factur-X, gelesen über die lokalen Elementnamen statt über zwei modellierte Schemata. |
| `backend/mailparse/pdf_embedded.go` | Das eingebettete XML aus einem hybriden PDF bergen — ohne PDF-Parser, durch begrenztes Scannen der Streams. |
| `backend/store/invoices.go` | Eine Zeile pro Rechnung, die Mails daran, der Upsert und die Leseabfragen. |
| `backend/syncer/message_invoices.go` | Der Backfill über gespeicherte Post. |
| `backend/web/api_invoices.go` | `/api/invoices`, `/api/invoices/due`, die Korrekturen. |
| `frontend/src/features/invoices/` | Die Ansicht, der Header-Chip, die Pille an der Mailzeile. |

### Die vier Entscheidungen, die das Verhalten bestimmen

**1. Bezahlt schlägt zu überweisen, Mahnung schlägt beides.** Eine Erinnerung
für Geld, das niemand schuldet, steht auf jeder Seite, die der Leser öffnet, und
lässt sich nur von Hand wegräumen; eine verpasste Rechnung ist eine Zeile in der
Liste, die er trotzdem findet. Die Reihenfolge in `invoiceStatus` folgt genau
dieser Asymmetrie. Die eine Ausnahme ist die Mahnung: wer schreibt, dass das Geld
nie ankam, hat die Frage entschieden — egal, was auf der ursprünglichen Rechnung
über den Zahlungsweg stand.

**2. Zahlungsarten zählen nur neben einem Anker.** Jeder Shop-Footer listet
PayPal, Kreditkarte und Lastschrift als das, was er akzeptiert. Ein Wort allein
sagt also nichts; erst „Zahlungsart:", „bezahlt mit", „paid via" davor macht es
zur Aussage über *diese* Rechnung. Ohne diese Regel hätte ein einziger Footer
jede echte Erinnerung im Postfach verstummen lassen. Innerhalb des Fensters
gewinnt die Methode, die dem Anker am nächsten steht — nicht eine feste
Rangfolge, sonst liest „Zahlungsart: PayPal. Wir akzeptieren außerdem …
Lastschrift" als Lastschrift.

**3. Mahnstufen kommen aus dem Betreff, nicht aus dem Body.** Auf der Rückseite
jeder zweiten Rechnung steht „bei Zahlungsverzug werden Mahngebühren fällig".
Aus dem Body gelesen wäre damit jede Rechnung eine Mahnung. Eine Mahnung sagt im
Betreff, was sie ist — das ist der Zweck, sie zu verschicken. Aus dem Body werden
nur ein paar unmissverständliche Sätze gelesen („konnten wir bis heute keinen
Zahlungseingang feststellen"), die kein Kleingedrucktes verwendet. Spiegelbildlich
wird „bereits bezahlt" ignoriert, wenn ein „falls" oder „sollten" davorsteht —
das ist die Entlastungsklausel am Fuß jedes Mahnschreibens.

**4. Ein abgeschnittener Scan darf nie Alarm auslösen.** Der Backfill liest
gespeicherte Post mit einem Byte-Budget. Bricht er ab, kann genau die Seite
gefehlt haben, auf der steht, dass abgebucht wurde. Also darf ein unvollständiger
Durchlauf eine Rechnung als *erledigt* melden — das schließt höchstens eine Zeile
— aber nie als *offen*. Was dabei verloren geht, hatte der Fetch-Pfad ohnehin
vollständig in der Hand.

### Identität: warum `(issuer, reference)`

Eine Sendungsnummer hat weltweit genau ein Carrier vergeben; „2026-001" vergibt
jeder zweite Absender im Postfach jeden Januar. Der Schlüssel ist deshalb die
Absender-Domäne plus eine Referenz — und zwar die registrierbare Domäne, damit
`billing.firma.de` und `mail.firma.de` derselbe Aussteller sind. Die Referenz ist
die Rechnungsnummer, wo eine stand; sonst der Betrag; sonst, nur bei Mahnungen,
der Tag der Mail. Die letzte Stufe ist bewusst grob: lieber zwei Zeilen, die
beide erinnern, als eine verschluckte Mahnung.

### Was es kostet

Der Backfill ist der einzige Durchlauf im Haus, der Anhangs-Bytes öffnet. Bezahlt
wird das durch die Auswahl: gelesen wird nur, was die Kategorie schon als
Papierkram abgelegt hat, in Stapeln zu 50 statt 200. Auf dem Fetch-Pfad kostet
das PDF nichts extra — `Attachment.SearchableText()` ist jetzt memoisiert, sodass
`pdftotext` je Anhang einmal läuft, für den Suchindex und den Rechnungsleser
gemeinsam.

### Grenzen, die bleiben

- **Kein OCR.** Eine eingescannte Rechnung ohne Textebene liefert keine Frist.
  Sie erscheint ohne Datum in der offenen Gruppe und löst keinen Alarm aus; die
  Frist lässt sich in der Zeile von Hand nachtragen.
- **Blob-Retention.** Der Backfill erreicht nur, was noch als Rohnachricht auf
  der Platte liegt — bei Voreinstellung die letzten vierzehn Tage.
- **Der Horizont.** Offene Rechnungen älter als 180 Tage fallen aus der Liste,
  damit ein Erstsync nicht zwei Jahre alter Post als offene Posten präsentiert.
  Gemahntes ist davon ausgenommen: da schreibt noch jemand.

---

Alles Folgende ist die ursprüngliche Untersuchung.


## Der Bauplan, der sich überträgt

Das Paket-Feature ist bewusst *keine* Kategorie, sondern ein Faktenspeicher:
eine Sendung ist eine Zeile, und die vier Mails, die über sie reden, hängen an
ihr (`delivery.go`, `store/shipments.go`). Genau diese Form braucht eine
Rechnung auch. Eine Rechnung wird von mehreren Mails erwähnt — der Rechnung
selbst, einer Zahlungserinnerung, einer Mahnung, einer Zahlungsbestätigung —
und die nützliche Sicht ist eine Zeile pro Rechnung mit der Post daran. Dass
*Invoices & Contracts* als Kategorie schon existiert, ist kein Widerspruch,
sondern der billige Vorfilter: die Kategorie beantwortet "ist das Papierkram",
der Rechnungssatz beantwortet "was ist damit zu tun".

Stück für Stück:

| Pakete | Rechnungen | Übertragbarkeit |
| --- | --- | --- |
| `mailparse/delivery.go`: Nummern, Anker-Fenster, Datums-Maschinerie | neues `mailparse/invoice_due.go` | Die komplette Datumserkennung (`germanDateRE`, `isoDateRE`, Monatsnamen, `plausibleDate`, Fenster um Anker) lässt sich herausziehen und teilen. Nur die Anker sind andere: "fällig am", "zahlbar bis", "Zahlungsziel", "spätestens bis", "due by", "payment due". `invoiceNumberRE` und `invoiceAmountRE` existieren schon in `category_invoices.go`. |
| `shipments` + `shipment_messages` + `delivery_version` | `invoices` + `invoice_messages` + `invoice_version` | Gleiches Schema-Muster, gleiche Migrationstechnik (`0011-shipments` als Vorlage). |
| Upsert mit `reported_at`-Wächter: die jüngere Mail gewinnt, außer Reihenfolge gelesener Backfill kann nichts zurückdrehen | identisch | Der Mechanismus, mit dem ein Zustellbericht `delivered` setzt, ist derselbe, mit dem eine Zahlungsbestätigung `paid` setzt. |
| `ManualStatus` delivered/dismissed | `ManualStatus` paid/dismissed | "Hab ich überwiesen" abhaken und "war keine Rechnung" wegräumen — wörtlich dieselben zwei Handgriffe. |
| Backfill über alle Mails mit Blob | Backfill **beschränkt auf `category = 'invoices'`** | Deutlich billiger als der Liefer-Scan: die Kategorie-Entscheidung ist schon gefallen und steht in der Datenbank; nur diese Mails werden erneut geöffnet. |
| Fetch-Pfad: `parsed.Deliveries` → `ReplaceMessageShipments` (`syncer/index.go`) | analog, nur wenn die Kategorie-Entscheidung Invoices ergab | Der Parser hat die ganze Mail samt Anhangs-Bytes in der Hand — der teure Teil (PDF-Text) fällt genau einmal an, beim Eingang. |
| `/api/deliveries` + `DeliveriesView` (überfällig / unterwegs / zugestellt) | `/api/invoices` + Ansicht (gemahnt / überfällig / fällig diese Woche / offen ohne Datum / bezahlt, 14 Tage) | Gleiche Gruppierung nach Tagen, gleiche Behandlung des Reader-Tags im Browser. |
| Header-Chip `ShipmentsExpectedOn` ("heute fällig?") | Chip "Rechnung fällig" | Ein Unterschied: ein Paket verschwindet nach dem Tag aus dem Chip, eine unbezahlte Rechnung wird *überfällig* und muss weiter zählen. Die Abfrage ist `due_date <= heute AND offen`, nicht `= heute`. |
| `ShipmentChip` an der Mailzeile | Fälligkeits-Pille an der Mailzeile | Direkt übertragbar. |

## Was neu ist, Teil 1: die Bezahlt-Erkennung

Das ist der Kern des Wunsches und hat keine Entsprechung bei Paketen. Drei
Quellen, in absteigender Verlässlichkeit:

**1. Strukturierte E-Rechnung.** XRechnung kommt als nackte XML-Datei und
ZUGFeRD/Factur-X als XML im PDF, und beide tragen genau die Felder, um die es
geht: Fälligkeitsdatum (BT-9), Zahlungsart (BT-81: Code 58 = SEPA-Überweisung
→ *ich* muss zahlen; 59 = SEPA-Lastschrift → wird eingezogen; 48/54 = Karte),
zu zahlender Betrag (BT-115 — steht dort 0,00, ist nichts zu tun), IBAN und
Verwendungszweck. Eine XRechnung-XML liest `encoding/xml` ohne neue
Abhängigkeit. Das ZUGFeRD-XML steckt als eingebettete Datei *im* PDF;
`pdftotext` liefert es nicht mit. Es zu bergen heißt, die
EmbeddedFiles-Streams des PDF zu finden und mit `compress/zlib` zu entpacken —
machbar als kleiner, begrenzter eigener Scan im Stil des Hauses, aber echte
Arbeit. Bis dahin trägt der sichtbare PDF-Text dieselben Angaben in Prosa.

**2. Der Wortlaut der Mail (und des PDF-Texts).** Zwei Wortfelder mit klaren
Rollen:

- *Bezahlt / keine Aktion:* "bereits bezahlt", "mit PayPal bezahlt", "Ihre
  Zahlung ist eingegangen", "wird von Ihrem Konto abgebucht", "per Lastschrift
  eingezogen", "payment received", "charged to your card". Dazu Absender, die
  grundsätzlich Belege schicken (paypal.de, Apple-/Google-Quittungen). Auch
  die schon vorhandenen `invoiceWords` sortieren hier vor:
  `zahlungsbestatigung`, `quittung`, `receipt`, `gutschrift` sind per
  Definition keine offenen Forderungen.
- *Zu überweisen:* IBAN plus Verwendungszweck im Text, "bitte überweisen",
  "zahlbar bis", "Zahlungsziel", ein Fälligkeitsdatum am Anker.

Grundhaltung, gleiche Asymmetrie wie bei den Kategorien: **Bezahlt-Signale
schlagen alles.** Nur eine Rechnung mit positivem Überweisungs-Signal und ohne
Bezahlt-Signal wird "offen" mit Erinnerung; alles Unklare landet sichtbar,
aber ohne Alarm, in "offen, ohne Datum". Eine falsche Erinnerung nervt bei
jeder Kopfzeile; eine still verpasste steht immer noch in der Liste.

**3. Die spätere Mail zur selben Nummer.** Eine Zahlungsbestätigung, die
dieselbe Rechnungsnummer nennt, kippt die bestehende Zeile auf `paid` — exakt
der Mechanismus, mit dem der Zustellbericht heute `delivered` setzt.
Zahlungserinnerungen und Mahnungen sind das Gegenstück und werden im nächsten
Abschnitt eigens behandelt.

Die Identität ist dabei kniffliger als bei Paketen: Sendungsnummern vergibt
weltweit genau ein Carrier, Rechnungsnummern vergibt jeder Absender selbst,
und "2026-001" kommt zweimal im Monat. Der Schlüssel muss also
`(user_id, absender-domäne, normalisierte nummer)` sein, nicht die Nummer
allein; der Betrag taugt als Plausibilitätszeuge beim Zusammenführen.

## Zahlungserinnerung und Mahnung

Beide sind nicht bloß ein Status-Update auf einer bestehenden Zeile, sondern
vollwertige Eintrittspunkte mit eigenen Regeln. Vier Dinge unterscheiden sie
von einer gewöhnlichen Rechnung:

**Sie dürfen eine Zeile anlegen, nicht nur eine erneuern.** Die ursprüngliche
Rechnung kam vielleicht per Brief, ging in einem anderen Postfach ein oder lag
vor dem Sync-Startdatum — die Mahnung ist dann die *erste* Mail, die von der
Forderung weiß. Der Upsert der Pakete kann das schon (eine Sendung entsteht
auch aus der ersten Mail, die sie nennt); es muss nur so bleiben und nicht auf
"nur bekannte Nummern" verengt werden.

**Sie sind ihr eigenes Überweisungs-Signal.** Die Grundhaltung "nur mit
positivem Überweisungs-Signal gibt es eine Erinnerung" wird hier aufgehoben:
wer mahnt, sagt damit, dass Geld aussteht. Eine erkannte Mahnung oder
Zahlungserinnerung ist immer `offen` mit Erinnerung, auch ohne IBAN und
Fälligkeitsanker im Text. Sie überstimmt auch ein früheres, schwaches
Bezahlt-Signal derselben Rechnung — der `reported_at`-Wächter regelt das von
selbst, weil die jüngere Mail gewinnt. (Die eine Ausnahme: eine
Zahlungsbestätigung, die *nach* der Mahnung eingeht, kippt wieder auf `paid` —
Mahnung und Zahlung überkreuzen sich in der Praxis oft um Tage.)

**Sie tragen eine Stufe.** Die Zeile bekommt neben dem Status eine Mahnstufe:
`0` = Rechnung, `1` = Zahlungserinnerung, `2` = Mahnung, `3` = letzte/zweite
Mahnung. Erkennung über den Wortlaut: "Zahlungserinnerung" und "Mahnung"
stehen schon in `invoiceWords` (die Kategorie fängt diese Mails also bereits
ein); dazu kommen "1./2./3. Mahnung", "letzte Mahnung", "Mahngebühr",
"in Verzug", "Verzugszinsen", "payment reminder", "overdue notice", "final
notice", "dunning". Die Stufe steigt nur und fällt nie — eine später
eingelesene ältere Mail (Backfill!) darf eine 2 nicht auf 1 zurückdrehen; das
ist derselbe Monotonie-Gedanke wie beim `reported_at`-Wächter, nur als `MAX`
statt als Datum.

**Sie ordnen die Dringlichkeit.** In der Übersicht steht Gemahntes als eigene
Gruppe zuoberst, vor Überfällig; der Header-Chip zählt gemahnte Rechnungen
immer mit, unabhängig vom Datum — eine Mahnung ohne erkennbares Fristdatum ist
trotzdem dringend, nicht "offen, ohne Datum". Die neue Frist aus der Mahnung
("bis spätestens", "innerhalb von 7 Tagen nach Erhalt") ersetzt das alte
Fälligkeitsdatum; relative Fristen rechnen gegen das Mail-Datum, genau wie
"morgen" bei den Paketen gegen `sent` rechnet.

Eine Ecke bleibt kantig: eine Mahnung, die keine lesbare Rechnungsnummer
nennt (oder nur im gescannten PDF). Dann fehlt der Schlüssel, um sie mit der
Rechnung zu verschmelzen, und sie wird eine eigene Zeile — als Rückfall-
Identität taugt `(absender-domäne, betrag)`, mit dem Risiko, zwei gleiche
Abo-Beträge zu verwechseln. Lieber zwei Zeilen, die beide erinnern, als eine
verschluckte Mahnung; zusammenführen kann der Leser von Hand, wegräumen auch.

## Was neu ist, Teil 2: ins PDF schauen

Heute liest die Klassifikation Anhangs-*Namen* und nie Bytes — das ist in
`category_content.go` als bewusste Entscheidung dokumentiert und bleibt für
die Kategorie auch richtig. Für die Fälligkeit reicht der Name aber nicht:
gerade der PayPal-Fall ("bezahlt, PDF kommt trotzdem") steht oft *nur* im PDF.

Die gute Nachricht: der teure Teil existiert schon.
`Attachment.SearchableText()` (`parser.go`) extrahiert PDF-Text über
`pdftotext` (im Docker-Image via poppler-utils), mit 10-Sekunden-Timeout und
1-MB-Textbudget, weil die Suche das Ergebnis indexiert. Der Fetch-Pfad kann
denselben Aufruf für die als Rechnung erkannten PDF-Anhänge wiederverwenden
und den Text durch dieselben Fälligkeits- und Bezahlt-Regeln schicken wie den
Mail-Body — Kosten fallen einmal beim Eingang an, und nur für Mails, die die
Kategorie-Entscheidung ohnehin als Papierkram eingestuft hat.

Der Backfill braucht mehr: `scanCategoryContent` überspringt Anhangs-Bytes
absichtlich. Nötig ist eine Variante, die aus dem gespeicherten `.eml` genau
den einen als Rechnung benannten Anhang dekodiert (mit eigenem Byte-Budget;
`maxPDFAttachmentBytes` = 32 MB als Obergrenze gibt es schon). Die
Blob-Retention von 14 Tagen ist dafür kein Problem, sondern passend:
Fälligkeiten entstehen beim Eingang, und was älter ist, holt der
On-Demand-Cache bei Bedarf vom Server nach.

Grenze, die bleibt: `pdftotext` liest keinen eingescannten Brief (kein OCR).
Solche Rechnungen erscheinen dann ohne Datum in der offenen Gruppe — ein
Miss, kein falscher Alarm, und von Hand nachtragbar.

## Die Erinnerung selbst

- **Header-Chip** wie bei den Paketen: eine schmale Abfrage
  ("wie viele offene Rechnungen sind heute oder früher fällig"), gelesen bei
  jedem Seitenaufruf und nach jedem Sync. Das deckt den Wunsch "Erinnerung
  bekommen" für jeden ab, der die App öffnet — und ist gratis zu haben.
- **Web-Push** wäre die Ausbaustufe: die Push-Infrastruktur existiert, feuert
  aber mail-getrieben (neue Post). "Heute wird Rechnung X fällig" ist ein
  zeitgetriebenes Ereignis und bräuchte einen kleinen Morgen-Anlauf im
  Wartungszyklus. Machbar, aber nicht Teil des Kerns.

## Vorschlag: drei Stufen

1. **Kern ohne PDF-Text.** Tabelle (samt Mahnstufe) + Extraktor (Betreff,
   Body, XRechnung-XML-Anhang, Mahnungs-Wortfeld), Fetch-Pfad und
   kategoriebeschränkter Backfill, `/api/invoices`, Ansicht mit den fünf
   Gruppen, Header-Chip, Pille an der Mailzeile, Abhaken/Wegräumen. Das ist
   das Paket-Feature nachgebaut und deckt jede Rechnung, Zahlungserinnerung
   und Mahnung ab, deren Mail-Body für sich spricht — die Mahnstufe gehört in
   den Kern, weil sie Schema und Upsert-Regeln prägt und ein Nachrüsten eine
   Migration plus Versions-Bump kosten würde.
2. **PDF-Text.** `SearchableText()` auf dem Fetch-Pfad für Rechnungs-PDFs
   anzapfen, Backfill-Variante, die den einen Anhang dekodiert. Ab hier
   funktioniert der PayPal-Fall auch dann, wenn nur das PDF vom Bezahltsein
   weiß.
3. **Feinschliff.** ZUGFeRD-Embedded-XML bergen (strukturierte Zahlungsart
   statt Wortlaut), Morgen-Push, ggf. Betrag in der Übersicht summieren.

## Risiken, gegen die gebaut werden muss

- **Gutschriften und Belege als offene Posten:** `gutschrift`, `quittung`,
  `zahlungsbestatigung` müssen vor der Fälligkeitslogik aussortiert sein,
  sonst erinnert die Liste an Geld, das zurückkam.
- **Bestellbestätigungen:** nennen Nummer und Betrag (und erfüllen damit
  `invoiceEvidenceDocument`), sind aber fast immer schon bezahlt. Ohne
  positives Überweisungs-Signal keine Erinnerung.
- **"Sofort fällig":** viele Rechnungen nennen kein Datum. Kein Datum raten;
  die Gruppe "offen, ohne Datum" ist die ehrliche Antwort, das Fälligkeits-
  datum ist von Hand nachtragbar (Analogie: Paket von Hand abhaken).
- **Datumsfenster:** Fälligkeiten liegen 0–90 Tage voraus; die Paket-Grenzen
  (−60/+120) sind nicht einfach zu übernehmen, sondern neu zu wählen.
