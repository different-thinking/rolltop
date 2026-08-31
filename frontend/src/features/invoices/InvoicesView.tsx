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
 * says. So "Chased" is a group of its own and it comes first.
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
    { key: "chased", title: "Chased", hint: "A reminder or an overdue notice has already arrived", items: chased },
    { key: "overdue", title: "Overdue", hint: "The deadline has passed", items: overdue },
    { key: "today", title: "Due today", hint: "Today is the last day", items: dueToday },
    { key: "upcoming", title: "Open", hint: "With a deadline still ahead", items: upcoming },
    { key: "undated", title: "No deadline", hint: "No deadline was read -- you can enter one yourself", items: undated },
    { key: "paid", title: "Settled", hint: "Paid, debited or credited", items: paid }
  ].filter((group) => group.items.length > 0);
}

/** dueLabel names a deadline the way a reader would say it. It differs from the
 * parcel list's in that days in the past are counted rather than merely named:
 * "9 days overdue" is what a reader needs to know about a late bill, where
 * "Tuesday" would leave them doing the arithmetic. */
export function dueLabel(day: string, today: string): string {
  if (day === "") return "No deadline given";
  const date = new Date(`${day}T00:00:00`);
  const now = new Date(`${today}T00:00:00`);
  if (Number.isNaN(date.getTime()) || Number.isNaN(now.getTime())) return day;
  const distance = Math.round((date.getTime() - now.getTime()) / 86400000);
  if (distance === 0) return "Due today";
  if (distance === 1) return "Due tomorrow";
  if (distance === -1) return "Overdue since yesterday";
  if (distance < -1) return `Overdue by ${-distance} days`;
  const written = date.toLocaleDateString("en", { day: "numeric", month: "short", year: "numeric" });
  if (distance < 7) return `${date.toLocaleDateString("en", { weekday: "long" })}, ${written}`;
  return `Due on ${written}`;
}

/** formatAmount renders the stored "1234.56" as a sum with its currency. The
 * amount is stored normalized so two spellings of one sum compare equal; this
 * is the other end of that. It is written the way the rest of this view writes,
 * rather than in the reader's date locale, so a row reads as one sentence. */
export function formatAmount(amount: string, currency: string): string {
  if (amount === "") return "";
  const value = Number(amount);
  if (!Number.isFinite(value)) return amount;
  try {
    return value.toLocaleString("en", {
      style: currency ? "currency" : "decimal",
      currency: currency || undefined,
      minimumFractionDigits: 2
    });
  } catch {
    // An unknown currency code is a sender's typo, not a reason to show nothing.
    return `${value.toLocaleString("en", { minimumFractionDigits: 2 })} ${currency}`.trim();
  }
}

/** dunningLabels name the three grades. The scale is what a reader needs to
 * tell apart rather than what any one country's dunning practice distinguishes. */
export const dunningLabels: Record<number, string> = {
  1: "Payment reminder",
  2: "Overdue notice",
  3: "Final notice"
};

export function dunningLabel(level: number): string {
  return dunningLabels[level] || "Chased";
}

/** settlementLabels say why a bill counts as settled, which is the difference
 * between "you paid this" and "you never had to". */
