// File overview: List-header maintenance actions that act on a whole list rather than on
// selected rows: archiving everything older than a cutoff, deleting everything older than
// a cutoff, and emptying a Trash folder.

import { useEffect, useId, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { createPortal } from "react-dom";
import { api } from "../../api";
import type { AddToast } from "../../appTypes";
import type { MailView } from "../../lib/routes";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { messageCountLabel } from "../../lib/format";
import type { RelativeCutoff } from "../../lib/retentionCutoff";
import {
  cutoffInstant,
  dateInputValue,
  displayCutoff,
  relativeCutoffDay,
  retentionUnitChoices
} from "../../lib/retentionCutoff";
import type { ScopeMoveResponse } from "../../types";

/** ArchiveScope is the list an act-by-date pass reads, in the server's own terms. */
export type ArchiveScope = {
  mailboxID: number;
  query: string;
  view?: MailView;
  /** label names the list in the dialog, e.g. "Inbox" or a folder name. */
  label: string;
};

/**
 * cutoffPresets offer the usual "clear the backlog" cutoffs. They are relative
 * rather than dates because that is what a reader clearing a backlog means: the
 * same button pressed next month is meant to keep the last three months again.
 */
const cutoffPresets: RelativeCutoff[] = [
  { count: 30, unit: "days" },
  { count: 3, unit: "months" },
  { count: 1, unit: "years" },
  { count: 3, unit: "years" }
];

/**
 * CutoffChoice is how the reader named the cutoff, kept in the shape they named
 * it in rather than resolved on the spot. Both spellings are offered because
 * they answer different questions: "older than 30 days" is a rolling backlog,
 * and a fixed day is one line drawn under a particular event. `day` is what the
 * relative spelling currently resolves to, so the dialog can say which day it
 * is talking about whichever mode is selected.
 */
type CutoffChoice = { mode: "relative" | "fixed"; relative: RelativeCutoff; day: string };

function defaultChoice(): CutoffChoice {
  const relative: RelativeCutoff = { count: 1, unit: "years" };
  return { mode: "relative", relative, day: relativeCutoffDay(relative) };
}

/**
 * CutoffFields is the cutoff half of both dialogs: the presets, the relative
 * spelling, and the fixed one. Sharing it is what keeps the two dialogs saying
 * the same thing about the same day — the day a cutoff names is kept, and
 * everything before it is acted on.
 */
function CutoffFields({
  value,
  disabled,
  onChange
}: {
  value: CutoffChoice;
  disabled: boolean;
  onChange: (next: CutoffChoice) => void;
}) {
  const countFieldID = useId();
  return (
    <>
      <div className="date-cutoff-presets">
        {cutoffPresets.map((preset) => {
          const active = value.mode === "relative"
            && value.relative.count === preset.count && value.relative.unit === preset.unit;
          return (
            <button
              className={`secondary ${active ? "active" : ""}`}
              type="button"
              key={`${preset.count}-${preset.unit}`}
              disabled={disabled}
              onClick={() => onChange({ mode: "relative", relative: preset, day: relativeCutoffDay(preset) })}
            >
              {`Older than ${preset.count} ${preset.unit === "years" && preset.count === 1 ? "year" : preset.unit}`}
            </button>
          );
        })}
      </div>
      <div className="date-cutoff-modes" role="group" aria-label="How the cutoff is chosen">
        <label className="date-cutoff-mode">
          <input
            type="radio"
            name={`${countFieldID}-mode`}
            checked={value.mode === "relative"}
            disabled={disabled}
            onChange={() => onChange({ ...value, mode: "relative", day: relativeCutoffDay(value.relative) })}
          />
          <span>Older than</span>
          <input
            type="number"
            min={1}
            max={3650}
            aria-label="How much older"
            value={value.relative.count}
            disabled={disabled || value.mode !== "relative"}
            onChange={(event) => {
              const relative = { ...value.relative, count: Number(event.target.value) || 0 };
              onChange({ mode: "relative", relative, day: relativeCutoffDay(relative) });
            }}
          />
          <select
            aria-label="Counted in"
            value={value.relative.unit}
            disabled={disabled || value.mode !== "relative"}
            onChange={(event) => {
              const relative = { ...value.relative, unit: event.target.value as RelativeCutoff["unit"] };
              onChange({ mode: "relative", relative, day: relativeCutoffDay(relative) });
            }}
          >
            {retentionUnitChoices.map((unit) => (
              <option key={unit.value} value={unit.value}>{unit.label}</option>
            ))}
          </select>
        </label>
        <label className="date-cutoff-mode">
          <input
            type="radio"
            name={`${countFieldID}-mode`}
            checked={value.mode === "fixed"}
            disabled={disabled}
            onChange={() => onChange({ ...value, mode: "fixed" })}
          />
          <span>Before</span>
          <input
            type="date"
            value={value.day}
            max={dateInputValue(new Date())}
            disabled={disabled || value.mode !== "fixed"}
            onChange={(event) => onChange({ mode: "fixed", relative: value.relative, day: event.target.value })}
          />
        </label>
      </div>
    </>
  );
}

/**
 * OlderThanControl is the shared shape of "act on everything in this list older
 * than a cutoff": a header button, a confirmation that owns the keyboard while
 * it is open, and one call that hands the server the filter rather than a page
 * of message IDs.
 *
 * Like the whole-filter delete, neither pass can hide behind an undo toast: the
 * resolved set is far larger than a page, so the dialog is the confirmation.
 */
function OlderThanControl({
  buttonLabel,
  buttonIcon,
  buttonClassName,
  buttonTitle,
  dialogTitle,
  describe,
  confirmLabel,
  busyLabel,
  failurePrefix,
  disabled,
  unavailable,
  addToast,
  run,
  report,
  onDone
}: {
  buttonLabel: string;
  buttonIcon: string;
  buttonClassName?: string;
  buttonTitle: string;
  dialogTitle: string;
  /** describe renders the sentence saying what the pass will do to which day. */
  describe: (day: string) => ReactNode;
  confirmLabel: string;
  busyLabel: string;
  failurePrefix: string;
  disabled: boolean;
  unavailable: boolean;
  addToast: AddToast;
  run: (before: string) => Promise<ScopeMoveResponse>;
  /** report turns the server's answer into the one sentence the toast says. */
  report: (result: ScopeMoveResponse, day: string) => string;
  onDone: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [cutoff, setCutoff] = useState<CutoffChoice>(defaultChoice);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const dialogBody = useRef<HTMLElement | null>(null);
  const titleID = useId();

  // The first enabled control, rather than a named one: the fixed-date field is
  // disabled while the relative spelling is selected, and focusing it there
  // would leave the keyboard outside the dialog it is supposed to own.
  useEffect(() => {
    if (!open) return;
    dialogBody.current?.querySelector<HTMLElement>(
      "button:not(:disabled),input:not(:disabled),select:not(:disabled)"
    )?.focus();
  }, [open]);

  function close() {
    if (busy) return;
    setOpen(false);
    trigger.current?.focus();
  }

  // The confirmation owns the keyboard while it is open: Escape backs out, and
  // Tab cycles inside the dialog rather than wandering onto the list behind the
  // backdrop while an irreversible action is being confirmed.
  function handleKeys(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape") {
      event.stopPropagation();
      close();
      return;
    }
    if (event.key !== "Tab") return;
    const focusable = Array.from(
      dialogBody.current?.querySelectorAll<HTMLElement>(
        "button:not(:disabled),input:not(:disabled),select:not(:disabled)"
      ) || []
    );
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    const active = document.activeElement;
    if (!event.shiftKey && active === last) {
      event.preventDefault();
      first.focus();
    } else if (event.shiftKey && active === first) {
      event.preventDefault();
      last.focus();
    }
  }

  async function act() {
    if (busy || !cutoff.day) return;
    setBusy(true);
    try {
      // A relative cutoff is resolved at the moment it is acted on rather than
      // when it was chosen, so a dialog left open overnight still keeps the
      // number of days it says it keeps.
      const day = cutoff.mode === "relative" ? relativeCutoffDay(cutoff.relative) : cutoff.day;
      const result = await run(cutoffInstant(day));
      addToast(report(result, day));
      if (result.partial_error) addToast(result.partial_error, "error");
      setOpen(false);
      onDone();
    } catch (err) {
      addToast(`${failurePrefix}: ${messageFromError(err)}`, "error");
    } finally {
      setBusy(false);
    }
  }

  const dialog = open && typeof document !== "undefined" ? createPortal(
    <div className="confirm-backdrop" role="presentation" onClick={close}>
      <section
        ref={dialogBody}
        className="confirm-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        onClick={(event) => event.stopPropagation()}
        onKeyDown={handleKeys}
      >
        <h2 id={titleID}>{dialogTitle}</h2>
        <p>{describe(cutoff.day)}</p>
        <CutoffFields value={cutoff} disabled={busy} onChange={setCutoff} />
        <div className="actions">
          <button className="secondary" type="button" disabled={busy} onClick={close}>Cancel</button>
          <button type="button" disabled={busy || !cutoff.day} onClick={() => void act()}>
            {busy ? busyLabel : confirmLabel}
          </button>
        </div>
      </section>
    </div>,
    document.body
  ) : null;

  return (
    <>
      <button
        className={`list-head-action${buttonClassName ? ` ${buttonClassName}` : ""}`}
        type="button"
        ref={trigger}
        disabled={disabled || busy || unavailable}
        onClick={() => setOpen(true)}
        title={buttonTitle}
      >
        <Icon name={buttonIcon} />
        <span>{buttonLabel}</span>
      </button>
      {dialog}
    </>
  );
}

/**
 * ArchiveBeforeControl moves everything in the current list that is older than a
 * chosen cutoff into each account's Archive folder. The cutoff is a calendar day
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
  const unavailableHint = "Choose an Archive folder for your accounts in Settings first";
  return (
    <OlderThanControl
      buttonLabel="Archive older"
      buttonIcon="archive"
      buttonTitle={archiveConfigured ? "Archive everything older than a cutoff" : unavailableHint}
      dialogTitle="Archive older mail"
      describe={(day) => (
        <>
          Every message in {scope.label} dated before {displayCutoff(day)} moves into its account's Archive
          folder. Mail from that day stays where it is. This is not limited to the page you can see, it runs in the
          background, and it cannot be undone.
        </>
      )}
      confirmLabel="Archive older mail"
      busyLabel="Archiving"
      failurePrefix="Archive failed"
      disabled={disabled}
      unavailable={!archiveConfigured}
      addToast={addToast}
      run={(before) => api.scopeArchiveMessages(csrf, {
        mailboxID: scope.mailboxID, query: scope.query, view: scope.view, before
      })}
      report={(result, day) => {
        const queuedMessages = result.queued_messages || 0;
        const parts: string[] = [];
        if (queuedMessages > 0) parts.push(`Archiving ${messageCountLabel(queuedMessages)}. This continues in the background.`);
        else if (result.skipped > 0) parts.push("Everything older than that date is already archived.");
        else parts.push(`Nothing in ${scope.label} is older than ${displayCutoff(day)}.`);
        if (queuedMessages > 0 && result.skipped > 0) parts.push(`${messageCountLabel(result.skipped)} already archived were skipped.`);
        if (result.truncated) parts.push(`This pass covers ${messageCountLabel(result.matched)} — repeat it to continue with the rest.`);
        return parts.join(" ");
      }}
      onDone={onArchived}
    />
  );
}

/**
 * DeleteBeforeControl throws away everything in the current list that is older
 * than a chosen cutoff: it moves the mail into each account's Trash folder, the
 * same as pressing Delete on every one of those messages would. Nothing is
 * removed from a mail server here — the Trash is emptied by hand, or on the
 * schedule the retention settings hold.
 *
 * It reaches every folder the list draws from rather than the Inbox alone, so
 * "delete everything in Newsletters older than a year" reaches the newsletters
 * filed away in folders of their own. It reaches exactly that list and no more:
 * a button on a list acts on what the reader is looking at, so the folders that
 * list leaves out — an Archive folder above all — are left out here too. The
 * standing retention rules are the ones that speak for a whole category.
 */
export function DeleteBeforeControl({
  csrf,
  scope,
  disabled = false,
  addToast,
  onDeleted
}: {
  csrf: string;
  scope: ArchiveScope;
  disabled?: boolean;
  addToast: AddToast;
  onDeleted: () => void;
}) {
  return (
    <OlderThanControl
      buttonLabel="Delete older"
      buttonIcon="delete"
      buttonClassName="destructive"
      buttonTitle="Move everything older than a cutoff to Trash"
      dialogTitle="Delete older mail"
      describe={(day) => (
        <>
          Every message in {scope.label} dated before {displayCutoff(day)} moves into its account's Trash
          folder, across every folder this list draws from. Mail from that day stays where it is. Nothing is deleted
          on the mail server itself until the Trash is emptied. This is not limited to the page you can see and it
          runs in the background.
        </>
      )}
      confirmLabel="Move older mail to Trash"
      busyLabel="Deleting"
      failurePrefix="Delete failed"
      disabled={disabled}
      unavailable={false}
      addToast={addToast}
      run={(before) => api.scopeTrashMessages(csrf, {
        mailboxID: scope.mailboxID, query: scope.query, view: scope.view, before
      })}
      report={(result, day) => {
        const queuedMessages = result.queued_messages || 0;
        const parts: string[] = [];
        if (queuedMessages > 0) parts.push(`Moving ${messageCountLabel(queuedMessages)} to Trash. This continues in the background.`);
        else if (result.skipped > 0) parts.push("Everything older than that date is already in Trash.");
        else parts.push(`Nothing in ${scope.label} is older than ${displayCutoff(day)}.`);
        if (queuedMessages > 0 && result.skipped > 0) parts.push(`${messageCountLabel(result.skipped)} already there were skipped.`);
        if (result.truncated) parts.push(`This pass covers ${messageCountLabel(result.matched)} — repeat it to continue with the rest.`);
        return parts.join(" ");
      }}
      onDone={onDeleted}
    />
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
