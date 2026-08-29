/**
 * File overview: The invoice list. What the mail says is still owed, grouped by
 * the only question a reader actually has about a bill: does it need paying,
 * and how soon.
 *
 * It is a view of invoices, not of mail, which is why it is not a mailbox. One
 * bill is talked about by the invoice, a reminder, a dunning letter and the
 * payment confirmation; as a mail list that is four rows in two folders, and as
 * this list it is one row with the mail behind it.
 *
 * The grouping differs from the parcel list's in one way, and it is the whole
 * difference between the two features. A parcel that did not arrive stops being
 * today's news; an invoice that was not paid becomes more pressing, and one
 * somebody is chasing is the most pressing thing on the page whatever its date
 * says. So "Gemahnt" is a group of its own and it comes first.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "../../api";
import type { AddToast, DatePrefs } from "../../appTypes";
import type { Invoice } from "../../types";
import { Icon } from "../../components/Icon";
import { messageURL } from "../../lib/routes";
import { messageFromError } from "../../lib/errors";
import { displayDateTime } from "../../lib/format";
import { localDay } from "../deliveries/DeliveriesView";
import { notifyInvoicesChanged } from "./revision";

/** invoiceIsOpen is money the reader still has to send. It is asked in enough
 * places to be worth naming once. */
export function invoiceIsOpen(item: Invoice): boolean {
  return item.status === "open";
}

/** invoicesDueOn is the bills that want paying on one day: due today or earlier,
 * plus everything being chased whatever its date. It is what the header chip
 * counts, so it is defined once and read from both.
 *
 * The "or earlier" is the point. An overdue bill is not yesterday's problem the
 * way a late parcel is -- it is today's, and more so than one due today. */
export function invoicesDueOn(invoices: readonly Invoice[], day: string): Invoice[] {
  return invoices.filter((item) => invoiceIsOpen(item)
    && (item.dunning_level > 0 || (item.due_date !== "" && item.due_date <= day)));
}

export type InvoiceGroup = { key: string; title: string; hint: string; items: Invoice[] };

/** groupInvoices splits the list the way a reader works down it: what somebody
 * is chasing, then what is late, then today, then the rest. */
export function groupInvoices(invoices: readonly Invoice[], today: string): InvoiceGroup[] {
  const open = invoices.filter(invoiceIsOpen);
  const chased = open.filter((item) => item.dunning_level > 0);
  // Every group below excludes the chased ones, so a dunned invoice appears
  // exactly once and always at the top.
  const rest = open.filter((item) => item.dunning_level === 0);
  const overdue = rest.filter((item) => item.due_date !== "" && item.due_date < today);
  const dueToday = rest.filter((item) => item.due_date === today);
  const upcoming = rest.filter((item) => item.due_date !== "" && item.due_date > today);
  const undated = rest.filter((item) => item.due_date === "");
  const paid = invoices.filter((item) => !invoiceIsOpen(item));
  return [
    { key: "chased", title: "Gemahnt", hint: "Es wurde bereits angemahnt oder erinnert", items: chased },
    { key: "overdue", title: "Überfällig", hint: "Die Frist ist verstrichen", items: overdue },
    { key: "today", title: "Heute fällig", hint: "Heute ist der letzte Tag", items: dueToday },
    { key: "upcoming", title: "Offen", hint: "Mit einer Frist in der Zukunft", items: upcoming },
    { key: "undated", title: "Ohne Frist", hint: "Keine Frist erkannt -- du kannst sie selbst eintragen", items: undated },
    { key: "paid", title: "Erledigt", hint: "Bezahlt, abgebucht oder gutgeschrieben", items: paid }
  ].filter((group) => group.items.length > 0);
}

const weekdayNames = ["Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"];

/** dueLabel names a deadline the way a reader would say it. It differs from the
 * parcel list's in that days in the past are counted rather than merely named:
 * "seit 9 Tagen" is what a reader needs to know about an overdue bill, where
 * "Dienstag" would leave them doing the arithmetic. */