const settlementLabels: Record<string, string> = {
  transfer: "Bank transfer",
  direct_debit: "Direct debit",
  card: "Card",
  wallet: "Payment service"
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
      if (manualStatus === "paid") addToast("Marked as paid.");
      if (manualStatus === "dismissed") addToast("No longer listed as an invoice.");
      if (manualStatus === "") addToast("Mark taken back.");
    });
  }

  async function setDueDate(invoice: Invoice, day: string) {
    await withBusyRow(invoice.id, async () => {
      const result = await api.setInvoiceDueDate(csrf, invoice.id, day);
      applyResult(invoice.id, result.invoice, false);
      addToast(day ? "Deadline saved." : "Deadline removed.");
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
          <h1>Invoices</h1>
          <span className="label-pill">{open.length.toLocaleString()} open</span>
          {dueNow > 0 ? (
            <span className="label-pill invoices-pill-due">{dueNow.toLocaleString()} due</span>
          ) : null}
        </div>
      </div>

      {error ? <div className="error">{error}</div> : null}

      {invoices !== null && groups.length === 0 && !error ? (
        <section className="panel invoices-idle">
          <Icon name="receipt" />
          <div>
            <strong>No open invoices.</strong>
            <p>
              Rolltop reads deadlines out of the mail — from the message itself and from the PDF
              attached to it. Anything already settled by direct debit, card or a payment service
              does not turn up here as a reminder. Reminders and overdue notices stand at the top.
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
        <span className="invoices-issuer" title={`Invoice from ${invoice.issuer}`}>
          <Icon name="receipt" weight={open ? "fill" : "regular"} />
          {invoice.issuer}
        </span>
        <div className="invoices-row-text">
          {latest ? (
            <a href={messageURL(latest.id)} className="invoices-subject" onClick={(event) => openMessage(event, messageURL(latest.id))}>
              {latest.subject || "(no subject)"}
            </a>
          ) : null}
          <div className="invoices-meta">
            <span className="invoices-due">{open ? dueLabel(invoice.due_date, today) : "Settled"}</span>
            {amount ? <span className="invoices-amount">{amount}</span> : null}
            {invoice.dunning_level > 0 ? (
              <span className={`invoices-dunning level-${invoice.dunning_level}`}>{dunningLabel(invoice.dunning_level)}</span>
            ) : null}
            {/* Why it counts as settled is worth saying: "you paid this" and
                "this is debited automatically" are different facts, and only
                one of them means the reader did something. */}
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
              title="This invoice is paid"
              onClick={() => onCorrect("paid")}
            >
              <Icon name="check" />
              Paid
            </button>
          ) : null}
          <button
            className={expanded ? "ghost invoices-expand expanded" : "ghost invoices-expand"}
            type="button"
            aria-expanded={expanded}
            title={expanded ? "Show less" : "Show the invoice number, deadline and every message"}
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
                <dt>Invoice number</dt>
                <dd><code>{invoice.number}</code></dd>
              </>
            ) : null}
            <dt>From</dt>
            <dd>{invoice.issuer}</dd>
          </dl>
          {/* An invoice whose terms were only in a scanned page arrives with no
              deadline at all, and the reader is the only one who can read those.
              This is the counterpart to ticking off a parcel the carrier never
              reported. */}
          <label className="invoices-duedate">
            <span>Deadline</span>
            <input
              type="date"
              value={invoice.manual_due_date || invoice.due_date || ""}
              disabled={busy}
              onChange={(event) => onDueDate(event.target.value)}
            />
            {invoice.manual_due_date ? (
              <button className="ghost" type="button" disabled={busy} onClick={() => onDueDate("")}>
                Reset
              </button>
            ) : null}
          </label>
          <div className="invoices-correction">
            {invoice.manual_status === "paid" ? (
              <>
                <span className="invoices-correction-note">
                  <Icon name="check" />
                  You marked this as paid.
                </span>
                <button className="ghost" type="button" disabled={busy} onClick={() => onCorrect("")}>
                  Undo
                </button>
              </>
            ) : (
              // Mail read as a bill is sometimes not one at all. Saying so is
              // what stops the list asking again.
              <button
                className="ghost invoices-dismiss"
                type="button"
                disabled={busy}
                title="This is not an invoice - stop showing it"
                onClick={() => onCorrect("dismissed")}
              >
                <Icon name="close" />
                Not an invoice
              </button>
            )}
          </div>
          {others.length > 0 ? (
            <div className="invoices-history">
              <span className="invoices-history-title">More mail about this invoice</span>
              {others.map((message) => (
                <a
                  key={message.id}
                  href={messageURL(message.id)}
                  className="invoices-history-row"
                  onClick={(event) => openMessage(event, messageURL(message.id))}
                >
                  <span className="invoices-history-subject">{message.subject || "(no subject)"}</span>
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
