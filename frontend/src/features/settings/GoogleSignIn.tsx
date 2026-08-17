// File overview: The sign-in method chooser and connection picker shared by the
// IMAP and SMTP server forms. Both forms make the same choice against the same
// connection list, and keeping one copy is what stops them drifting apart the
// way they already had.

import { AUTH_GOOGLE, AUTH_PASSWORD } from "../../lib/accountForm";
import type { GoogleConnection } from "./googleConnections";

/** SignInMethodField offers Google as an alternative to a stored password. It
 * renders nothing when the server has no Google credentials configured, so the
 * form looks exactly as it did before the feature existed. */
export function SignInMethodField({
  configured,
  value,
  onChange
}: {
  configured: boolean;
  value: string;
  onChange: (value: string) => void;
}) {
  if (!configured) return null;
  return (
    <label className="field">
      <span>Sign-in</span>
      <select value={value} onChange={(event) => onChange(event.target.value)}>
        <option value={AUTH_PASSWORD}>Password</option>
        <option value={AUTH_GOOGLE}>Google account</option>
      </select>
    </label>
  );
}

/** GoogleConnectionField picks which connected account a server authenticates
 * as, and says so when there is nothing to pick yet. */
export function GoogleConnectionField({
  connections,
  value,
  onChange,
  hint,
  onConnectAccounts
}: {
  connections: GoogleConnection[];
  value: string;
  onChange: (value: string) => void;
  hint: string;
  onConnectAccounts: () => void;
}) {
  return (
    <>
      <label className="field">
        <span>Google account</span>
        <select value={value} onChange={(event) => onChange(event.target.value)} required>
          <option value="">Choose a connected account</option>
          {connections.map((connection) => (
            <option key={connection.id} value={String(connection.id)}>
              {connection.email}
              {connection.needs_reauth ? " (needs reauthorization)" : ""}
            </option>
          ))}
        </select>
      </label>
      {connections.length === 0 ? (
        <p className="muted">
          No Google account is connected yet.{" "}
          <a
            href="/settings/account/google"
            onClick={(event) => {
              event.preventDefault();
              onConnectAccounts();
            }}
          >
            Connect one first
          </a>
          .
        </p>
      ) : (
        <p className="muted">{hint}</p>
      )}
    </>
  );
}
