// File overview: The SMTP conversation, on the page where the outgoing server
// is configured. A send that fails answers the composer with one sentence and
// writes the reason to a log nobody self-hosting can read, which left "sending
// does not work" as the whole diagnosis. This panel shows what actually
// happened on the wire: the greeting, the extensions the server advertised, the
// TLS upgrade, the reply to AUTH, and the reply to the message.
//
// The button beside it runs the same login without offering a message, so a
// server can be tested before anybody is written to. Nothing here can show a
// password or a message body — the backend redacts the AUTH exchange and
// records the payload as a byte count — so the transcript is safe to read on
// screen and to paste into a support thread.

import { useCallback, useEffect, useRef, useState } from "react";

import { api } from "../../api";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { displayLogTimestamp } from "../../lib/format";
import type { DatePrefs } from "../../appTypes";
import type { SMTPLogSession } from "../../types";

/** SESSION_LIMIT is how many attempts the panel asks for. The backend keeps a
 * short tail per user, and a misconfigured server is diagnosed from the last
 * few attempts rather than from a history. */
const SESSION_LIMIT = 10;

function sessionTitle(session: SMTPLogSession): string {
  const kind = session.kind === "test" ? "Connection test" : "Send";
  return `${kind} · ${session.host}:${session.port}`;
}

function sessionTone(session: SMTPLogSession): "ok" | "bad" | "busy" {
  if (session.error) return "bad";
  return session.ended_at ? "ok" : "busy";
}

function sessionState(session: SMTPLogSession): string {
  if (session.error) return "Failed";
  return session.ended_at ? "Succeeded" : "Running";
}

/** Transcript renders one conversation. Each line says who spoke, because a
 * reply only means something next to the command that drew it. */
function Transcript({ session, datePrefs }: { session: SMTPLogSession; datePrefs: DatePrefs }) {
  if (!session.lines.length) return <p className="settings-hint">Nothing was recorded for this attempt.</p>;
  return (
    <>
      <ol className="smtp-traffic-lines">
        {session.lines.map((line, index) => (
          <li key={`${session.id}-${index}`} className={`is-${line.direction}`}>
            <time dateTime={line.time}>{displayLogTimestamp(line.time, datePrefs) || line.time}</time>
            <span className="smtp-traffic-arrow" aria-hidden="true">
              {line.direction === "client" ? "→" : line.direction === "server" ? "←" : "·"}
            </span>
            <pre>{line.text}</pre>
          </li>
        ))}
      </ol>
      {session.truncated ? (
        <p className="settings-hint">This conversation was longer than the recorder keeps; the rest was dropped.</p>
      ) : null}
    </>
  );
}

/**
 * SMTPTrafficPanel is the connection test and the recorded traffic for one
 * outgoing server. It loads on mount so a reader who arrives after a failed
 * send finds the failure already on screen instead of having to reproduce it.
 */
export function SMTPTrafficPanel({
  csrf,
  accountID,
  datePrefs
}: {
  csrf: string;
  accountID: number;
  datePrefs: DatePrefs;
}) {
  const [sessions, setSessions] = useState<SMTPLogSession[] | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testOutcome, setTestOutcome] = useState<{ ok: boolean; message: string } | null>(null);
  const [openSessionID, setOpenSessionID] = useState(0);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const { sessions: loaded } = await api.smtpLog(accountID, SESSION_LIMIT);
      if (!mounted.current) return;
      setSessions(loaded || []);
      setError("");
    } catch (err) {
      if (mounted.current) setError(messageFromError(err));
    } finally {
      if (mounted.current) setLoading(false);
    }
  }, [accountID]);

  useEffect(() => {
    setSessions(null);
    setTestOutcome(null);
    setOpenSessionID(0);
    void load();
  }, [load]);

  async function runTest() {
    setTesting(true);
    setTestOutcome(null);
    try {
      const result = await api.testSMTPAccount(csrf, accountID);
      if (!mounted.current) return;
      setTestOutcome({
        ok: result.ok,
        message: result.ok
          ? "Connected and authenticated. No message was sent."
          : result.error || "The test failed without naming a reason."
      });
      // The transcript the test produced is opened straight away: pressing the
      // button is a request to see the conversation, not to be told it exists.
      if (result.session) setOpenSessionID(result.session.id);
      await load();
    } catch (err) {
      if (mounted.current) setTestOutcome({ ok: false, message: messageFromError(err) });
    } finally {
      if (mounted.current) setTesting(false);
    }
  }

  return (
    <section className="panel smtp-traffic">
      <h2>Connection test and SMTP traffic</h2>
      <p className="settings-hint">
        The test opens a connection to this server, upgrades it if TLS is configured, and signs in — then hangs up
        without offering a message, so nobody is written to. Every real send is recorded the same way. Passwords,
        tokens, and message contents are never recorded: the sign-in exchange is redacted and a message appears
        only as its size.
      </p>
      <div className="smtp-traffic-actions">
        <button type="button" disabled={testing} onClick={() => void runTest()}>
          <Icon name="send" />
          {testing ? "Testing..." : "Test connection"}
        </button>
        <button type="button" className="secondary" disabled={loading} onClick={() => void load()}>
          <Icon name="sync" />
          {sessions ? "Reload traffic" : "Load traffic"}
        </button>
      </div>
      {testOutcome ? (
        <p className={testOutcome.ok ? "settings-hint smtp-traffic-result is-ok" : "settings-error"}>
          {testOutcome.message}
        </p>
      ) : null}
      {error ? <p className="settings-error">{error}</p> : null}
      {sessions && sessions.length === 0 ? (
        <p className="settings-hint">
          Nothing recorded yet for this server. Run the test above, or send a message, and the conversation appears
          here. Recorded traffic is kept in memory only and is lost when Rolltop restarts.
        </p>
      ) : null}
      {sessions && sessions.length > 0 ? (
        <ol className="smtp-traffic-sessions">
          {sessions.map((session) => {
            const open = session.id === openSessionID;
            return (
              <li key={session.id} className={`is-${sessionTone(session)}`}>
                <button
                  type="button"
                  className="smtp-traffic-session"
                  aria-expanded={open}
                  onClick={() => setOpenSessionID(open ? 0 : session.id)}
                >
                  <span className="smtp-traffic-session-title">{sessionTitle(session)}</span>
                  <span className="smtp-traffic-session-state">{sessionState(session)}</span>
                  <time dateTime={session.started_at}>
                    {displayLogTimestamp(session.started_at, datePrefs) || session.started_at}
                  </time>
                </button>
                {session.error ? <p className="smtp-traffic-session-error">{session.error}</p> : null}
                {open ? <Transcript session={session} datePrefs={datePrefs} /> : null}
              </li>
            );
          })}
        </ol>
      ) : null}
    </section>
  );
}
