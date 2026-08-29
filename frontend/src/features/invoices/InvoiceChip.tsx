/**
 * File overview: The invoice marker on a message -- a chip on the list row and
 * the same fact spelled out in an open thread.
 *
 * It reads the invoice the server attached to the message rather than anything
 * the message itself says, so a row and the invoice list are always the same
 * answer: the row cannot claim a deadline the list has already moved on from.
 */

import type { MessageInvoiceSummary } from "../../types";
import { Icon } from "../../components/Icon";
import { localDay } from "../deliveries/DeliveriesView";
import { dueLabel, dunningLabel, formatAmount } from "./InvoicesView";

/** compactDue is the deadline as a row has room for it. The invoice list spells
 * it out because it is the page about deadlines; a chip beside a subject line
 * has to stay short, so an overdue bill becomes a day count and everything past
 * the coming week is just its date. */
export function compactDue(day: string, today: string): string {
  if (day === "") return "open";
  const date = new Date(`${day}T00:00:00`);
  const now = new Date(`${today}T00:00:00`);
  if (Number.isNaN(date.getTime()) || Number.isNaN(now.getTime())) return day;
  const distance = Math.round((date.getTime() - now.getTime()) / 86400000);
  // Adverbs rather than dates, and lower case: they are read as part of the
  // chip's sentence, not as a field.
  if (distance === 0) return "today";
  if (distance === 1) return "tomorrow";
  if (distance < 0) return `${-distance} days overdue`;
  const written = date.toLocaleDateString("en", { day: "numeric", month: "short" });
  if (distance < 7) return `${date.toLocaleDateString("en", { weekday: "short" })} ${written}`;
  return written;
}

/** invoiceChipText is what the marker says. A chased bill says so instead of
 * naming a date, because that is the fact that outranks the date. */
export function invoiceChipText(invoice: MessageInvoiceSummary, today: string): string {
  if (invoice.dunning_level > 0) return dunningLabel(invoice.dunning_level);
  return `Due: ${compactDue(invoice.due_date, today)}`;
}

/**
 * InvoiceChip marks a message in a list. A settled bill is left unmarked: the
 * list is scanned for what still needs doing, and a chip on every paid invoice
 * would be a column of noise the reader has to look past.
 */
export function InvoiceChip({ invoice }: { invoice: MessageInvoiceSummary }) {
  if (invoice.status !== "open") return null;
  const today = localDay();
  const pressing = invoice.dunning_level > 0 || (invoice.due_date !== "" && invoice.due_date <= today);
  const amount = formatAmount(invoice.amount, invoice.currency);
  return (
    <span
      className={`invoice-chip ${pressing ? "due-now" : ""} ${invoice.dunning_level > 0 ? "chased" : ""}`}
      title={`Invoice${invoice.number ? ` ${invoice.number}` : ""} from ${invoice.issuer}${amount ? `, ${amount}` : ""}`}
    >
      <Icon name="receipt" weight={pressing ? "fill" : "regular"} />
      <span className="invoice-chip-label">{invoiceChipText(invoice, today)}</span>
    </span>
  );
}

/**
 * InvoiceDetails is the same bill in an open message, where there is room to
 * name it: the number, the sum, the deadline, and the way out to the list.
 */
export function InvoiceDetails({
  invoice,
  navigate
}: {
  invoice: MessageInvoiceSummary;
  navigate: (url: string) => void;
}) {
  const today = localDay();
  const amount = formatAmount(invoice.amount, invoice.currency);
  return (
    <div className="invoice-details">
      <span className="invoice-details-head">
        <Icon name="receipt" weight="fill" />
        {invoice.issuer}
        {invoice.dunning_level > 0 ? (
          <span className={`invoice-details-dunning level-${invoice.dunning_level}`}>{dunningLabel(invoice.dunning_level)}</span>
        ) : null}
      </span>
      <dl>
        {invoice.number ? (
          <>
            <dt>Invoice number</dt>
            <dd><code>{invoice.number}</code></dd>
          </>
        ) : null}
        {amount ? (
          <>
            <dt>Amount</dt>
            <dd>{amount}</dd>
          </>
        ) : null}
        <dt>{invoice.status === "open" ? "Due" : "Status"}</dt>
        <dd>{invoice.status === "open" ? dueLabel(invoice.due_date, today) : "Settled"}</dd>
      </dl>
      <div className="invoice-details-actions">
        <a
          className="secondary"
          href="/invoices"
          onClick={(event) => {
            event.preventDefault();
            navigate("/invoices");
          }}
        >
          <Icon name="receipt" />
          All invoices
        </a>
      </div>
    </div>
  );
}
