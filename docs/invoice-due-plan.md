# Rechnungs-Fälligkeiten: was sich vom Paket-Feature übertragen lässt

Stand: 2026-08-28. Untersuchung, kein Umsetzungsprotokoll.

Die Frage: Pakete werden aus der Post mitgelesen und als eigene Übersicht mit
Erinnerung ("kommt heute") geführt. Lässt sich derselbe Bau für Rechnungen
wiederholen — mit dem Ziel, erinnert zu werden, wenn eine Rechnung fällig ist,
und zwar **nur** für Rechnungen, die noch zu überweisen sind? Bereits bezahlte
(PayPal, Lastschrift, Kreditkarte) dürfen nicht erinnern, auch wenn trotzdem
noch eine Rechnung als PDF kommt.

Kurzantwort: Ja, der Bauplan der Pakete passt fast eins zu eins. Zwei Dinge
sind wirklich neu — die Bezahlt-Erkennung und das Lesen des PDF-Anhangs — und
das zweite ist billiger als es aussieht, weil die Infrastruktur dafür schon im
Haus ist: `Attachment.SearchableText()` jagt PDF-Anhänge für die Suche bereits
durch `pdftotext` (poppler-utils sind im Image).

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
| `/api/deliveries` + `DeliveriesView` (überfällig / unterwegs / zugestellt) | `/api/invoices` + Ansicht (überfällig / fällig diese Woche / offen ohne Datum / bezahlt, 14 Tage) | Gleiche Gruppierung nach Tagen, gleiche Behandlung des Reader-Tags im Browser. |
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
der Mechanismus, mit dem der Zustellbericht heute `delivered` setzt. Eine
Mahnung oder Zahlungserinnerung zur selben Nummer hält die Zeile offen und
darf das Datum erneuern.

Die Identität ist dabei kniffliger als bei Paketen: Sendungsnummern vergibt
weltweit genau ein Carrier, Rechnungsnummern vergibt jeder Absender selbst,
und "2026-001" kommt zweimal im Monat. Der Schlüssel muss also
`(user_id, absender-domäne, normalisierte nummer)` sein, nicht die Nummer
allein; der Betrag taugt als Plausibilitätszeuge beim Zusammenführen.

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

1. **Kern ohne PDF-Text.** Tabelle + Extraktor (Betreff, Body,
   XRechnung-XML-Anhang), Fetch-Pfad und kategoriebeschränkter Backfill,
   `/api/invoices`, Ansicht mit den vier Gruppen, Header-Chip, Pille an der
   Mailzeile, Abhaken/Wegräumen. Das ist das Paket-Feature nachgebaut und
   deckt jede Rechnung ab, deren Mail-Body die Fälligkeit nennt.
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