export function dueLabel(day: string, today: string): string {
  if (day === "") return "Keine Frist genannt";
  const date = new Date(`${day}T00:00:00`);
  const now = new Date(`${today}T00:00:00`);
  if (Number.isNaN(date.getTime()) || Number.isNaN(now.getTime())) return day;
  const distance = Math.round((date.getTime() - now.getTime()) / 86400000);
  if (distance === 0) return "Heute fällig";
  if (distance === 1) return "Morgen fällig";
  if (distance === -1) return "Seit gestern überfällig";
  if (distance < -1) return `Seit ${-distance} Tagen überfällig`;
  const written = date.toLocaleDateString("de-DE", { day: "2-digit", month: "2-digit", year: "numeric" });
  if (distance < 7) return `${weekdayNames[date.getDay()]}, ${written}`;
  return `Fällig am ${written}`;
}

/** formatAmount renders the stored "1234.56" the way a German reader writes it.
 * The amount is stored normalized so two spellings of one sum compare equal;
 * this is the other end of that. */
export function formatAmount(amount: string, currency: string): string {
  if (amount === "") return "";
  const value = Number(amount);
  if (!Number.isFinite(value)) return amount;
  try {
    return value.toLocaleString("de-DE", {
      style: currency ? "currency" : "decimal",
      currency: currency || undefined,
      minimumFractionDigits: 2
    });
  } catch {
    // An unknown currency code is a sender's typo, not a reason to show nothing.
    return `${value.toLocaleString("de-DE", { minimumFractionDigits: 2 })} ${currency}`.trim();
  }
}

/** dunningLabels name the four grades. The scale is what a reader needs to tell
 * apart rather than what German dunning practice distinguishes. */
export const dunningLabels: Record<number, string> = {
  1: "Zahlungserinnerung",
  2: "Mahnung",
  3: "Letzte Mahnung"
};

export function dunningLabel(level: number): string {
  return dunningLabels[level] || "Gemahnt";
}

/** settlementLabels say why a bill counts as settled, which is the difference
 * between "you paid this" and "you never had to". */
const settlementLabels: Record<string, string> = {
  transfer: "Überweisung",
  direct_debit: "Lastschrift",
  card: "Karte",
  wallet: "Bezahldienst"
};

