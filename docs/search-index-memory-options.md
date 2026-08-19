# Der Suchindex und der Speicher: Optionen ohne mehr RAM

Ausgangslage (19.08.2026): 4 GiB Container, Bleve-Index 836,3 MiB, Heap-Decke
80 % = 3,2 GiB, also 819,2 MiB Rest für alles außerhalb des Heaps. Der Index
passt nicht mehr in diesen Rest, der Kernel wirft seine Seiten raus, ein
Commit von 232 KB lief über zwei Minuten und löste den Neustart um 17:06 aus.

`ROLLTOP_MEMORY_LIMIT` senken verschiebt die Grenze, verkleinert aber weder
den Index noch die Ursache. Diese Notiz sammelt die Alternativen. Nichts davon
ist umgesetzt.

## 1. Das `_all`-Feld abschalten (größter Hebel im Index)

`textField()` (`backend/search/search.go:537`) setzt `Store` und `DocValues`
auf false, lässt aber `IncludeInAll` auf dem Bleve-Standard `true`. Damit
landet jedes Textfeld ein zweites Mal im zusammengesetzten Feld `_all`: der
Body (bis 1 MiB), der Anhangstext (bis 1 MiB), `compound` (bis 128 KiB) und
sämtliche Kopfzeilen. Die Postings dafür existieren doppelt.

Gebraucht wird `_all` an genau einer Stelle: `domainTextQuery`
(`search.go:2528`) baut `termConjunction("", terms)` mit Boost 1 neben
`from_domain` mit Boost 15 — ein schwacher Fallback. Alle anderen Abfragen
setzen ihr Feld explizit (`SetField`), die Fuzzy-Suche geht auf `compound`
(`search.go:2023`).

Änderung: `IncludeInAll = false` in `textField()`, und den Fallback in
`domainTextQuery` auf `compound` umbiegen. Wirkung: der Index sollte grob auf
die Hälfte fallen — zu messen, nicht zu glauben. Preis: Domains, die tief in
einem langen Body jenseits der 32 KiB stehen, die `compound` davon aufnimmt,
werden vom schwachen Fallback nicht mehr gefunden.

Achtung: Bleve schreibt das Mapping beim Anlegen fest. Ein bestehender Index
übernimmt die Änderung nicht — jeder Tenant muss einmal neu aufgebaut werden.

## 2. Den Index auf eine lokale Platte legen (keine Codeänderung)

`ROLLTOP_INDEX_PATH` (`backend/config/config.go:106`) trennt den Indexpfad
vom Datenverzeichnis; aktuell zeigen beide auf `/data`. Der Kommentar in
`cmd/rolltop/main.go:1243` benennt den Verstärker der Lage genau: auf einem
FUSE-gestützten Volume wird aus einem Commit von wenigen Kilobyte einer, der
Minuten läuft. Die Deployment-Adresse (`hpr-…svc`) deutet auf Kubernetes mit
einem netzgestützten PVC.

Zu prüfen wäre zuerst, worauf `/data` tatsächlich liegt (`mount | grep /data`,
Storage-Klasse des PVC). Liegt der Index auf Netzspeicher, ist das der
eigentliche Grund, warum aus Paging ein Stillstand wird. Der Index ist aus
PostgreSQL wiederherstellbar, braucht also kein repliziertes Volume — lokale
SSD oder ein Node-Volume genügt, Preis ist ein Rebuild nach Node-Wechsel.

## 3. Weniger indizieren

`backend/search/document_limits.go:14` erlaubt pro Nachricht 1 MiB Body und
1 MiB Anhangstext. Beides sind Obergrenzen, keine Normalfälle, aber sie
bestimmen den Schwanz der Verteilung.

- Anhangstext optional machen (pro Konto oder global). Wer viele PDFs
  archiviert, bezahlt ihn heute im Index mit.
- `maxIndexedBodyBytes` senken. 1 MiB Text pro Mail ist viel; der Nutzen
  jenseits der ersten paar hundert Kilobyte ist gering.
- Alte Nachrichten nur mit Kopfzeilen indizieren. Kostet Volltext im Archiv.

## 4. Bleve durch PostgreSQL-Volltextsuche ersetzen (strategisch)

Die relationalen Daten liegen seit der Migration in PostgreSQL. Eine
`tsvector`-Spalte mit GIN-Index würde das gesamte Bleve-Subsystem überflüssig
machen: kein mmap, das mit dem Heap um denselben cgroup konkurriert, kein
Stall-Wächter, keine Quarantäne, keine Recovery-Marker, kein Neustart zur
Reparatur, keine Instanz-Sperre wegen Ein-Prozess-Index. PostgreSQL verwaltet
seinen eigenen Puffer und kommt mit Indizes größer als der RAM zurecht.

Der Preis ist ehrlich zu nennen: das ist der größte Umbau der Liste. Rund
9.800 Zeilen in `backend/search` hängen daran, samt Fuzzy-Ranking,
Compound-Wort-Suche für Deutsch und Ähnlichkeitssuche. Manches davon lässt
sich in PostgreSQL nachbauen, nicht alles gleich gut.

## 5. Was zuerst zu messen ist

- `mount` und Storage-Klasse für `/data` — entscheidet, ob Option 2 die
  Ursache trifft oder nur ein Symptom.
- `pgmajfault` und `workingset_refault_file` in `/sys/fs/cgroup/memory.stat`,
  `max` in `memory.events` — zeigen das Thrashing direkt.
- `memory.current` gegen die Heap-Decke — sagt, ob der Heap überhaupt in die
  Nähe der 3,2 GiB kommt. Bisher weiß das niemand; die App misst es nicht.
- Für Option 1: einen Tenant-Index testweise ohne `_all` neu aufbauen und die
  Verzeichnisgrößen vergleichen.
