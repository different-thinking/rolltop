// File overview: List-header maintenance actions that act on a whole list rather than on
// selected rows: archiving everything older than a date, and emptying a Trash folder.

import { useEffect, useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { api } from "../../api";
import type { AddToast } from "../../appTypes";
import type { MailView } from "../../lib/routes";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { messageCountLabel } from "../../lib/format";

/** ArchiveScope is the list an archive-by-date pass reads, in the server's own terms. */
export type ArchiveScope = {
  mailboxID: number;
  query: string;
  view?: MailView;
  /** label names the list in the dialog, e.g. "Inbox" or a folder name. */
  label: string;
};

/** dateInputValue renders a Date as the yyyy-mm-dd a date input expects, in local time. */
function dateInputValue(value: Date): string {
  const local = new Date(value.getTime() - value.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 10);
}

function monthsAgo(months: number): Date {
  const value = new Date();
  value.setHours(0, 0, 0, 0);
  value.setMonth(value.getMonth() - months);
  return value;
}

/** archiveCutoffPresets offer the usual "clear the backlog" cutoffs. */
function archiveCutoffPresets(): { label: string; value: string }[] {
  return [
    { label: "Older than 3 months", value: dateInputValue(monthsAgo(3)) },
    { label: "Older than 1 year", value: dateInputValue(monthsAgo(12)) },
    { label: "Older than 3 years", value: dateInputValue(monthsAgo(36)) }
  ];
}

/** displayCutoff renders the chosen day the way the dialog talks about it. */
function displayCutoff(value: string): string {
  const parsed = new Date(`${value}T00:00:00`);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleDateString(undefined, { year: "numeric", month: "long", day: "numeric" });
}

/**
 * ArchiveBeforeControl moves everything in the current list that is older than a
 * chosen day into each account's Archive folder. The cutoff is a calendar day
 * and the day itself is kept, so "before 1 March" leaves 1 March where it is.
 */
export function ArchiveBeforeControl({
  csrf,
  scope,
  archiveConfigured,
  disabled = false,
  addToast,
  onArchived
}: {
  csrf: string;
  scope: ArchiveScope;
  /** False while no account has an Archive folder, which the server would reject. */
  archiveConfigured: boolean;
  disabled?: boolean;
  addToast: AddToast;
  onArchived: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [cutoff, setCutoff] = useState(() => dateInputValue(monthsAgo(12)));
  const trigger = useRef<HTMLButtonElement | null>(null);
  const dateField = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (open) dateField.current?.focus();
  }, [open]);

  function close() {
    if (busy) return;
    setOpen(false);
    trigger.current?.focus();
  }

  function handleKeys(event: KeyboardEvent<HTMLElement>) {
    if (event.key !== "Escape") return;
    event.stopPropagation();
    close();
  }

  // Like the whole-filter delete, this cannot hide behind an undo toast: the
  // resolved set is far larger than a page, so the dialog is the confirmation.
  async function archiveOlder() {
    if (busy || !cutoff) return;
    setBusy(true);
    try {
      const result = await api.scopeArchiveMessages(csrf, {
        mailboxID: scope.mailboxID,
        query: scope.query,
        view: scope.view,
        before: cutoff
      });
      const queuedMessages = result.queued_messages || 0;
      const parts: string[] = [];
      if (queuedMessages > 0) parts.push(`Archiving ${messageCountLabel(queuedMessages)}. This continues in the background.`);
      else if (result.skipped > 0) parts.push("Everything older than that date is already archived.");
      else parts.push(`Nothing in ${scope.label} is older than ${displayCutoff(cutoff)}.`);
      if (queuedMessages > 0 && result.skipped > 0) parts.push(`${messageCountLabel(result.skipped)} already archived were skipped.`);
      if (result.truncated) parts.push(`This pass covers ${messageCountLabel(result.matched)} — repeat it to continue with the rest.`);
      addToast(parts.join(" "));
      if (result.partial_error) addToast(result.partial_error, "error");
      setOpen(false);
      onArchived();
    } catch (err) {
      addToast(`Archive failed: ${messageFromError(err)}`, "error");
    } finally {
      setBusy(false);
    }
  }

  const dialog = open && typeof document !== "undefined" ? createPortal(
    <div className="confirm-backdrop" role="presentation" onClick={close}>
      <section
        className="confirm-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="archive-before-title"
        onClick={(event) => event.stopPropagation()}
        onKeyDown={handleKeys}
      >
        <h2 id="archive-before-title">Archive older mail</h2>
        <p>
          Every message in {scope.label} dated before {displayCutoff(cutoff)} moves into its account's Archive
          folder. Mail from that day stays where it is. This is not limited to the page you can see, it runs in the
          background, and it cannot be undone.
        </p>
        <div className="date-cutoff-presets">
          {archiveCutoffPresets().map((preset) => (
            <button
              className={`secondary ${preset.value === cutoff ? "active" : ""}`}
              type="button"
              key={preset.label}
              disabled={busy}
              onClick={() => setCutoff(preset.value)}
            >
              {preset.label}
            </button>
          ))}
        </div>
        <label className="date-cutoff-field">
          <span>Archive everything before</span>
          <input
            ref={dateField}
            type="date"
            value={cutoff}
            max={dateInputValue(new Date())}
            disabled={busy}
            onChange={(event) => setCutoff(event.target.value)}
          />
        </label>
        <div className="actions">
          <button className="secondary" type="button" disabled={busy} onClick={close}>Cancel</button>
          <button type="button" disabled={busy || !cutoff} onClick={() => void archiveOlder()}>
            {busy ? "Archiving" : "Archive older mail"}
          </button>
        </div>
      </section>
    </div>,
    document.body
  ) : null;

  const unavailableHint = "Choose an Archive folder for your accounts in Settings first";
  return (
    <>
      <button
        className="list-head-action"
        type="button"
        ref={trigger}
        disabled={disabled || busy || !archiveConfigured}
        onClick={() => setOpen(true)}
        title={archiveConfigured ? "Archive everything older than a date" : unavailableHint}
      >
        <Icon name="archive" />
        <span>Archive older</span>
      </button>
      {dialog}
    </>
  );
}