export function InvoicesView({
  csrf,
  datePrefs,
  mailGeneration,
  navigate,
  addToast
}: {
  csrf: string;
  datePrefs: DatePrefs;
  /** Bumps when a sync stores messages, which is when a bill can change. */
  mailGeneration: number;
  navigate: (url: string) => void;
  addToast: AddToast;
}) {
  const [invoices, setInvoices] = useState<Invoice[] | null>(null);
  const [error, setError] = useState("");
  const [openRows, setOpenRows] = useState<Set<number>>(() => new Set());
  const [busyRows, setBusyRows] = useState<Set<number>>(() => new Set());
  // The day is read once per load rather than per render: a tab left open over
  // midnight should not silently re-group under the reader while they scroll.
  const [today] = useState(() => localDay());

  const refresh = useCallback(async () => {
    try {
      const next = await api.invoices(today);
      setInvoices(next.invoices || []);
      setError("");
    } catch (err) {
      setError(messageFromError(err));
    }
  }, [today]);

  useEffect(() => {
    void refresh();
  }, [refresh, mailGeneration]);

  const groups = useMemo(() => groupInvoices(invoices || [], today), [invoices, today]);
  const open = useMemo(() => (invoices || []).filter(invoiceIsOpen), [invoices]);
  const dueNow = useMemo(() => invoicesDueOn(invoices || [], today).length, [invoices, today]);

  // The corrected row is replaced in place rather than the list refetched: the
  // answer the server returns is the whole change, and a refetch would regroup
  // the list under the reader's hand while they work down a backlog -- which is
  // exactly when this is used.
  function applyResult(id: number, result: Invoice, drop: boolean) {
    setInvoices((current) => {
      const rows = current || [];
      if (drop) return rows.filter((item) => item.id !== id);
      return rows.map((item) => (item.id === id ? { ...result, messages: item.messages } : item));
    });
    // The header chip reads its own count and cannot see this list's state.
    notifyInvoicesChanged();
  }

  async function withBusyRow(id: number, work: () => Promise<void>) {
    setBusyRows((current) => new Set(current).add(id));
    try {
      await work();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusyRows((current) => {
        const next = new Set(current);
        next.delete(id);
        return next;
      });
    }
  }

  async function correct(invoice: Invoice, manualStatus: Invoice["manual_status"]) {
    await withBusyRow(invoice.id, async () => {
      const result = await api.setInvoiceManualStatus(csrf, invoice.id, manualStatus);
      applyResult(invoice.id, result.invoice, manualStatus === "dismissed");
      if (manualStatus === "paid") addToast("Als bezahlt vermerkt.");
      if (manualStatus === "dismissed") addToast("Nicht mehr als Rechnung geführt.");
      if (manualStatus === "") addToast("Vermerk zurückgenommen.");
    });
  }

  async function setDueDate(invoice: Invoice, day: string) {
    await withBusyRow(invoice.id, async () => {
      const result = await api.setInvoiceDueDate(csrf, invoice.id, day);
      applyResult(invoice.id, result.invoice, false);
      addToast(day ? "Frist eingetragen." : "Frist entfernt.");
    });
  }

  function toggleRow(id: number) {
    setOpenRows((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  return (
    <>
      <div className="content-head">
        <div>
          <h1>Rechnungen</h1>
          <span className="label-pill">{open.length.toLocaleString()} offen</span>
          {dueNow > 0 ? (
            <span className="label-pill invoices-pill-due">{dueNow.toLocaleString()} fällig</span>
          ) : null}
        </div>
      </div>

      {error ? <div className="error">{error}</div> : null}

      {invoices !== null && groups.length === 0 && !error ? (
        <section className="panel invoices-idle">
          <Icon name="receipt" />
          <div>
            <strong>Keine offenen Rechnungen.</strong>
            <p>
              Rolltop liest Fälligkeiten aus der Post mit — aus der Mail selbst und aus dem PDF im
              Anhang. Was per Lastschrift, Karte oder Bezahldienst schon beglichen ist, landet hier
              nicht als Erinnerung. Zahlungserinnerungen und Mahnungen stehen ganz oben.
            </p>
          </div>
        </section>
      ) : null}

      {groups.map((group) => (
        <section className={`panel invoices-section section-${group.key}`} key={group.key}>
          <h2>
            {group.title}
            <span className="invoices-section-count">{group.items.length.toLocaleString()}</span>
          </h2>
          <p className="invoices-section-hint">{group.hint}</p>
          <div className="invoices-rows">
            {group.items.map((item) => (
              <InvoiceRow
                key={item.id}
                invoice={item}
                today={today}
                datePrefs={datePrefs}
                expanded={openRows.has(item.id)}
                busy={busyRows.has(item.id)}
                onToggle={() => toggleRow(item.id)}
                onCorrect={(manualStatus) => void correct(item, manualStatus)}
                onDueDate={(day) => void setDueDate(item, day)}
                navigate={navigate}
              />
            ))}
          </div>
        </section>
      ))}
    </>
  );
}

function InvoiceRow({
  invoice,
  today,
  datePrefs,
  expanded,
  busy,
  onToggle,
  onCorrect,
  onDueDate,
  navigate
}: {
  invoice: Invoice;
  today: string;
  datePrefs: DatePrefs;
  expanded: boolean;
  busy: boolean;
  onToggle: () => void;
  onCorrect: (manualStatus: Invoice["manual_status"]) => void;
  onDueDate: (day: string) => void;
  navigate: (url: string) => void;
}) {
  // The newest mail is the one that said what the row now claims, so it is the
  // one the row opens; the rest are shown only when the row is expanded.
  const latest = invoice.messages[0];
  const others = invoice.messages.slice(1);
  const amount = formatAmount(invoice.amount, invoice.currency);
  const open = invoiceIsOpen(invoice);
  const overdue = open && invoice.due_date !== "" && invoice.due_date < today;

  function openMessage(event: { preventDefault: () => void }, url: string) {
    event.preventDefault();
    navigate(url);
  }

  return (
    <div className={`invoices-row status-${invoice.status}${overdue ? " overdue" : ""}${invoice.dunning_level > 0 ? " chased" : ""}`}>
      <div className="invoices-row-main">
        <span className="invoices-issuer" title={`Rechnung von ${invoice.issuer}`}>
          <Icon name="receipt" weight={open ? "fill" : "regular"} />
          {invoice.issuer}
        </span>
        <div className="invoices-row-text">
          {latest ? (
            <a href={messageURL(latest.id)} className="invoices-subject" onClick={(event) => openMessage(event, messageURL(latest.id))}>
              {latest.subject || "(kein Betreff)"}
            </a>
          ) : null}
          <div className="invoices-meta">
            <span className="invoices-due">{open ? dueLabel(invoice.due_date, today) : "Erledigt"}</span>
            {amount ? <span className="invoices-amount">{amount}</span> : null}
            {invoice.dunning_level > 0 ? (
              <span className={`invoices-dunning level-${invoice.dunning_level}`}>{dunningLabel(invoice.dunning_level)}</span>
            ) : null}
            {/* Why it counts as settled is worth saying: "bezahlt" and "wird
                abgebucht" are different facts, and only one of them means the
                reader did something. */}
            {!open && invoice.settlement && settlementLabels[invoice.settlement] ? (
              <span className="invoices-settlement">{settlementLabels[invoice.settlement]}</span>
            ) : null}
          </div>
        </div>
        <div className="invoices-row-actions">
          {open ? (
            <button
              className="secondary invoices-paid"
              type="button"
              disabled={busy}
              title="Diese Rechnung ist bezahlt"
              onClick={() => onCorrect("paid")}
            >
              <Icon name="check" />
              Bezahlt
            </button>
          ) : null}
          <button
            className={expanded ? "ghost invoices-expand expanded" : "ghost invoices-expand"}
            type="button"
            aria-expanded={expanded}
            title={expanded ? "Weniger anzeigen" : "Rechnungsnummer, Frist und alle Mails anzeigen"}
            onClick={onToggle}
          >
            <Icon name="expand_more" />
          </button>
        </div>
      </div>
      {expanded ? (
        <div className="invoices-row-details">
          <dl>
            {invoice.number ? (
              <>
                <dt>Rechnungsnummer</dt>
                <dd><code>{invoice.number}</code></dd>
              </>
            ) : null}
            <dt>Absender</dt>
            <dd>{invoice.issuer}</dd>
          </dl>
          {/* An invoice whose terms were only in a scanned page arrives with no
              deadline at all, and the reader is the only one who can read those.
              This is the counterpart to ticking off a parcel the carrier never
              reported. */}
          <label className="invoices-duedate">
            <span>Frist</span>
            <input
              type="date"
              value={invoice.manual_due_date || invoice.due_date || ""}
              disabled={busy}
              onChange={(event) => onDueDate(event.target.value)}
            />
            {invoice.manual_due_date ? (
              <button className="ghost" type="button" disabled={busy} onClick={() => onDueDate("")}>
                Zurücksetzen
              </button>
            ) : null}
          </label>
          <div className="invoices-correction">
            {invoice.manual_status === "paid" ? (
              <>
                <span className="invoices-correction-note">
                  <Icon name="check" />
                  Von dir als bezahlt vermerkt.
                </span>
                <button className="ghost" type="button" disabled={busy} onClick={() => onCorrect("")}>
                  Rückgängig
                </button>
              </>
            ) : (
              // Mail read as a bill is sometimes not one at all. Saying so is
              // what stops the list asking again.
              <button
                className="ghost invoices-dismiss"
                type="button"
                disabled={busy}
                title="Das ist keine Rechnung - nicht mehr anzeigen"
                onClick={() => onCorrect("dismissed")}
              >
                <Icon name="close" />
                Keine Rechnung
              </button>
            )}
          </div>
          {others.length > 0 ? (
            <div className="invoices-history">
              <span className="invoices-history-title">Weitere Mails zu dieser Rechnung</span>
              {others.map((message) => (
                <a
                  key={message.id}
                  href={messageURL(message.id)}
                  className="invoices-history-row"
                  onClick={(event) => openMessage(event, messageURL(message.id))}
                >
                  <span className="invoices-history-subject">{message.subject || "(kein Betreff)"}</span>
                  <span className="invoices-history-date">{displayDateTime(message.date, datePrefs)}</span>
                </a>
              ))}
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
