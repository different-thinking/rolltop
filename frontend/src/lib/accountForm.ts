// File overview: Form adapters for account settings. They isolate UI defaults and account-to-form
// conversion from the large settings component.

import type { Account } from "../types";

/** Authentication methods an IMAP account can use. */
export const AUTH_PASSWORD = "password";
export const AUTH_GOOGLE = "google_oauth";

/** emptyAccountForm returns UI defaults for a new IMAP account form. */
export function emptyAccountForm() {
  return {
    email: "",
    label: "",
    host: "",
    port: "993",
    username: "",
    password: "",
    use_tls: true,
    smtp_host: "",
    smtp_port: "587",
    smtp_username: "",
    smtp_password: "",
    smtp_use_tls: true,
    smtp_same_as_imap: true,
    mailbox: "*",
    sync_interval_minutes: "10",
    auth_type: AUTH_PASSWORD,
    google_connection_id: "",
    sync_start_at: ""
  };
}

/** accountToForm adapts an API account row into editable string/boolean form state. */
export function accountToForm(account: Account | null) {
  if (!account) return emptyAccountForm();
  return {
    email: account.email || "",
    label: account.label || "",
    host: account.host || "",
    port: String(account.port || 993),
    username: account.username || "",
    password: "",
    use_tls: account.use_tls,
    smtp_host: account.smtp_host || "",
    smtp_port: String(account.smtp_port || 587),
    smtp_username: account.smtp_username || "",
    smtp_password: "",
    smtp_use_tls: account.smtp_use_tls,
    smtp_same_as_imap: account.smtp_same_as_imap,
    mailbox: account.mailbox || "*",
    sync_interval_minutes: String(account.sync_interval_minutes || 10),
    auth_type: account.auth_type === AUTH_GOOGLE ? AUTH_GOOGLE : AUTH_PASSWORD,
    google_connection_id: account.google_connection_id ? String(account.google_connection_id) : "",
    sync_start_at: account.sync_start_at || ""
  };
}

/**
 * suggestedSyncStart returns the date two years back, offered when a Google
 * account is added. A long-lived Gmail mailbox otherwise spends hours on its
 * first sync and grows the database far beyond what the user is looking for.
 */
export function suggestedSyncStart(today = new Date()): string {
  const start = new Date(Date.UTC(today.getUTCFullYear() - 2, today.getUTCMonth(), today.getUTCDate()));
  return start.toISOString().slice(0, 10);
}
