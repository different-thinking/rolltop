// File overview: The card that opens from a name or address in a message
// header. It shows what the address book knows about that address - or just
// the address when it knows nothing - and offers the three things a reader
// wants from a header: the address on the clipboard, a new message to it, and
// the contact behind it (opened, or created with the name the header carried).
//
// The card is rendered through a portal with fixed placement rather than as an
// absolutely positioned child of the header line, because every line it opens
// from truncates with overflow hidden, and a child card would be clipped to a
// single line of it.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, KeyboardEvent, MouseEvent, ReactNode } from "react";
import { createPortal } from "react-dom";
import { api } from "../../api";
import type { AddToast, Navigate } from "../../appTypes";
import type { Contact } from "../../types";
import { Icon } from "../../components/Icon";
import { formatAddress, sameEmail, type MailAddress } from "../../lib/addresses";
import { copyText } from "../../lib/clipboard";
import { contactsURL } from "../../lib/routes";
import { displayInitial } from "../../lib/senderIdentity";

/** AddressCardActions is what the card needs from the view that hosts it. */
export type AddressCardActions = {
  openCompose: (query?: string) => void;
  navigate: Navigate;
  addToast: AddToast;
};

const CARD_WIDTH = 340;
const CARD_MARGIN = 8;
const CARD_GAP = 6;

/**
 * AddressLink is the clickable name or address in a header line. It stops the
 * click where it lands: the header row around it collapses the message, and
 * the recipient line's summary toggles the full headers, and neither is what
 * a reader picking an address asked for.
 */
export function AddressLink({
  address,
  actions,
  className = "",
  title,
  children
}: {
  address: MailAddress;
  actions: AddressCardActions;
  className?: string;
  title?: string;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);

  function toggle(event: MouseEvent<HTMLButtonElement>) {
    event.preventDefault();
    event.stopPropagation();
    setOpen((current) => !current);
  }

  function onKeyDown(event: KeyboardEvent<HTMLButtonElement>) {
    // Enter and Space activate the button on their own; they must not reach
    // the header row, which would read them as "collapse this message".
    if (event.key === "Enter" || event.key === " ") event.stopPropagation();
  }

  const close = useCallback((restoreFocus: boolean) => {
    setOpen(false);
    if (restoreFocus) anchorRef.current?.focus();
  }, []);

  if (!address.email) return <span className={className}>{children}</span>;

  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        className={`address-link ${className}`.trim()}
        title={title ?? address.email}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={toggle}
        onKeyDown={onKeyDown}
      >
        {children}
      </button>
      {open ? <AddressCard address={address} anchorRef={anchorRef} actions={actions} onClose={close} /> : null}
    </>
  );
}

type ContactLookup = { status: "loading" } | { status: "done"; contact: Contact | null };

/**
 * AddressCard is the popover itself. It asks the address book once per open
 * and does not hold the answer across opens: a contact created from this card
 * lands in the address book while the card is closed, and the next open must
 * see it.
 */