/**
 * EmptyTrashControl deletes a Trash folder's contents on the IMAP server. It is
 * the only action in the app that removes mail rather than moving it, so the
 * dialog says so plainly and the button only appears on a Trash folder.
 */
export function EmptyTrashControl({
  csrf,
  mailboxID,
  mailboxName,
  messageCount,
  disabled = false,
  addToast,
  onEmptied
}: {
  csrf: string;
  mailboxID: number;
  mailboxName: string;
  /** The local count, which is what the list is showing; the server empties all of it. */
  messageCount?: number;
  disabled?: boolean;
  addToast: AddToast;
  onEmptied: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const cancel = useRef<HTMLButtonElement | null>(null);
  const confirm = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    if (open) cancel.current?.focus();
  }, [open]);

  function close() {
    if (busy) return;
    setOpen(false);
    trigger.current?.focus();
  }

  // The confirmation owns the keyboard while it is open: Escape backs out and
  // Tab cannot wander behind the backdrop of an irreversible action.
  function handleKeys(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape") {
      event.stopPropagation();
      close();
      return;
    }
    if (event.key !== "Tab") return;
    const back = cancel.current;
    const forwardTarget = confirm.current;
    if (!back || !forwardTarget) return;
    event.preventDefault();
    const active = document.activeElement;
    if (!event.shiftKey) (active === back ? forwardTarget : back).focus();
    else (active === forwardTarget ? back : forwardTarget).focus();
  }

  async function emptyTrash() {
    if (busy) return;
    setBusy(true);
    try {
      await api.emptyTrash(csrf, mailboxID);
      addToast(`Emptying ${mailboxName} on the server. This continues in the background.`);
      setOpen(false);
      onEmptied();
    } catch (err) {
      addToast(`Empty Trash failed: ${messageFromError(err)}`, "error");
    } finally {
      setBusy(false);
    }
  }

  const dialog = open && typeof document !== "undefined" ? createPortal(
    <div className="confirm-backdrop" role="presentation" onClick={close}>
      <section
        className="confirm-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="empty-trash-title"
        onClick={(event) => event.stopPropagation()}
        onKeyDown={handleKeys}
      >
        <h2 id="empty-trash-title">Empty {mailboxName}?</h2>
        <p>
          Every message in {mailboxName}
          {messageCount !== undefined && messageCount > 0 ? ` — about ${messageCount.toLocaleString()} of them` : ""} is
          deleted on the mail server itself, not moved somewhere else. Mail this app has never downloaded is deleted too.
          This runs in the background and it cannot be undone.
        </p>
        <div className="actions">
          <button className="secondary" type="button" ref={cancel} disabled={busy} onClick={close}>Cancel</button>
          <button type="button" ref={confirm} disabled={busy} onClick={() => void emptyTrash()}>
            {busy ? "Emptying" : "Delete everything"}
          </button>
        </div>
      </section>
    </div>,
    document.body
  ) : null;

  return (
    <>
      <button
        className="list-head-action destructive"
        type="button"
        ref={trigger}
        disabled={disabled || busy}
        onClick={() => setOpen(true)}
        title={`Permanently delete everything in ${mailboxName}`}
      >
        <Icon name="delete" />
        <span>Empty Trash</span>
      </button>
      {dialog}
    </>
  );
}