function AddressCard({
  address,
  anchorRef,
  actions,
  onClose
}: {
  address: MailAddress;
  anchorRef: { current: HTMLButtonElement | null };
  actions: AddressCardActions;
  onClose: (restoreFocus: boolean) => void;
}) {
  const cardRef = useRef<HTMLDivElement | null>(null);
  const [lookup, setLookup] = useState<ContactLookup>({ status: "loading" });
  const [style, setStyle] = useState<CSSProperties>({ visibility: "hidden" });

  useEffect(() => {
    let cancelled = false;
    void api
      .contacts(address.email)
      .then((data) => {
        if (cancelled) return;
        const contact = (data.contacts || []).find((candidate) => candidate.emails.some((row) => sameEmail(row.email, address.email))) || null;
        setLookup({ status: "done", contact });
      })
      .catch(() => {
        // The card is still useful without the address book: the address and
        // the copy and compose buttons do not depend on it.
        if (!cancelled) setLookup({ status: "done", contact: null });
      });
    return () => {
      cancelled = true;
    };
  }, [address.email]);

  const place = useCallback(() => {
    const anchor = anchorRef.current;
    const card = cardRef.current;
    if (!anchor || !card) return;
    const rect = anchor.getBoundingClientRect();
    const width = Math.min(CARD_WIDTH, window.innerWidth - CARD_MARGIN * 2);
    const height = card.offsetHeight;
    const left = Math.max(CARD_MARGIN, Math.min(rect.left, window.innerWidth - width - CARD_MARGIN));
    const below = rect.bottom + CARD_GAP;
    const fitsBelow = below + height <= window.innerHeight - CARD_MARGIN;
    const above = rect.top - CARD_GAP - height;
    const top = fitsBelow || above < CARD_MARGIN ? Math.min(below, Math.max(CARD_MARGIN, window.innerHeight - height - CARD_MARGIN)) : above;
    setStyle({ top, left, width, visibility: "visible" });
  }, [anchorRef]);

  useLayoutEffect(() => {
    place();
  }, [place, lookup]);

  useEffect(() => {
    cardRef.current?.focus({ preventScroll: true });
  }, []);

  useEffect(() => {
    function onPointerDown(event: PointerEvent) {
      const target = event.target as Node | null;
      if (!target) return;
      if (cardRef.current?.contains(target) || anchorRef.current?.contains(target)) return;
      onClose(false);
    }
    function onKeyDown(event: globalThis.KeyboardEvent) {
      if (event.key === "Escape") {
        event.stopPropagation();
        onClose(true);
      }
    }
    document.addEventListener("pointerdown", onPointerDown, true);
    document.addEventListener("keydown", onKeyDown, true);
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      document.removeEventListener("keydown", onKeyDown, true);
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
    };
  }, [anchorRef, onClose, place]);

  const contact = lookup.status === "done" ? lookup.contact : null;
  const name = contact?.display_name || address.name || "";
  const heading = name || address.email;
  const roleLine = contact ? [contact.job_title, contact.organization].filter((part) => part.trim() !== "").join(" · ") : "";
  const otherEmails = contact ? contact.emails.filter((row) => row.email.trim() !== "" && !sameEmail(row.email, address.email)) : [];
  const phones = contact ? contact.phones.filter((row) => row.number.trim() !== "") : [];

  async function copy() {
    const ok = await copyText(address.email);
    actions.addToast(ok ? "Address copied." : "Could not copy the address.", ok ? undefined : "error");
    if (ok) onClose(true);
  }

  function compose() {
    onClose(false);
    actions.openCompose(`to=${encodeURIComponent(formatAddress({ name: contact?.display_name || address.name, email: address.email }))}`);
  }

  function openContact() {
    onClose(false);
    actions.navigate(contact ? contactsURL({ contactID: contact.id, email: address.email }) : contactsURL({ name: address.name, email: address.email }));
  }

  return createPortal(
    <div
      ref={cardRef}
      className="address-card"
      role="dialog"
      aria-label={`Details for ${heading}`}
      tabIndex={-1}
      style={style}
      onClick={(event) => event.stopPropagation()}
    >
      <div className="address-card-head">
        {contact?.icon_url ? <img className="address-card-avatar" src={contact.icon_url} alt="" /> : <span className="address-card-avatar">{displayInitial(heading)}</span>}
        <div className="address-card-identity">
          <strong>{heading}</strong>
          {roleLine ? <span className="address-card-role">{roleLine}</span> : null}
          <span className="address-card-status">
            {lookup.status === "loading" ? "Looking up contact..." : contact ? "In your contacts" : "Not in your contacts"}
          </span>
        </div>
        <button className="ghost icon-only address-card-close" type="button" title="Close" aria-label="Close" onClick={() => onClose(true)}>
          <Icon name="close" />
        </button>
      </div>
      <div className="address-card-row">
        <span className="address-card-email">{address.email}</span>
        <button className="ghost icon-only" type="button" title="Copy address" aria-label="Copy address" onClick={() => void copy()}>
          <Icon name="copy" />
        </button>
      </div>
      {otherEmails.length > 0 || phones.length > 0 ? (
        <dl className="address-card-details">
          {otherEmails.map((row) => (
            <div key={`email:${row.email}`}>
              <dt>{row.label || "Email"}</dt>
              <dd>{row.email}</dd>
            </div>
          ))}
          {phones.map((row) => (
            <div key={`phone:${row.number}`}>
              <dt>{row.label || "Phone"}</dt>
              <dd>{row.number}</dd>
            </div>
          ))}
        </dl>
      ) : null}
      <div className="address-card-actions">
        <button className="secondary" type="button" onClick={() => void copy()}>
          <Icon name="copy" />
          Copy address
        </button>
        <button className="secondary" type="button" onClick={compose}>
          <Icon name="edit" />
          New message
        </button>
        <button className="secondary" type="button" disabled={lookup.status === "loading"} onClick={openContact}>
          <Icon name={contact ? "person" : "person_add"} />
          {contact ? "Open contact" : "Add contact"}
        </button>
      </div>
    </div>,
    document.body
  );
}
