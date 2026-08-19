// File overview: Mailbox and search result lists. These components fetch paged conversations,
// surface sync clues, keep selection state stable, and link rows back to their source page.

import { Fragment, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, DragEvent, KeyboardEvent, MouseEvent, ReactNode, TouchEvent } from "react";
import { ApiError, api, bulkMessageIDLimit } from "../../api";
import type { AddToast, DatePrefs, LocationState } from "../../appTypes";
import type { AccountMailboxChoice, Bootstrap, Conversation, MailCategorySummary, Mailbox, SwipeAction, SwipePreferences, SyncRun } from "../../types";
import { Icon } from "../../components/Icon";
import { ListHeader } from "../../components/common";
import { androidNativeAvailable } from "../../lib/androidNative";
import { messageFromError } from "../../lib/errors";
import { dateGroupLabel, displaySnoozeUntil, displayTime, localDayKey, messageCountLabel } from "../../lib/format";
import { displayInitial, stableHash } from "../../lib/senderIdentity";
import { archiveMailboxForAccount, junkMailboxForAccount, roleMailboxIDs, trashMailboxForAccount } from "../../lib/folders";
import { shouldIgnoreMailShortcut } from "../../lib/keyboard";
import { effectiveMailboxSyncMode, mailboxActiveRun, mailboxNeedsSync, mailboxRefreshKey } from "../../lib/sync";
import { HighlightedText } from "../../lib/searchHighlight";
import { mailPageSize } from "../../lib/constants";
import { loadMailSortOrder, saveMailSortOrder } from "../../lib/mailSort";
import type { MailSortOrder } from "../../lib/mailSort";
import { usePullToRefresh } from "../../lib/pullToRefresh";
import { composeURL, mailRoute, mailURL, mailViewCategory, messageURL, routeWithSearch, searchRoute, searchURL } from "../../lib/routes";
import type { MailView } from "../../lib/routes";
import { messageSecurityIndicators, messageSecurityPreviewText, messageSecuritySnippetClassName } from "../../plugins/messageSecurity";
import type { RuntimePlugin } from "../../plugins/runtime";
import { defaultSwipePreferences, swipeActionPresentation, swipeSnoozeUntil } from "../../lib/swipeActions";
import { SnoozeControl } from "./SnoozeControl";
import { ArchiveBeforeControl, EmptyTrashControl } from "./MailListActions";

type SearchActionPlugin = RuntimePlugin & {
  renderSearchActions?: (context: {
    query: string;
    navigate: (url: string) => void;
  }) => ReactNode;
};

type MessageAnnotationPlugin = RuntimePlugin & {
  renderMessageAnnotations?: (context: {
    location: "message-list" | "thread";
    message: Conversation["message"];
    annotations: NonNullable<Conversation["message"]["annotations"]>;
  }) => ReactNode;
};

function searchActionNodes(plugins: RuntimePlugin[], query: string, navigate: (url: string) => void) {
  if (!query) return [];
  return (plugins as SearchActionPlugin[])
    .map((plugin) => plugin.renderSearchActions?.({ query, navigate }))
    .filter(Boolean);
}

// Gmail-style initial avatars: one of eight fixed hues, picked from the sender
// text so the same correspondent keeps the same colour between renders. The
// count is mirrored by the .avatar-hue-N classes in styles/_message-list.scss;
// raising it here without raising it there leaves the new hue unpainted.
const senderAvatarHueCount = 8;

function senderAvatarHue(seed: string): number {
  return stableHash(seed) % senderAvatarHueCount;
}

/**
 * rowDate is the instant a list row is placed by: the reminder in the Snoozed
 * view, and otherwise the key the server sorted the page with, which is the
 * message date except for mail returning from a snooze — that sorts by the
 * moment it came back. The row's timestamp and its date section both read it,
 * so a row can never sit under a heading its own time contradicts.
 */
function rowDate(conversation: Conversation, snoozedView: boolean): string {
  if (snoozedView) return conversation.snoozed_until || conversation.message.date;
  return conversation.list_date || conversation.message.date;
}

/**
 * participantSource is the address line a row names: the recipients in Sent and
 * Drafts, the senders everywhere else. The label beside the avatar and the
 * avatar's own letter and colour are all derived from this one value.
 */
function participantSource(conversation: Conversation, showRecipients: boolean): string {
  const message = conversation.message;
  if (showRecipients) return conversation.recipient_participants || message.to_addr || conversation.participants || "";
  return conversation.participants || message.from_addr || "";
}

/**
 * dateSectionHeadings returns the heading each row opens, or "" for a row that
 * continues the section above it. It is one pass over the page against a single
 * clock: labels derived per row from their own `new Date()` could disagree
 * across midnight, and a row whose date cannot be named must not make the next
 * row re-open a heading that is already on screen.
 */
function dateSectionHeadings(conversations: Conversation[], snoozedView: boolean): string[] {
  const now = new Date();
  let openSection = "";
  return conversations.map((conversation) => {
    const label = dateGroupLabel(rowDate(conversation, snoozedView), now);
    if (!label || label === openSection) return "";
    openSection = label;
    return label;
  });
}

function messageAnnotationNodes(plugins: RuntimePlugin[], message: Conversation["message"]) {
  const annotations = message.annotations || [];
  if (annotations.length === 0) return [];
  return (plugins as MessageAnnotationPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageAnnotations?.({ location: "message-list", message, annotations });
      return node ? <span key={`message-annotation-${index}`}>{node}</span> : null;
    })
    .filter(Boolean);
}

/**
 * MailView fetches one page of mailbox/all-mail conversations. It clears stale
 * rows when the URL changes, animates newly delivered messages on the first page,
 * and shows a folder-level sync clue when the selected mailbox is manual or off.
 */
export function MailView({
  userID,
  csrf,
  datePrefs,
  location,
  navigate,
  replaceRoute,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  archiveMailboxes,
  mailCategories,
  latestSyncRun,
  activeSyncRuns,
  mailGeneration,
  refreshChrome,
  messageSecurityPlugins = [],
  addToast
}: {
  userID: number;
  csrf: string;
  datePrefs: DatePrefs;
  location: LocationState;
  navigate: (url: string) => void;
  /** Used for the empty-page bounce, which must not add a history entry. */
  replaceRoute: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  archiveMailboxes: AccountMailboxChoice[];
  mailCategories: MailCategorySummary[];
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  mailGeneration: number;
  refreshChrome: () => Promise<Bootstrap | null>;
  messageSecurityPlugins?: RuntimePlugin[];
  addToast: AddToast;
}) {
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [syncBusy, setSyncBusy] = useState(false);
  const [pullRefreshing, setPullRefreshing] = useState(false);
  // Bumped by pull-to-refresh and by list mutations that need the page reloaded
  // rather than only patched, so emptied pages pull the next rows forward.
  const [manualRefreshGeneration, setManualRefreshGeneration] = useState(0);
  const loaded = useRef(false);
  const manualViewSyncKey = useRef("");
  const [error, setError] = useState("");
  const [showingSavedPage, setShowingSavedPage] = useState(false);
  const [hasPrev, setHasPrev] = useState(false);
  const [hasNext, setHasNext] = useState(false);
  const [newMessageIDs, setNewMessageIDs] = useState<Set<number>>(() => new Set());
  const [sortOrder, setSortOrder] = useState<MailSortOrder>(() => loadMailSortOrder(userID));
  const [sortOrderUserID, setSortOrderUserID] = useState(userID);
  const previousPageIDs = useRef<Set<number>>(new Set());
  const previousListKey = useRef("");
  const newMessageTimer = useRef<number | null>(null);
  const route = mailRoute(location.path);
  const mailboxID = route.mailboxID;
  const page = route.page;
  const view = route.view;
  // A different signed-in user brings their own stored direction along. This is
  // adjusted during render rather than in an effect so the fetch below already
  // closes over the new user's order instead of requesting the old one first.
  if (sortOrderUserID !== userID) {
    setSortOrderUserID(userID);
    setSortOrder(loadMailSortOrder(userID));
  }
  const mailbox = mailboxes.find((item) => String(item.id) === mailboxID);
  // Each named view counts the folders it actually reads, mirroring how the
  // server builds it: Sent and Drafts are the folders carrying that role, and
  // Inbox is All Mail minus every account's chosen Archive folder.
  const archiveMailboxIDs = new Set(archiveMailboxes.map((item) => item.mailbox_id));
  const viewRole = view === "sent" ? "sent" : view === "drafts" ? "drafts" : "";
  const roleMailboxIDSet = useMemo(
    () => viewRole ? roleMailboxIDs(mailboxes, viewRole) : new Set<number>(),
    [mailboxes, viewRole]
  );
  // A category is decided per message rather than per folder, so its size is
  // the count the server reports for the list; folder arithmetic cannot answer it.
  const activeCategory = mailViewCategory(view);
  const categorySummary = activeCategory ? mailCategories.find((item) => item.name === activeCategory) : undefined;
  // Inbox and the categories both describe mail that is still in play, so both
  // leave each account's Archive folder out.
  const excludesArchived = view === "inbox" || Boolean(activeCategory);
  // Junk is out of these lists by role on the server, whatever its All Mail
  // setting says, so counting it here would promise rows the list cannot show.
  const viewMailboxes = mailboxes.filter((item) => {
    if (viewRole) return roleMailboxIDSet.has(item.id);
    if (item.role === "junk" || item.show_in_all_mail === false) return false;
    return !excludesArchived || !archiveMailboxIDs.has(item.id);
  });
  // A category's size is only knowable from the chrome payload. Leaving it
  // undefined when that has not arrived keeps "unknown" distinct from "empty":
  // reporting 0 would both hide the whole-view affordance and put a wrong number
  // into the delete confirmation behind it.
  const totalCount = mailbox
    ? mailbox.message_count
    : activeCategory
      ? categorySummary?.total
      : viewMailboxes.reduce((sum, item) => sum + item.message_count, 0);
  const viewLabel = activeCategory
    ? categorySummary?.label || activeCategory
    : view === "inbox" ? "Inbox" : view === "sent" ? "Sent" : view === "drafts" ? "Drafts" : "All Mail";
  // The scope comes from the route, never from the folder lookup: a folder being
  // deleted drops out of the chrome list while its page is still open, and
  // falling back to 0 there would silently widen a delete to All Mail. When the
  // route names a folder this view cannot see, no whole-folder scope is offered.
  const scopeMailboxID = Number.parseInt(mailboxID || "0", 10) || 0;
  const listScope: MessageListScope | undefined = scopeMailboxID === 0
    ? { mailboxID: 0, query: "", view, label: viewLabel, total: totalCount }
    : mailbox
      ? { mailboxID: scopeMailboxID, query: "", label: mailbox.name, total: mailbox.message_count }
      : undefined;
  // Archiving by date is offered wherever archiving a single message would make
  // sense. Sent, Drafts, Trash, and Junk are left out: filing a received backlog
  // is no reason to empty the user's own sent mail, and moving the others into
  // Archive would pull mail back out of the folder it was deliberately put in.
  // The server enforces the same rule on the folders inside a whole-account
  // list, which this button cannot narrow.
  const archiveOlderAvailable = view !== "drafts" && view !== "sent"
    && !["sent", "drafts", "trash", "junk"].includes(mailbox?.role || "");
  const refreshKey = `${mailGeneration}:${manualRefreshGeneration}:${mailboxRefreshKey(latestSyncRun, mailbox)}`;
  const listScopeKey = `${userID}:${mailboxID || view || "all"}:${sortOrder}`;
  const listKey = listScopeKey + ":" + page;
  const slideDirection = useListSlideDirection(listScopeKey, page);
  const cachedTransitionPage = previousListKey.current !== listKey ? api.cachedMail(userID, mailboxID, page, sortOrder, view) : null;
  const displayConversations = cachedTransitionPage?.conversations || conversations;
  const displayHasPrev = cachedTransitionPage?.has_prev ?? hasPrev;
  const displayHasNext = cachedTransitionPage?.has_next ?? hasNext;
  const listPending = (loading || previousListKey.current !== listKey) && !cachedTransitionPage;
  const listTransitionSpeed: SlideSpeed = cachedTransitionPage ? "fast" : listPending ? "slow" : "fast";
  const activeRun = mailboxActiveRun(mailbox, activeSyncRuns, latestSyncRun);
  const effectiveMode = mailbox ? effectiveMailboxSyncMode(mailbox, mailboxes) : "auto";
  const accountActiveRun = activeRun || (mailbox ? activeSyncRuns.find((run) =>
    run.status === "running" && run.account_id === mailbox.account_id
  ) || null : null);
  const showRecoveryEmptyState = Boolean(
    mailbox &&
    displayConversations.length === 0 &&
    mailbox.remote_message_count > 0 &&
    typeof mailbox.local_message_count === "number" &&
    mailbox.local_message_count < mailbox.remote_message_count &&
    (effectiveMode === "auto" || Boolean(accountActiveRun))
  );
  const syncAlreadyRunning = syncBusy || (mailbox ? Boolean(activeRun) : activeSyncRuns.length > 0);

  function refreshList() {
    setManualRefreshGeneration((current) => current + 1);
  }

  async function refreshByPull() {
    const startedAt = performance.now();
    setPullRefreshing(true);
    try {
      try {
        if (!syncAlreadyRunning && (!mailbox || effectiveMode !== "never")) {
          if (mailbox) await api.syncFolder(csrf, mailbox.id);
          else await api.syncAccount(csrf);
        }
      } catch (err) {
        // A sync may start between the chrome snapshot and this request. Its SSE
        // updates will still refresh the list, so a conflict is not a pull error.
        if (!(err instanceof ApiError && err.status === 409)) {
          addToast(`Refresh failed: ${messageFromError(err)}`, "error");
        }
      }
      refreshList();
      await refreshChrome();
    } finally {
      const remaining = 450 - (performance.now() - startedAt);
      if (remaining > 0) await new Promise((resolve) => window.setTimeout(resolve, remaining));
      setPullRefreshing(false);
    }
  }

  const pullRefresh = usePullToRefresh<HTMLDivElement>({
    disabled: listPending || pullRefreshing || syncBusy,
    onRefresh: refreshByPull
  });
  const pullStyle = { "--pull-distance": `${pullRefresh.distance}px` } as CSSProperties;

  useEffect(() => {
    return () => {
      if (newMessageTimer.current !== null) window.clearTimeout(newMessageTimer.current);
    };
  }, []);

  // A manual folder is refreshed when the user enters its view. The ref
  // coalesces chrome/SSE rerenders, while a new component mount (browser
  // refresh) or navigating away and back permits one fresh request.
  useEffect(() => {
    if (!mailbox || effectiveMode !== "manual") {
      manualViewSyncKey.current = "";
      return;
    }
    const nextKey = `${userID}:${mailbox.id}`;
    if (manualViewSyncKey.current === nextKey) return;
    manualViewSyncKey.current = nextKey;
    let cancelled = false;
    api.syncFolder(csrf, mailbox.id).catch((err) => {
      // Older servers may still answer with a conflict if another sync won the
      // race. Its event stream will refresh this view, so that is not an error.
      if (err instanceof ApiError && err.status === 409) return;
      if (!cancelled) addToast(`Folder refresh failed: ${messageFromError(err)}`, "error");
    });
    return () => {
      cancelled = true;
    };
  }, [userID, mailbox?.id, effectiveMode, csrf, addToast]);

  // Route changes should feel immediate: clear the old page before the server
  // responds so the user is not looking at stale rows for another folder.
  useEffect(() => {
    let cancelled = false;
    const isNewList = previousListKey.current !== listKey;
    const canAnimateNewMail = page === 1 && loaded.current && !isNewList && Boolean(refreshKey) && Boolean(latestSyncRun?.new_messages);
    if (isNewList || !loaded.current) {
      const cached = api.cachedMail(userID, mailboxID, page, sortOrder, view);
      if (cached) {
        previousPageIDs.current = new Set(cached.conversations.map((conversation) => conversation.message.id));
        previousListKey.current = listKey;
        setConversations(cached.conversations);
        setHasPrev(cached.has_prev);
        setHasNext(cached.has_next);
        setLoading(false);
        setShowingSavedPage(false);
      } else {
        setLoading(true);
        setConversations([]);
        setHasPrev(false);
        setHasNext(false);
        setShowingSavedPage(false);
      }
    }
    setError("");
    api
      .mail(userID, mailboxID, page, sortOrder, view)
      .then((data) => {
        if (cancelled) return;
        const nextIDs = new Set(data.conversations.map((conversation) => conversation.message.id));
        if (canAnimateNewMail) {
          const appeared = data.conversations
            .map((conversation) => conversation.message.id)
            .filter((id) => !previousPageIDs.current.has(id));
          if (appeared.length > 0) {
            setNewMessageIDs(new Set(appeared));
            if (newMessageTimer.current !== null) window.clearTimeout(newMessageTimer.current);
            newMessageTimer.current = window.setTimeout(() => setNewMessageIDs(new Set()), 1200);
          }
        } else {
          setNewMessageIDs(new Set());
        }
        previousPageIDs.current = nextIDs;
        previousListKey.current = listKey;
        setConversations(data.conversations);
        setHasPrev(data.has_prev);
        setHasNext(data.has_next);
        setShowingSavedPage(false);
        // Deleting a full page can leave a later page with nothing on it. The
        // rows did not vanish, they moved forward, so follow them back instead
        // of parking the user on an empty page.
        if (data.conversations.length === 0 && page > 1) replaceRoute(mailURL(mailboxID, page - 1, view));
        if (data.has_next) api.prefetchMail(userID, mailboxID, page + 1, sortOrder, view);
        if (data.has_prev && page > 1) api.prefetchMail(userID, mailboxID, page - 1, sortOrder, view);
      })
      .catch((err) => {
        if (!cancelled) {
          const cached = api.cachedMail(userID, mailboxID, page, sortOrder, view);
          previousListKey.current = listKey;
          if (cached) {
            previousPageIDs.current = new Set(cached.conversations.map((conversation) => conversation.message.id));
            setConversations(cached.conversations);
            setHasPrev(cached.has_prev);
            setHasNext(cached.has_next);
            setShowingSavedPage(true);
            setError(`Showing saved mail. Refresh failed: ${messageFromError(err)}`);
          } else {
            previousPageIDs.current = new Set();
            setConversations([]);
            setHasPrev(false);
            setHasNext(false);
            setShowingSavedPage(false);
            setError(messageFromError(err));
          }
        }
      })
      .finally(() => {
        if (!cancelled) {
          loaded.current = true;
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [userID, mailboxID, page, sortOrder, view, refreshKey, listKey, latestSyncRun?.new_messages]);

  const pageURL = (nextPage: number) => mailURL(mailboxID, nextPage, view);

  // Reversing the direction rebuilds the paging window from the other end, so a
  // reader who was on page 4 of newest-first is sent back to the new first page.
  function changeSortOrder(next: MailSortOrder) {
    if (next === sortOrder) return;
    saveMailSortOrder(userID, next);
    setSortOrder(next);
    if (page !== 1) navigate(mailURL(mailboxID, 1, view));
  }

  function updateReadStates(states: ConversationReadState[]) {
    const readByID = new Map(states.map((state) => [state.id, state.read]));
    setConversations((current) => current.map((conversation) => {
      const read = readByID.get(conversation.message.id);
      if (read === undefined) return conversation;
      return { ...conversation, is_read: read, message: { ...conversation.message, is_read: read } };
    }));
  }

  function removeMovedConversations(messageIDs: number[]) {
    const moved = new Set(messageIDs);
    setConversations((current) => current.filter((conversation) =>
      !conversationTransferMessageIDs(conversation).some((id) => moved.has(id))
    ));
  }

  async function startFolderSync() {
    if (!mailbox) return;
    if (effectiveMode === "never") {
      addToast(`${mailbox.name} is set to Never. Change the folder sync mode before syncing.`, "error");
      return;
    }
    setSyncBusy(true);
    try {
      await api.syncFolder(csrf, mailbox.id);
      addToast(`${mailbox.name} sync started.`);
      await refreshChrome();
    } catch (err) {
      addToast(`Sync failed: ${messageFromError(err)}`, "error");
    } finally {
      setSyncBusy(false);
    }
  }

  return (
    <>
      <ListHeader
        title={mailbox?.name || viewLabel}
        titleClassName="mailbox-title"
        actions={(
          <>
            <MailSortToggle order={sortOrder} onChange={changeSortOrder} />
            {mailbox?.role === "trash" ? (
              <EmptyTrashControl
                // The list header survives a route change, so an open
                // confirmation would otherwise retarget itself at whichever
                // Trash folder the reader navigated to next.
                key={mailbox.id}
                csrf={csrf}
                mailboxID={mailbox.id}
                mailboxName={mailbox.name}
                messageCount={mailbox.message_count}
                disabled={listPending}
                addToast={addToast}
                onEmptied={() => {
                  refreshList();
                  void refreshChrome();
                }}
              />
            ) : null}
            {listScope && archiveOlderAvailable ? (
              <ArchiveBeforeControl
                csrf={csrf}
                scope={{ mailboxID: listScope.mailboxID, query: listScope.query, view: listScope.view, label: listScope.label }}
                archiveConfigured={archiveMailboxes.length > 0}
                disabled={listPending}
                addToast={addToast}
                onArchived={() => {
                  refreshList();
                  void refreshChrome();
                }}
              />
            ) : null}
          </>
        )}
        pager={{
          page,
          pageSize: mailPageSize,
          itemCount: listPending ? 0 : displayConversations.length,
          total: totalCount,
          hasPrev: listPending ? false : displayHasPrev,
          hasNext: listPending ? false : displayHasNext,
          pageURL,
          navigate,
          ariaLabel: "Mailbox pagination",
          loading: listPending
        }}
      />
      <div
        className={`mail-pull-refresh${pullRefresh.distance > 0 ? " pulling" : ""}${pullRefresh.ready ? " ready" : ""}${pullRefreshing ? " refreshing" : ""}`}
        ref={pullRefresh.targetRef}
        style={pullStyle}
      >
        <div
          className="pull-refresh-indicator"
          role="status"
          aria-live="polite"
          aria-label={pullRefreshing ? "Refreshing mail" : pullRefresh.ready ? "Release to refresh mail" : pullRefresh.distance > 0 ? "Pull to refresh mail" : undefined}
        >
          <Icon name="sync" />
          {pullRefreshing ? <span>Refreshing mail</span> : null}
        </div>
        {mailbox ? (
          <FolderSyncNotice
            mailbox={mailbox}
            effectiveMode={effectiveMode}
            activeRun={activeRun}
            busy={syncBusy}
            onSync={startFolderSync}
          />
        ) : null}
        {error ? <div className={showingSavedPage ? "mail-cache-warning" : "error"} role="status">{error}</div> : null}
        {!error || showingSavedPage ? (
          <SlidingMessageListStage stageKey={listKey} direction={slideDirection} pending={listPending} speed={listTransitionSpeed}>
            {listPending ? (
              <div className="mail-list-loading" role="status" aria-label="Refreshing mail" aria-busy="true"><span /></div>
            ) : (
              <MessageList
                csrf={csrf}
                conversations={displayConversations}
                hiddenMessageIDs={hiddenMessageIDs}
                mailboxes={mailboxes}
                currentMailboxID={mailbox?.id || 0}
                swipePreferences={swipePreferences}
                archiveMailboxes={archiveMailboxes}
                highlightMessageIDs={newMessageIDs}
                showRecipients={Boolean(viewRole) || mailbox?.role === "sent" || mailbox?.role === "drafts"}
                openAsDraft={view === "drafts" || mailbox?.role === "drafts"}
                datePrefs={datePrefs}
                returnURL={mailURL(mailboxID, page, view)}
                navigate={navigate}
                messageSecurityPlugins={messageSecurityPlugins}
                addToast={addToast}
                onReadStatesChange={updateReadStates}
                onMessagesMoved={removeMovedConversations}
                onListChanged={refreshList}
                listScope={listScope}
                emptyState={showRecoveryEmptyState && mailbox ? (
                  <MailboxRecoveryEmptyState mailbox={mailbox} activeRun={accountActiveRun} />
                ) : undefined}
              />
            )}
          </SlidingMessageListStage>
        ) : null}
      </div>
    </>
  );
}

/**
 * MailSortToggle switches All Mail and folder lists between newest and oldest
 * first. Both directions stay visible so the current one is readable at a
 * glance instead of hidden behind a single toggle's next state, and both stay
 * clickable while a page loads because the pending request is dropped anyway.
 */
function MailSortToggle({
  order,
  onChange
}: {
  order: MailSortOrder;
  onChange: (order: MailSortOrder) => void;
}) {
  const choices: Array<{ value: MailSortOrder; label: string; icon: string; title: string }> = [
    { value: "newest", label: "Newest", icon: "sort_descending", title: "Sort by date, newest first" },
    { value: "oldest", label: "Oldest", icon: "sort_ascending", title: "Sort by date, oldest first" }
  ];
  return (
    <div className="mail-sort-toggle" role="group" aria-label="Sort by date">
      {choices.map((choice) => (
        <button
          className={choice.value === order ? "active" : ""}
          type="button"
          key={choice.value}
          title={choice.title}
          aria-label={choice.title}
          aria-pressed={choice.value === order}
          onClick={() => onChange(choice.value)}
        >
          <Icon name={choice.icon} />
          <span>{choice.label}</span>
        </button>
      ))}
    </div>
  );
}

/** SnoozedView reuses the normal conversation list for active local reminders. */
export function SnoozedView({
  csrf,
  datePrefs,
  location,
  navigate,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  archiveMailboxes,
  mailGeneration,
  messageSecurityPlugins = [],
  addToast
}: {
  csrf: string;
  datePrefs: DatePrefs;
  location: LocationState;
  navigate: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  archiveMailboxes: AccountMailboxChoice[];
  mailGeneration: number;
  messageSecurityPlugins?: RuntimePlugin[];
  addToast: AddToast;
}) {
  const page = Math.max(1, Number.parseInt(new URLSearchParams(location.search).get("page") || "1", 10) || 1);
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [hasPrev, setHasPrev] = useState(false);
  const [hasNext, setHasNext] = useState(false);
  const [refreshGeneration, setRefreshGeneration] = useState(0);
  const [refreshing, setRefreshing] = useState(false);
  const pullRefresh = usePullToRefresh<HTMLDivElement>({
    disabled: loading || refreshing,
    onRefresh: async () => {
      setRefreshing(true);
      setRefreshGeneration((current) => current + 1);
      await new Promise((resolve) => window.setTimeout(resolve, 350));
      setRefreshing(false);
    }
  });
  const pullStyle = { "--pull-distance": `${pullRefresh.distance}px` } as CSSProperties;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError("");
    api.snoozes(page)
      .then((data) => {
        if (cancelled) return;
        setConversations(data.conversations);
        setHasPrev(data.has_prev);
        setHasNext(data.has_next);
      })
      .catch((err) => {
        if (!cancelled) setError(messageFromError(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [page, mailGeneration, refreshGeneration]);

  function updateReadStates(states: ConversationReadState[]) {
    const readByID = new Map(states.map((state) => [state.id, state.read]));
    setConversations((current) => current.map((conversation) => {
      const read = readByID.get(conversation.message.id);
      return read === undefined ? conversation : { ...conversation, is_read: read, message: { ...conversation.message, is_read: read } };
    }));
  }

  function removeMovedConversations(messageIDs: number[]) {
    const moved = new Set(messageIDs);
    setConversations((current) => current.filter((conversation) =>
      !conversationTransferMessageIDs(conversation).some((id) => moved.has(id))
    ));
  }

  const pageURL = (nextPage: number) => `/snoozes${nextPage > 1 ? `?page=${nextPage}` : ""}`;
  return (
    <>
      <ListHeader
        title="Snoozed"
        pager={{ page, pageSize: mailPageSize, itemCount: loading ? 0 : conversations.length, hasPrev, hasNext, pageURL, navigate, ariaLabel: "Snoozed pagination", loading }}
      />
    <div
    className={`mail-pull-refresh${pullRefresh.distance > 0 ? " pulling" : ""}${pullRefresh.ready ? " ready" : ""}${refreshing ? " refreshing" : ""}`}
    ref={pullRefresh.targetRef}
    style={pullStyle}
    >
    <div className="pull-refresh-indicator" role="status" aria-live="polite">
      <Icon name="sync" />
      {refreshing ? <span>Refreshing snoozed</span> : null}
    </div>
    {error ? <div className="error">{error}</div> : null}
    {!error ? <div className="message-list-pane">
      {loading ? <div className="mail-list-loading" role="status" aria-label="Refreshing snoozed mail" aria-busy="true"><span /></div> : (
            <MessageList
              csrf={csrf}
              conversations={conversations}
              hiddenMessageIDs={hiddenMessageIDs}
              mailboxes={mailboxes}
              swipePreferences={swipePreferences}
              archiveMailboxes={archiveMailboxes}
              datePrefs={datePrefs}
              returnURL={routeWithSearch(location.path, location.search)}
              navigate={navigate}
              messageSecurityPlugins={messageSecurityPlugins}
              addToast={addToast}
              onReadStatesChange={updateReadStates}
              onMessagesMoved={removeMovedConversations}
              onListChanged={() => setRefreshGeneration((current) => current + 1)}
              snoozedView
            />
      )}
    </div> : null}
    </div>
    </>
  );
}

// FolderSyncNotice is shown only when the selected folder is known to be
// excluded from automatic sync or behind the remote mailbox.
function FolderSyncNotice({
  mailbox,
  effectiveMode,
  activeRun,
  busy,
  onSync
}: {
  mailbox: Mailbox;
  effectiveMode: string;
  activeRun: SyncRun | null;
  busy: boolean;
  onSync: () => void;
}) {
  const syncOff = effectiveMode === "never";
  const needsManualSync = effectiveMode === "manual" && mailboxNeedsSync(mailbox) && !activeRun;
  if (!syncOff && !needsManualSync) return null;

  const title = syncOff ? "Folder sync is off" : "Folder is not fully synced";
  const detail = syncOff
    ? "This folder is excluded from sync. Change its sync mode in folder settings before mirroring it."
    : "This manual-sync folder is behind the remote mailbox. Sync it to mirror the latest messages.";
  const buttonLabel = busy ? "Starting" : "Sync folder";

  return (
    <section className="folder-sync-notice" aria-live="polite">
      <Icon name="report" />
      <div className="folder-sync-copy">
        <strong>{title}</strong>
        <span>{detail}</span>
      </div>
      {!syncOff ? (
        <button className="secondary" type="button" disabled={busy} onClick={onSync}>
          <Icon name="sync" />
          {buttonLabel}
        </button>
      ) : null}
    </section>
  );
}

function MailboxRecoveryEmptyState({
  mailbox,
  activeRun
}: {
  mailbox: Mailbox;
  activeRun: SyncRun | null;
}) {
  const localCount = Math.max(0, mailbox.local_message_count || 0);
  const remoteCount = Math.max(0, mailbox.remote_message_count || 0);
  const runMailbox = activeRun?.current_mailbox.trim() || "";
  const runMatchesMailbox = Boolean(activeRun) && runMailbox.toLowerCase() === mailbox.name.trim().toLowerCase();
  const total = Math.max(0, activeRun?.messages_total || 0);
  const seen = Math.max(0, activeRun?.messages_seen || 0);
  const progress = total > 0 ? Math.min(100, Math.round((seen / total) * 100)) : 0;
  const title = runMatchesMailbox
    ? "Loading this folder"
    : activeRun
      ? "Account sync is still running"
      : "This folder has not finished loading";
  const activity = activeRun
    ? total > 0
      ? `${runMatchesMailbox ? "Folder sync" : `Syncing ${runMailbox || "this account"}`}: ${Math.min(seen, total).toLocaleString()} of ${total.toLocaleString()} checked.`
      : runMatchesMailbox
        ? "Checking this folder for messages now."
        : `Currently syncing ${runMailbox || "this account"}.`
    : "Waiting for this folder's next synchronization.";

  return (
    <section
      className={`mailbox-recovery-empty${activeRun ? " running" : ""}`}
      role="status"
      aria-live="polite"
      aria-busy={Boolean(activeRun)}
    >
      <Icon name={activeRun ? "sync" : "report"} />
      <div className="folder-sync-copy">
        <strong>{title}</strong>
        <span>
          The mail server reports {remoteCount.toLocaleString()} messages; {localCount.toLocaleString()} are available in Rolltop so far.
        </span>
        <span>{activity}</span>
        {activeRun && total > 0 ? (
          <div
            className="folder-sync-progress"
            role="progressbar"
            aria-label={`${runMailbox || mailbox.name} sync progress`}
            aria-valuemin={0}
            aria-valuemax={total}
            aria-valuenow={Math.min(seen, total)}
          >
            <div style={{ width: `${progress}%` }} />
          </div>
        ) : null}
      </div>
    </section>
  );
}


// The server treats in: as a folder operator only at the start of the query or
// after whitespace, so the same rule decides whether a query is folder-scoped.
const searchMailboxOperator = /(^|\s)in:("[^"]+"|\S+)/i;

function searchNamesMailbox(query: string): boolean {
  return searchMailboxOperator.test(query);
}

function activeSearchMaintenanceRun(runs: SyncRun[]): SyncRun | null {
  return runs.find((run) => {
    const subject = (run.latest_new_subject || "").toLowerCase();
    return subject === "purging full-text index" ||
      subject === "purging local references and full-text index" ||
      subject === "repairing full-text index" ||
      subject.includes("search index repair");
  }) || null;
}

function SearchMaintenanceNotice({ run }: { run: SyncRun }) {
  const total = Math.max(0, run.messages_total || 0);
  const seen = Math.max(0, run.messages_seen || 0);
  const done = total > 0 ? Math.min(seen, total) : seen;
  const remaining = total > 0 ? Math.max(0, total - done) : 0;
  const label = run.latest_new_subject || "Full-text indexing";
  const scope = run.current_mailbox ? ` in ${run.current_mailbox}` : "";
  const progress = total > 0
    ? `${done.toLocaleString()} of ${total.toLocaleString()} messages checked`
    : done > 0 ? `${done.toLocaleString()} messages checked` : "Index work is running";
  const remainingText = remaining > 0 ? `, ${remaining.toLocaleString()} remaining` : "";

  return (
    <section className="folder-sync-notice search-maintenance-notice running" aria-live="polite">
      <Icon name="report" />
      <div className="folder-sync-copy">
        <strong>Search may be slow</strong>
        <span>{label}{scope}. {progress}{remainingText}.</span>
      </div>
    </section>
  );
}

/**
 * SearchView is always best-match search. The URL carries the query and page so
 * opening a result can preserve a precise back target to the same result page.
 */
export function SearchView({
  csrf,
  location,
  navigate,
  replaceRoute,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  archiveMailboxes,
  datePrefs,
  activeSyncRuns,
  mailGeneration,
  messageSecurityPlugins = [],
  searchActionPlugins = [],
  addToast
}: {
  csrf: string;
  location: LocationState;
  navigate: (url: string) => void;
  /** Used for the empty-page bounce, which must not add a history entry. */
  replaceRoute: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  archiveMailboxes: AccountMailboxChoice[];
  datePrefs: DatePrefs;
  activeSyncRuns: SyncRun[];
  mailGeneration: number;
  messageSecurityPlugins?: RuntimePlugin[];
  searchActionPlugins?: RuntimePlugin[];
  addToast: AddToast;
}) {
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [hasPrev, setHasPrev] = useState(false);
  const [hasNext, setHasNext] = useState(false);
  const [refreshGeneration, setRefreshGeneration] = useState(0);
  const [staleResults, setStaleResults] = useState("");
  const loadedKey = useRef("");
  const route = searchRoute(location.path);
  const query = route.query;
  const page = route.page;
  const searchKey = query + ":best:" + page;
  const slideDirection = useListSlideDirection("search:" + query, page);
  const listPending = loading || loadedKey.current !== searchKey;
  // A search is far more expensive than a mailbox page, and a bulk move emits a
  // mail-list change per moved message. Coalescing those bursts keeps one delete
  // from turning into hundreds of searches while still following the mailbox.
  const settledGeneration = useSettledValue(mailGeneration, 1500, 8000);

  useEffect(() => {
    let cancelled = false;
    // A refresh of the same result page keeps its rows on screen: only a new
    // query or page clears them, so a background reload does not flash a
    // loading state over a list the user is working in.
    if (loadedKey.current !== searchKey) {
      setLoading(true);
      setConversations([]);
      setHasPrev(false);
      setHasNext(false);
    }
    setError("");
    setStaleResults("");
    api
      .search(query, page)
      .then((data) => {
        if (cancelled) return;
        loadedKey.current = searchKey;
        setConversations(data.conversations);
        setHasPrev(data.has_prev);
        setHasNext(data.has_next);
        if (data.conversations.length === 0 && page > 1) replaceRoute(searchURL(query, page - 1));
        if (data.has_next) api.prefetchSearch(query, page + 1);
      })
      .catch((err) => {
        if (cancelled) return;
        // A failed background reload must not take the results away: keep the
        // page that is already on screen and say it could not be refreshed. Only
        // a first load for this query has nothing to fall back to.
        if (loadedKey.current === searchKey) {
          setStaleResults(`Showing earlier results. Refresh failed: ${messageFromError(err)}`);
          return;
        }
        loadedKey.current = searchKey;
        setConversations([]);
        setHasPrev(false);
        setHasNext(false);
        setError(messageFromError(err));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [query, page, searchKey, settledGeneration, refreshGeneration]);

  const pageURL = (nextPage: number) => searchURL(query, nextPage);
  const returnURL = routeWithSearch(location.path, location.search);
  const maintenanceRun = activeSearchMaintenanceRun(activeSyncRuns);
  const pluginSearchActions = searchActionNodes(searchActionPlugins, query, navigate);

  function updateReadStates(states: ConversationReadState[]) {
    const readByID = new Map(states.map((state) => [state.id, state.read]));
    setConversations((current) => current.map((conversation) => {
      const read = readByID.get(conversation.message.id);
      if (read === undefined) return conversation;
      return { ...conversation, is_read: read, message: { ...conversation.message, is_read: read } };
    }));
  }

  function removeMovedConversations(messageIDs: number[]) {
    const moved = new Set(messageIDs);
    setConversations((current) => current.filter((conversation) =>
      !conversationTransferMessageIDs(conversation).some((id) => moved.has(id))
    ));
  }

  return (
    <>
      <ListHeader
        title="Search"
        pager={{
          page,
          pageSize: mailPageSize,
          itemCount: listPending ? 0 : conversations.length,
          hasPrev: listPending ? false : hasPrev,
          hasNext: listPending ? false : hasNext,
          pageURL,
          navigate,
          ariaLabel: "Search pagination",
          loading: listPending
        }}
      />
      {query ? (
        <div className="search-result-actions">
          <div className="muted">
            Results for <strong>{query}</strong>
            {/* Deleted mail is left out the way All Mail leaves it out, so say
                where it went instead of letting results look incomplete. A query
                that names a folder is already explicitly scoped, so it says
                nothing there — matched against the operator the server parses,
                not any "in:" in the text, so "checkin:notes" still gets it. */}
            {searchNamesMailbox(query) ? null : <span className="search-scope-hint"> · Trash excluded, add in:trash to include it</span>}
          </div>
          {pluginSearchActions}
        </div>
      ) : null}
      {maintenanceRun ? <SearchMaintenanceNotice run={maintenanceRun} /> : null}
      {error ? <div className="error">{error}</div> : null}
      {staleResults ? <div className="mail-cache-warning" role="status">{staleResults}</div> : null}
      {!error ? (
        <SlidingMessageListStage stageKey={searchKey} direction={slideDirection} pending={listPending} speed={listPending ? "slow" : "fast"}>
          {listPending ? (
            <div className="mail-list-loading" role="status" aria-label="Searching mail" aria-busy="true"><span /></div>
          ) : (
            <MessageList
              csrf={csrf}
              conversations={conversations}
              hiddenMessageIDs={hiddenMessageIDs}
              mailboxes={mailboxes}
              swipePreferences={swipePreferences}
              archiveMailboxes={archiveMailboxes}
              navigate={navigate}
              searchQuery={query}
              datePrefs={datePrefs}
              returnURL={returnURL}
              addToast={addToast}
              messageSecurityPlugins={messageSecurityPlugins}
              onReadStatesChange={updateReadStates}
              onMessagesMoved={removeMovedConversations}
              onListChanged={() => setRefreshGeneration((current) => current + 1)}
              listScope={{ mailboxID: 0, query, label: query ? `“${query}”` : "All Mail" }}
            />
          )}
        </SlidingMessageListStage>
      ) : null}
    </>
  );
}

type SlideDirection = "left" | "right" | "none";
type SlideSpeed = "fast" | "slow";

type OutgoingListPane = {
  key: string;
  child: ReactNode;
  direction: Exclude<SlideDirection, "none">;
};

/**
 * useSettledValue follows `value` but waits for a quiet period before adopting a
 * new one, and never lags further behind than maxWaitMS. Sync runs report every
 * moved message, so views whose reload is expensive follow the settled value.
 */
function useSettledValue<T>(value: T, quietMS: number, maxWaitMS: number): T {
  const [settled, setSettled] = useState(value);
  const firstChangeAt = useRef(0);

  useEffect(() => {
    if (settled === value) {
      firstChangeAt.current = 0;
      return;
    }
    if (firstChangeAt.current === 0) firstChangeAt.current = Date.now();
    const waited = Date.now() - firstChangeAt.current;
    const delay = Math.max(0, Math.min(quietMS, maxWaitMS - waited));
    const timer = window.setTimeout(() => setSettled(value), delay);
    return () => window.clearTimeout(timer);
  }, [value, settled, quietMS, maxWaitMS]);

  return settled;
}

function useListSlideDirection(scopeKey: string, page: number): SlideDirection {
  const previous = useRef({ scopeKey, page });
  const direction = useRef<SlideDirection>("none");
  if (previous.current.scopeKey !== scopeKey || previous.current.page !== page) {
    direction.current = previous.current.scopeKey === scopeKey && page !== previous.current.page
      ? page > previous.current.page ? "left" : "right"
      : "none";
    previous.current = { scopeKey, page };
  }
  return direction.current;
}

function SlidingMessageListStage({
  stageKey,
  direction,
  pending,
  speed,
  children
}: {
  stageKey: string;
  direction: SlideDirection;
  pending: boolean;
  speed: SlideSpeed;
  children: ReactNode;
}) {
  const lastPane = useRef({ key: stageKey, child: children });
  const measuredHeight = useRef(0);
  const currentPane = useRef<HTMLDivElement | null>(null);
  const [outgoing, setOutgoing] = useState<OutgoingListPane | null>(null);
  const [lockedHeight, setLockedHeight] = useState<number | null>(null);

  useLayoutEffect(() => {
    if (lastPane.current.key === stageKey) return;
    const previous = lastPane.current;
    if (measuredHeight.current > 0) setLockedHeight(measuredHeight.current);
    if (direction !== "none") {
      setOutgoing({ key: previous.key, child: previous.child, direction });
      const timer = window.setTimeout(() => {
        setOutgoing((current) => current?.key === previous.key ? null : current);
      }, speed === "slow" ? 640 : 140);
      lastPane.current = { key: stageKey, child: children };
      return () => window.clearTimeout(timer);
    }
    lastPane.current = { key: stageKey, child: children };
    setOutgoing(null);
  }, [stageKey, direction, speed, children]);

  useLayoutEffect(() => {
    lastPane.current = { key: stageKey, child: children };
    if (!pending && currentPane.current) {
      measuredHeight.current = currentPane.current.offsetHeight;
    }
  });

  useLayoutEffect(() => {
    if (!pending && !outgoing) setLockedHeight(null);
  }, [pending, outgoing]);

  const stageStyle = lockedHeight ? { minHeight: `${lockedHeight}px` } : undefined;
  if (outgoing) {
    const incomingPane = (
      <div className="message-list-pane incoming" key={stageKey} ref={currentPane}>
        {children}
      </div>
    );
    const outgoingPane = (
      <div className="message-list-pane outgoing" key={`out-${outgoing.key}`}>
        {outgoing.child}
      </div>
    );
    return (
      <div className={`message-list-stage speed-${speed}`} style={stageStyle}>
        <div
          className={`message-list-track slide-${outgoing.direction}`}
          onAnimationEnd={() => setOutgoing((current) => current?.key === outgoing.key ? null : current)}
        >
          {outgoing.direction === "right" ? incomingPane : outgoingPane}
          {outgoing.direction === "right" ? outgoingPane : incomingPane}
        </div>
      </div>
    );
  }
  return (
    <div className={`message-list-stage speed-${speed}`} style={stageStyle}>
      <div className="message-list-pane" key={stageKey} ref={currentPane}>
        {children}
      </div>
    </div>
  );
}

function messageDragPreview(conversations: Conversation[], ids: number[]) {
  if (typeof document === "undefined" || ids.length === 0) return null;
  const idSet = new Set(ids);
  const rows = conversations.filter((conversation) => conversationTransferMessageIDs(conversation).some((id) => idSet.has(id)));
  const preview = document.createElement("div");
  preview.className = "message-drag-preview";
  preview.setAttribute("aria-hidden", "true");
  const count = ids.length;
  const title = document.createElement("div");
  title.className = "message-drag-preview-count";
  title.textContent = messageCountLabel(count);
  preview.appendChild(title);
  rows.slice(0, 4).forEach((conversation) => {
    const line = document.createElement("div");
    line.className = "message-drag-preview-row";
    const sender = conversation.participants || conversation.message.from_addr || "Unknown sender";
    const subject = conversation.message.subject || "(no subject)";
    line.textContent = `${sender} - ${subject}`;
    preview.appendChild(line);
  });
  if (count > rows.length || count > 4) {
    const more = document.createElement("div");
    more.className = "message-drag-preview-more";
    more.textContent = `+${Math.max(0, count - Math.min(rows.length, 4)).toLocaleString()} more`;
    preview.appendChild(more);
  }
  document.body.appendChild(preview);
  return preview;
}

function uniquePositiveIDs(ids: number[]): number[] {
  return Array.from(new Set(ids.filter((id) => Number.isFinite(id) && id > 0)));
}

function conversationTransferMessageIDs(conversation: Conversation): number[] {
  const ids = conversation.message_ids && conversation.message_ids.length > 0 ? conversation.message_ids : [conversation.message.id];
  return uniquePositiveIDs(ids);
}

function conversationTransferAccountIDs(conversation: Conversation): number[] {
  const ids = conversation.message_account_ids && conversation.message_account_ids.length > 0
    ? conversation.message_account_ids
    : [conversation.message.account_id];
  return uniquePositiveIDs(ids);
}

/**
 * QueuedMoveGroup pairs the background runs a move was handed to with the
 * messages those runs carry. A move can start a run per account and per chunk,
 * and the runs do not have to end the same way, so the rows are settled one
 * group at a time rather than on whichever result came back first.
 */
type QueuedMoveGroup = { runIDs: number[]; ids: number[] };

/**
 * RowMoveAction names the row commands that relocate a whole conversation into
 * one folder. Spam is a move like the other two: reporting it files the mail in
 * the account's Junk folder, which is what keeps it out of Inbox, All Mail, and
 * every category list.
 */
type RowMoveAction = "trash" | "archive" | "spam";

/** rowMoveVerb names an action inside a sentence about what could not be done. */
function rowMoveVerb(action: RowMoveAction): string {
  return action === "spam" ? "report" : action;
}

/** rowMoveLabel names a failed action the way its button did. */
function rowMoveLabel(action: RowMoveAction): string {
  switch (action) {
  case "trash":
    return "Move to trash";
  case "spam":
    return "Report spam";
  default:
    return "Archive";
  }
}

/** rowMoveMissingTargetHint says which folder is missing and where to set it. */
function rowMoveMissingTargetHint(action: RowMoveAction): string {
  switch (action) {
  case "trash":
    return "Choose a Trash folder for this account before moving messages to Trash.";
  case "spam":
    return "This account has no Junk folder to report spam into.";
  default:
    return "Choose an Archive folder for this account in swipe settings.";
  }
}

type ConversationReadState = {
  id: number;
  read: boolean;
};

/**
 * MessageListScope describes the filter a list is showing so a selection can
 * mean "everything this filter matches" instead of only the loaded page. The
 * server resolves it again on delete; `total` is only for the button label and
 * is absent for searches, whose match count is not counted up front.
 */
type MessageListScope = {
  mailboxID: number;
  query: string;
  /** Names the whole-account list so a delete matches exactly what it shows. */
  view?: MailView;
  label: string;
  total?: number;
};

/** TrashMoveGroup collects the selected rows headed for one Trash mailbox. */
type TrashMoveGroup = {
  target: Mailbox;
  messageIDs: number[];
  items: { rowID: number; messageIDs: number[] }[];
};

// The backend queues a background run for bulk moves above 5 message IDs and
// stops reporting per-message progress; smaller moves run one message at a
// time so a partial failure restores only the rows that did not move.
const inlineMoveMessageLimit = 5;

// Keepalive request bodies share a 64 KiB browser quota; a 1000-ID move body
// is roughly 10 KiB, so background commits dispatch at most this many chunks —
// anything beyond the quota would be rejected by the browser anyway.
const keepaliveMoveChunkBudget = 6;

// A queued move hides its rows until the background run has actually moved them.
// The run is polled rather than assumed: a failed or interrupted run has to give
// the rows back. The limit only bounds the hiding, not the run itself.
const queuedMoveWatchIntervalMS = 5000;
const queuedMoveWatchLimitMS = 10 * 60 * 1000;

const messageSwipeMaxDistance = 112;
const messageSwipeCommitDistance = 68;
const messageSwipeCommitHoldMS = 170;
const messageSwipeSettleMS = 210;
const messageSwipeExitMS = 320;

type MessageSwipeState = {
  id: number;
  deltaX: number;
  visualDeltaX: number;
  direction: "start" | "end";
  phase: "tracking" | "committing" | "settling" | "exiting";
  committed: boolean;
  rowHeight?: number;
};

function messageSwipeAffordanceStyle(state: MessageSwipeState): CSSProperties | undefined {
  const deltaX = state.visualDeltaX;
  const distance = Math.abs(deltaX);
  if (distance === 0) return undefined;
  const progress = Math.min(distance / messageSwipeCommitDistance, 1);
  const overshoot = Math.min(Math.max(distance - messageSwipeCommitDistance, 0) / (messageSwipeMaxDistance - messageSwipeCommitDistance), 1);
  const shift = 12 * (1 - progress);
  const iconScale = progress < 1 ? .76 + (.24 * progress) : 1.08 - (.08 * overshoot);
  const labelProgress = Math.min(Math.max((distance - 18) / (messageSwipeCommitDistance - 18), 0), 1);
  const style = {
    "--swipe-action-content-opacity": (.28 + (.72 * progress)).toFixed(3),
    "--swipe-action-icon-scale": iconScale.toFixed(3),
    "--swipe-action-label-opacity": labelProgress.toFixed(3),
    "--swipe-action-start-shift": `-${shift.toFixed(1)}px`,
    "--swipe-action-end-shift": `${shift.toFixed(1)}px`
  } as CSSProperties & Record<string, string>;
  if (state.rowHeight) style["--swipe-row-height"] = `${state.rowHeight}px`;
  return style;
}

// MessageList is shared by mailbox and search pages. It owns local row selection,
// shift-select ranges, drag payloads, optimistic star updates, and message links.
function MessageList({
  csrf,
  conversations,
  hiddenMessageIDs,
  mailboxes,
  swipePreferences,
  archiveMailboxes = [],
  highlightMessageIDs,
  showRecipients = false,
  openAsDraft = false,
  searchQuery = "",
  datePrefs,
  returnURL = "",
  navigate,
  messageSecurityPlugins = [],
  addToast,
  onReadStatesChange,
  onMessagesMoved,
  onListChanged,
  listScope,
  snoozedView = false,
  currentMailboxID = 0,
  emptyState
}: {
  csrf: string;
  conversations: Conversation[];
  hiddenMessageIDs: Set<number>;
  mailboxes: Mailbox[];
  swipePreferences: SwipePreferences;
  /** Effective Archive folder per account: identity choice first, swipe mapping otherwise. */
  archiveMailboxes?: AccountMailboxChoice[];
  highlightMessageIDs?: Set<number>;
  showRecipients?: boolean;
  openAsDraft?: boolean;
  searchQuery?: string;
  datePrefs: DatePrefs;
  returnURL?: string;
  navigate: (url: string) => void;
  messageSecurityPlugins?: RuntimePlugin[];
  addToast: AddToast;
  onReadStatesChange: (states: ConversationReadState[]) => void;
  onMessagesMoved: (messageIDs: number[]) => void;
  /** Reload the current page: rows removed here are replaced by the next ones. */
  onListChanged?: () => void;
  /** The filter this list shows, which enables whole-filter selection. */
  listScope?: MessageListScope;
  snoozedView?: boolean;
  /** The mailbox this list is showing, so Delete can skip rows already in it. */
  currentMailboxID?: number;
  emptyState?: ReactNode;
}) {
  const [selectedIDs, setSelectedIDs] = useState<Set<number>>(() => new Set());
  const [dismissedIDs, setDismissedIDs] = useState<Set<number>>(() => new Set());
  const [readStateBusy, setReadStateBusy] = useState(false);
  const [snoozeBusy, setSnoozeBusy] = useState(false);
  // A counter rather than a boolean: deferred delete commits can overlap when
  // a second selection is deleted during the first commit's undo window.
  const [trashOps, setTrashOps] = useState(0);
  const [swipeActionBusy, setSwipeActionBusy] = useState(false);
  const [pendingSwipeMoveIDs, setPendingSwipeMoveIDs] = useState<Set<number>>(() => new Set());
  const [pendingSwipeSnoozeIDs, setPendingSwipeSnoozeIDs] = useState<Set<number>>(() => new Set());
  const [pendingSwipeReadStates, setPendingSwipeReadStates] = useState<Map<number, boolean>>(() => new Map());
  const [swipeState, setSwipeState] = useState<MessageSwipeState | null>(null);
  const [keyboardIndex, setKeyboardIndex] = useState<number | null>(null);
  // scopeSelected means the selection is the filter itself, not a set of rows.
  // Any per-row selection change drops back to the concrete row selection, so
  // the two modes never disagree about what is selected.
  const [scopeSelected, setScopeSelected] = useState(false);
  const [scopeDeletePending, setScopeDeletePending] = useState(false);
  const [scopeDeleteBusy, setScopeDeleteBusy] = useState(false);
  const selectionAnchorID = useRef<number | null>(null);
  const moveOutTimers = useRef<Map<number, number>>(new Map());
  const swipeCompletionTimer = useRef<number | null>(null);
  const pendingSwipeActionIDs = useRef<Set<number>>(new Set());
  const rowRefs = useRef<Map<number, HTMLDivElement>>(new Map());
  const swipeDismissTimers = useRef<Map<number, number>>(new Map());
  // Rows this list dismissed itself, as opposed to the ones it dismissed on
  // behalf of the app-wide hidden set. Only the latter come back when that hide
  // is released - a thread move's undo, a failed drag, a category filing that
  // finished - because a dismissal of its own is answered by its own mutation.
  const selfDismissedIDs = useRef<Set<number>>(new Set());
  const keyboardIndexRef = useRef<number | null>(null);
  // Set on unmount so the queued-move watch stops touching state after the view
  // is gone; it outlives a single render by design.
  const unmounted = useRef(false);
  const scopeDeleteTrigger = useRef<HTMLButtonElement | null>(null);
  const scopeDeleteCancel = useRef<HTMLButtonElement | null>(null);
  const scopeDeleteConfirm = useRef<HTMLButtonElement | null>(null);
  const swipeSession = useRef<{ id: number; startX: number; startY: number; lastX: number; lastY: number; active: boolean; blocked: boolean } | null>(null);
  const suppressRowClickUntil = useRef(0);
  // selectionBusy gates every bulk row mutation (toolbar buttons, swipes, drags)
  // so concurrent moves cannot race each other on the same rows.
  const selectionBusy = readStateBusy || snoozeBusy || swipeActionBusy || scopeDeleteBusy || trashOps > 0;
  const visible = conversations
    .filter((conversation) => !dismissedIDs.has(conversation.message.id))
    .map((conversation) => {
      const pendingRead = pendingSwipeReadStates.get(conversation.message.id);
      if (pendingRead === undefined || pendingRead === conversation.is_read) return conversation;
      return { ...conversation, is_read: pendingRead, message: { ...conversation.message, is_read: pendingRead } };
    });
  const visibleKey = visible.map((conversation) => conversation.message.id).join(",");
  const sourceKey = conversations.map((conversation) => conversation.message.id).join(",");
  // Search results are ranked by match rather than by date, so they carry no
  // date sections — the same rule Gmail follows. Derived here rather than asked
  // of every caller, so a new list cannot be wired up without its headings.
  const groupByDate = !searchQuery;
  // Headings depend only on the rows' dates and on which day it is. Selection,
  // swipes and drags re-render this list without moving a row, and a swipe
  // re-renders it per touch event, so they are computed once per page rather
  // than per render. The key carries the dates themselves, not just the row
  // ids: a refresh can hand back the same messages with a reminder that has
  // since come due. It carries the day too, so a list left open past midnight
  // stops calling yesterday's mail "Today".
  const sectionKey = groupByDate
    ? `${localDayKey()}|${visible.map((conversation) => `${conversation.message.id}:${rowDate(conversation, snoozedView)}`).join(",")}`
    : "";
  const sectionHeadings = useMemo(
    () => groupByDate ? dateSectionHeadings(visible, snoozedView) : [],
    [sectionKey, groupByDate, snoozedView]
  );
  const hiddenKey = Array.from(hiddenMessageIDs).sort((a, b) => a - b).join(",");
  const pendingSwipeMoveKey = Array.from(pendingSwipeMoveIDs).sort((a, b) => a - b).join(",");
  const pendingSwipeSnoozeKey = Array.from(pendingSwipeSnoozeIDs).sort((a, b) => a - b).join(",");
  const nativeTouchDrag = androidNativeAvailable();
  const effectiveSwipePreferences = swipePreferences || defaultSwipePreferences();
  const leftSwipePresentation = swipeActionPresentation(effectiveSwipePreferences.left_action);
  const rightSwipePresentation = swipeActionPresentation(effectiveSwipePreferences.right_action);
  const selectedDragItems = selectedIDs.size > 0 ? visible.filter((conversation) => selectedIDs.has(conversation.message.id)) : [];
  const selectedDragMessageIDs = uniquePositiveIDs(selectedDragItems.flatMap(conversationTransferMessageIDs));
  const selectedDragAccountIDs = uniquePositiveIDs(selectedDragItems.flatMap(conversationTransferAccountIDs));

  keyboardIndexRef.current = keyboardIndex;

  useEffect(() => {
    return () => {
      unmounted.current = true;
      moveOutTimers.current.forEach((timer) => window.clearTimeout(timer));
      moveOutTimers.current.clear();
      swipeDismissTimers.current.forEach((timer) => window.clearTimeout(timer));
      swipeDismissTimers.current.clear();
      if (swipeCompletionTimer.current !== null) window.clearTimeout(swipeCompletionTimer.current);
    };
  }, []);

  useEffect(() => {
    const sourceIDs = new Set(conversations.map((conversation) => conversation.message.id));
    const sourceMessageIDs = new Set(conversations.flatMap(conversationTransferMessageIDs));
    // A row a mutation dismissed stays dismissed for as long as the list still
    // carries it, and is released only when the list stops returning it or when
    // something explicitly puts it back: an undo, a failed move, or a queued
    // move this view gave up watching. Releasing it because the mutation
    // finished tied the row to the round trip behind it - a queued move ends
    // minutes after the click, and the rows returned to the screen for the gap
    // between that end and the reload that finally drops them, which is exactly
    // the flash the dismissal exists to prevent. The set stays bounded by the
    // list either way: an id the server stops sending leaves it on the next page.
    setDismissedIDs((current) => {
      const next = new Set<number>();
      current.forEach((id) => {
        if (sourceMessageIDs.has(id)) next.add(id);
      });
      return next.size === current.size ? current : next;
    });
    selfDismissedIDs.current.forEach((id) => {
      if (!sourceMessageIDs.has(id)) selfDismissedIDs.current.delete(id);
    });
    setPendingSwipeMoveIDs((current) => {
      const next = new Set(Array.from(current).filter((id) => sourceMessageIDs.has(id)));
      return next.size === current.size ? current : next;
    });
    setPendingSwipeSnoozeIDs((current) => {
      const next = new Set(Array.from(current).filter((id) => sourceMessageIDs.has(id)));
      return next.size === current.size ? current : next;
    });
    const releasedFromHide: number[] = [];
    sourceIDs.forEach((id) => {
      if (hiddenMessageIDs.has(id)) {
        if (!moveOutTimers.current.has(id)) {
          const timer = window.setTimeout(() => {
            moveOutTimers.current.delete(id);
            setDismissedIDs((current) => {
              const next = new Set(current);
              next.add(id);
              return next;
            });
          }, 230);
          moveOutTimers.current.set(id, timer);
        }
      } else {
        const timer = moveOutTimers.current.get(id);
        if (timer !== undefined) {
          window.clearTimeout(timer);
          moveOutTimers.current.delete(id);
        }
        // The hide is gone and this list never dismissed the row itself, so the
        // hide was the only reason it was not on screen: the move it stood for
        // was undone or failed, and the row belongs back in the list.
        if (dismissedIDs.has(id) && !selfDismissedIDs.current.has(id)) releasedFromHide.push(id);
      }
    });
    if (releasedFromHide.length > 0) restoreDismissed(releasedFromHide);
  }, [conversations, hiddenKey, pendingSwipeMoveKey, pendingSwipeSnoozeKey, sourceKey, hiddenMessageIDs, dismissedIDs]);

  // Focus lands on Cancel rather than the destructive control: a stray Enter on
  // a freshly opened confirmation must not delete a whole filter.
  useEffect(() => {
    if (scopeDeletePending) scopeDeleteCancel.current?.focus();
  }, [scopeDeletePending]);

  // A different folder or query is a different selection: whole-filter mode must
  // not survive the move, or Delete would act on a filter the user left.
  useEffect(() => {
    setScopeSelected(false);
    setScopeDeletePending(false);
  }, [listScope?.mailboxID, listScope?.query, listScope?.view]);

  useEffect(() => {
    const ids = new Set(visible.map((conversation) => conversation.message.id));
    setSelectedIDs((current) => {
      const next = new Set(Array.from(current).filter((id) => ids.has(id)));
      return next.size === current.size ? current : next;
    });
    if (selectionAnchorID.current !== null && !ids.has(selectionAnchorID.current)) {
      selectionAnchorID.current = null;
    }
    if (keyboardIndexRef.current !== null && keyboardIndexRef.current >= visible.length) {
      keyboardIndexRef.current = null;
      setKeyboardIndex(null);
    }
  }, [visibleKey]);

  useEffect(() => {
    function handleListShortcut(event: globalThis.KeyboardEvent) {
      if (event.shiftKey || shouldIgnoreMailShortcut(event) || visible.length === 0) return;
      const key = event.key.toLowerCase();
      if (key !== "j" && key !== "k" && key !== "x") return;
      event.preventDefault();
      const focusedRow = document.activeElement instanceof Element
        ? document.activeElement.closest<HTMLElement>("[data-rolltop-list-index]")
        : null;
      const focusedIndex = focusedRow ? Number.parseInt(focusedRow.dataset.rolltopListIndex || "", 10) : NaN;
      const currentIndex = Number.isFinite(focusedIndex) ? focusedIndex : keyboardIndexRef.current;
      const nextIndex = key === "j"
        ? currentIndex === null ? 0 : Math.min(visible.length - 1, currentIndex + 1)
        : key === "k"
          ? currentIndex === null ? visible.length - 1 : Math.max(0, currentIndex - 1)
          : currentIndex === null ? 0 : currentIndex;
      keyboardIndexRef.current = nextIndex;
      setKeyboardIndex(nextIndex);
      const messageID = visible[nextIndex].message.id;
      window.requestAnimationFrame(() => {
        const row = rowRefs.current.get(messageID);
        row?.focus({ preventScroll: true });
        row?.scrollIntoView({ block: "nearest" });
      });
      if (key === "x" && !event.repeat) {
        setScopeSelected(false);
        setScopeDeletePending(false);
        setSelectedIDs((current) => {
          const next = new Set(current);
          if (next.has(messageID)) next.delete(messageID);
          else next.add(messageID);
          return next;
        });
        selectionAnchorID.current = messageID;
      }
    }
    window.addEventListener("keydown", handleListShortcut);
    return () => window.removeEventListener("keydown", handleListShortcut);
  }, [visibleKey]);

  function selectedDragConversations(conversation: Conversation): Conversation[] {
    if (!selectedIDs.has(conversation.message.id)) return [conversation];
    const selected = visible.filter((item) => selectedIDs.has(item.message.id));
    return selected.length > 0 ? selected : [conversation];
  }

  function startMessageDrag(event: DragEvent<HTMLDivElement>, conversation: Conversation) {
    const selected = selectedDragConversations(conversation);
    const ids = uniquePositiveIDs(selected.flatMap(conversationTransferMessageIDs));
    const accountIDs = uniquePositiveIDs(selected.flatMap(conversationTransferAccountIDs));
    event.dataTransfer.effectAllowed = "copyMove";
    event.dataTransfer.setData("application/x-rolltop-message-transfer", JSON.stringify({ ids, account_ids: accountIDs }));
    event.dataTransfer.setData("application/x-rolltop-messages", JSON.stringify(ids));
    event.dataTransfer.setData("application/x-rolltop-message", String(ids[0]));
    event.dataTransfer.setData("text/plain", String(ids[0]));
    const dragImage = messageDragPreview(visible, ids);
    if (dragImage) {
      event.dataTransfer.setDragImage(dragImage, 18, 18);
      window.setTimeout(() => dragImage.remove(), 0);
    }
  }

  function selectMessage(event: MouseEvent<HTMLInputElement>, index: number, messageID: number) {
    event.stopPropagation();
    const checked = event.currentTarget.checked;
    const anchorIndex = event.shiftKey && selectionAnchorID.current !== null
      ? visible.findIndex((conversation) => conversation.message.id === selectionAnchorID.current)
      : -1;
    exitScopeSelection();
    setSelectedIDs((current) => {
      const next = new Set(current);
      if (anchorIndex >= 0) {
        const start = Math.min(anchorIndex, index);
        const end = Math.max(anchorIndex, index);
        for (const conversation of visible.slice(start, end + 1)) {
          if (checked) next.add(conversation.message.id);
          else next.delete(conversation.message.id);
        }
      } else if (checked) {
        next.add(messageID);
      } else {
        next.delete(messageID);
      }
      return next;
    });
    if (!event.shiftKey || anchorIndex < 0) selectionAnchorID.current = messageID;
  }

  function clearSelection() {
    setSelectedIDs(new Set());
    selectionAnchorID.current = null;
    exitScopeSelection();
  }

  function selectAllOnPage() {
    setSelectedIDs(new Set(visible.map((conversation) => conversation.message.id)));
    selectionAnchorID.current = null;
  }

  // Leaving whole-filter mode also drops a pending confirmation: the selection it
  // was asking about no longer exists.
  function exitScopeSelection() {
    setScopeSelected(false);
    setScopeDeletePending(false);
  }

  // The confirmation is modal over an irreversible action, so it owns the
  // keyboard while it is open: Escape backs out, Tab cannot wander behind the
  // backdrop, and whichever way it closes, focus returns to the button that
  // opened it.
  function closeScopeDeleteDialog() {
    setScopeDeletePending(false);
    scopeDeleteTrigger.current?.focus();
  }

  function handleScopeDeleteKeys(event: KeyboardEvent<HTMLElement>) {
    if (event.key === "Escape") {
      event.stopPropagation();
      closeScopeDeleteDialog();
      return;
    }
    if (event.key !== "Tab") return;
    const cancel = scopeDeleteCancel.current;
    const confirm = scopeDeleteConfirm.current;
    if (!cancel || !confirm) return;
    event.preventDefault();
    const forward = !event.shiftKey;
    const active = document.activeElement;
    if (forward) (active === cancel ? confirm : cancel).focus();
    else (active === confirm ? cancel : confirm).focus();
  }

  function selectWholeScope() {
    if (!listScope || selectionBusy) return;
    setSelectedIDs(new Set(visible.map((conversation) => conversation.message.id)));
    selectionAnchorID.current = null;
    setScopeSelected(true);
  }

  // The whole-filter delete hands the server the filter instead of message IDs.
  // It cannot be deferred behind an undo toast: the resolved set is far larger
  // than a page, so the confirmation happens before anything moves.
  async function deleteWholeScope() {
    if (!listScope || scopeDeleteBusy) return;
    setScopeDeletePending(false);
    setScopeDeleteBusy(true);
    try {
      const result = await api.scopeTrashMessages(csrf, { mailboxID: listScope.mailboxID, query: listScope.query, view: listScope.view });
      const queuedMessages = result.queued_messages || 0;
      // One action, one summary: the counts belong in a single sentence rather
      // than a stack of toasts the user has to read in order.
      const parts: string[] = [];
      if (queuedMessages > 0) parts.push(`Moving ${messageCountLabel(queuedMessages)} to Trash. This continues in the background.`);
      else if (result.skipped > 0) parts.push("Every matching message is already in Trash.");
      else parts.push("This filter has no messages left to delete.");
      if (queuedMessages > 0 && result.skipped > 0) parts.push(`${messageCountLabel(result.skipped)} already there were skipped.`);
      if (result.truncated) parts.push(`This pass covers ${messageCountLabel(result.matched)} — repeat the delete to continue with the rest.`);
      addToast(parts.join(" "));
      if (result.partial_error) addToast(result.partial_error, "error");
      clearSelection();
      // Every row on screen matches the filter the server just took, so the page
      // clears on the click rather than message by message as the background
      // runs work through the folder. Only a pass that took the whole filter can
      // say that: a truncated one stops somewhere inside it, a skipped message
      // is one this list may well be showing because it is already in Trash, and
      // a partial start leaves a whole account's mail where it was, so none of
      // them may hide a row the folder is going to keep. The runs are the only
      // proof the rows really left, and a delete that fails halfway has to give
      // them back rather than leave a folder looking emptier than it is, so a
      // pass that named no run to watch clears nothing either.
      const runsByAccount = new Map<number, number[]>();
      for (const run of result.runs || []) {
        if (run.run_id <= 0) continue;
        runsByAccount.set(run.account_id, [...(runsByAccount.get(run.account_id) || []), run.run_id]);
      }
      const wholeFilterTaken = queuedMessages > 0 && result.skipped === 0 && !result.truncated
        && !result.partial_error && runsByAccount.size > 0;
      // The rows are grouped by the account whose run took them, so a run that
      // fails gives back exactly the mail it left where it was. A row belonging
      // to an account this pass started no run for is never cleared at all.
      const idsByAccount = new Map<number, number[]>();
      if (wholeFilterTaken) {
        for (const conversation of visible) {
          const ids = [conversation.message.id, ...conversationTransferMessageIDs(conversation)];
          for (const accountID of conversationTransferAccountIDs(conversation)) {
            if (!runsByAccount.has(accountID)) continue;
            idsByAccount.set(accountID, [...(idsByAccount.get(accountID) || []), ...ids]);
          }
        }
      }
      const groups: QueuedMoveGroup[] = Array.from(idsByAccount, ([accountID, ids]) => ({
        runIDs: runsByAccount.get(accountID) || [],
        ids: uniquePositiveIDs(ids)
      }));
      const clearedIDs = uniquePositiveIDs(groups.flatMap((group) => group.ids));
      if (clearedIDs.length > 0) {
        optimisticallyDismiss(clearedIDs);
        void watchQueuedMove(groups, "Trash");
      }
      onListChanged?.();
    } catch (err) {
      addToast(`Delete failed: ${messageFromError(err)}`, "error");
    } finally {
      setScopeDeleteBusy(false);
    }
  }

  async function markSelectedRead(read: boolean) {
    const selected = visible.filter((conversation) => selectedIDs.has(conversation.message.id));
    const messageIDs = uniquePositiveIDs(selected.flatMap(conversationTransferMessageIDs));
    if (messageIDs.length === 0 || selectionBusy) return;
    const previous = selected.map((conversation) => ({ id: conversation.message.id, read: conversation.is_read }));
    onReadStatesChange(selected.map((conversation) => ({ id: conversation.message.id, read })));
    setReadStateBusy(true);
    try {
      await api.bulkRead(csrf, messageIDs, read);
    } catch (err) {
      onReadStatesChange(previous);
      addToast(`${read ? "Mark read" : "Mark unread"} failed: ${messageFromError(err)}`, "error");
    } finally {
      setReadStateBusy(false);
    }
  }

  // Deleting moves the selection into each account's Trash mailbox through the
  // same IMAP-mirrored move APIs as drag and swipe. The mutation is deferred
  // behind an undo toast; the commit tracks per-message progress so a partial
  // failure only restores rows whose messages did not move.
  function deleteSelected() {
    if (selectionBusy) return;
    const selected = visible.filter((conversation) =>
      selectedIDs.has(conversation.message.id) && !pendingSwipeActionIDs.current.has(conversation.message.id));
    if (selected.length === 0) {
      if (selectedIDs.size > 0) addToast("Selected messages are still finishing another action.", "error");
      return;
    }
    const groups = new Map<number, TrashMoveGroup>();
    const skippedTrashNames = new Set<string>();
    let alreadyInTrash = 0;
    for (const conversation of selected) {
      const accountIDs = conversationTransferAccountIDs(conversation);
      if (accountIDs.length !== 1) {
        addToast("Cannot delete a conversation containing messages from multiple accounts.", "error");
        return;
      }
      const target = trashMailboxForAccount(mailboxes, accountIDs[0]);
      if (!target) {
        addToast("Choose a Trash folder for this account before deleting messages.", "error");
        return;
      }
      const messageIDs = conversationTransferMessageIDs(conversation);
      // Skip rows that are certainly already in Trash: any row while viewing
      // that Trash folder, or a single-message row whose message lives there.
      // Multi-message threads elsewhere still move — their representative
      // message may sit in Trash while older messages remain in the Inbox.
      if (target.id === currentMailboxID || (messageIDs.length === 1 && conversation.message.mailbox_id === target.id)) {
        alreadyInTrash++;
        skippedTrashNames.add(target.name);
        continue;
      }
      const group = groups.get(target.id) || { target, messageIDs: [], items: [] };
      group.items.push({ rowID: conversation.message.id, messageIDs });
      group.messageIDs.push(...messageIDs);
      groups.set(target.id, group);
    }
    const skippedLabel = skippedTrashNames.size === 1 ? Array.from(skippedTrashNames)[0] : "Trash";
    if (groups.size === 0) {
      addToast(alreadyInTrash === 1 ? `Message is already in ${skippedLabel}.` : `Messages are already in ${skippedLabel}.`);
      return;
    }
    const entries = Array.from(groups.values());
    for (const entry of entries) entry.messageIDs = uniquePositiveIDs(entry.messageIDs);
    const destLabel = entries.length === 1 ? entries[0].target.name : "Trash";
    const rowIDs = entries.flatMap((entry) => entry.items.map((item) => item.rowID));
    // Dismiss every thread message ID too, so sibling rows of the same thread
    // are hidden and locked during the undo window, matching the swipe path.
    const dismissIDs = uniquePositiveIDs([...rowIDs, ...entries.flatMap((entry) => entry.messageIDs)]);
    const totalMessages = entries.reduce((sum, entry) => sum + entry.messageIDs.length, 0);
    const registered = deferSwipeMutation(
      rowIDs[0],
      `Moved ${messageCountLabel(totalMessages)} to ${destLabel}.`,
      () => {
        removePendingSwipeMoveIDs(dismissIDs);
        restoreDismissed(dismissIDs);
        setSelectedIDs((current) => new Set([...current, ...rowIDs]));
      },
      (keepalive) => commitTrashMove(entries, dismissIDs, destLabel, keepalive)
    );
    if (!registered) return;
    if (alreadyInTrash > 0) addToast(`Skipped ${messageCountLabel(alreadyInTrash)} already in ${skippedLabel}.`);
    setPendingSwipeMoveIDs((current) => new Set([...current, ...dismissIDs]));
    optimisticallyDismiss(dismissIDs);
    clearSelection();
  }

  async function commitTrashMove(entries: TrashMoveGroup[], dismissIDs: number[], destLabel: string, keepalive: boolean) {
    // On a background commit the unsnooze requests must reach the browser
    // before any await, or the unload cancels them; deleting a snoozed message
    // includes dismissing its reminder, so this does not wait for the moves.
    if (snoozedView && keepalive) {
      void Promise.allSettled(entries.flatMap((entry) =>
        entry.items.map((item) => api.unsnoozeMessage(csrf, item.rowID, { keepalive: true }))));
    }
    const movedMessageIDs: number[] = [];
    const stillQueuedIDs = new Set<number>();
    const queuedGroups: QueuedMoveGroup[] = [];
    const movedRowIDs: number[] = [];
    const restoreIDs: number[] = [];
    const reselectRowIDs: number[] = [];
    let queuedMessages = 0;
    let firstError: unknown;
    setTrashOps((count) => count + 1);
    try {
      await Promise.all(entries.map(async (entry) => {
        const { movedIDs, queuedIDs, queuedGroups: groups, queuedCount, error } = await executeMailboxMove(entry.target, entry.messageIDs, keepalive);
        if (error !== undefined && firstError === undefined) firstError = error;
        queuedIDs.forEach((id) => stillQueuedIDs.add(id));
        queuedGroups.push(...groups);
        queuedMessages += queuedCount;
        movedMessageIDs.push(...movedIDs);
        const movedSet = new Set(movedIDs);
        for (const item of entry.items) {
          // The reminder belongs to the row's own message, so it only goes when
          // that message is one of the messages this move relocated.
          if (movedSet.has(item.rowID)) movedRowIDs.push(item.rowID);
          const stayedIDs = item.messageIDs.filter((id) => !movedSet.has(id));
          if (stayedIDs.length === 0) continue;
          // Whatever stayed in the folder is still this list's mail, even when a
          // sibling in the same thread moved: a thread with messages left here is
          // a row the list still has to show, and nothing else releases the
          // dismissal now that it outlives the mutation.
          restoreIDs.push(item.rowID, ...stayedIDs);
          reselectRowIDs.push(item.rowID);
        }
      }));
    } finally {
      setTrashOps((count) => Math.max(0, count - 1));
    }
    if (snoozedView && !keepalive && movedRowIDs.length > 0) {
      void Promise.allSettled(movedRowIDs.map((rowID) => api.unsnoozeMessage(csrf, rowID)));
    }
    // A queued move is only accepted, not done: its messages still sit in the
    // source folder until the background run reaches them, so only the messages
    // that really moved are reported moved. Taking a queued row out of the list
    // would drop the dismissal that is keeping it off the screen - a dismissal
    // lasts while the list carries the row - and the reload below would hand it
    // back as a row nothing is hiding any more. The watch reports them instead,
    // once the run proves they left.
    const settledMovedIDs = movedMessageIDs.filter((id) => !stillQueuedIDs.has(id));
    if (settledMovedIDs.length > 0) onMessagesMoved(settledMovedIDs);
    // They stay pending so a reload of this page does not show them again on
    // their way out.
    removePendingSwipeMoveIDs(dismissIDs.filter((id) => !stillQueuedIDs.has(id)));
    if (restoreIDs.length > 0) {
      restoreDismissed(uniquePositiveIDs(restoreIDs));
      setSelectedIDs((current) => new Set([...current, ...reselectRowIDs]));
    }
    if (queuedMessages > 0) addToast(`Move to ${destLabel} started for ${messageCountLabel(queuedMessages)}.`);
    if (firstError !== undefined) addToast(`Delete failed: ${messageFromError(firstError)}`, "error");
    // The rows are gone from this page; reload it so the following messages move
    // up instead of leaving the page short, or empty after a full-page delete.
    if (movedMessageIDs.length > 0) onListChanged?.();
    if (!keepalive && queuedGroups.length > 0) {
      void watchQueuedMove(queuedGroups, destLabel);
    }
  }

  // executeMailboxMove pushes messageIDs into the target mailbox and reports
  // which messages moved (or were queued as a background run) so callers can
  // reconcile rows without guessing. Shared by swipe moves and bulk delete.
  async function executeMailboxMove(target: Mailbox, messageIDs: number[], keepalive: boolean): Promise<{ movedIDs: number[]; queuedIDs: number[]; queuedGroups: QueuedMoveGroup[]; queuedCount: number; error?: unknown }> {
    if (keepalive || messageIDs.length > inlineMoveMessageLimit) {
      // Chunk here (within the backend's batch cap) so each chunk's outcome is
      // tracked independently: one failed chunk must not discard the moved IDs
      // of chunks the backend already accepted.
      const chunks: number[][] = [];
      for (let start = 0; start < messageIDs.length; start += bulkMessageIDLimit) {
        chunks.push(messageIDs.slice(start, start + bulkMessageIDLimit));
      }
      const dispatched = keepalive ? chunks.slice(0, keepaliveMoveChunkBudget) : chunks;
      const results = await Promise.allSettled(dispatched.map((chunk) =>
        api.bulkMoveMessages(csrf, chunk, target.id, keepalive ? { keepalive: true } : undefined)));
      const movedIDs: number[] = [];
      const queuedIDs: number[] = [];
      const queuedGroups: QueuedMoveGroup[] = [];
      let queuedCount = 0;
      let error: unknown;
      results.forEach((result, index) => {
        if (result.status === "fulfilled") {
          movedIDs.push(...dispatched[index]);
          if (result.value.queued) {
            queuedIDs.push(...dispatched[index]);
            // This chunk's messages and the runs that took them travel together,
            // so a mixed outcome can be settled one run's messages at a time.
            queuedGroups.push({ runIDs: result.value.run_ids, ids: dispatched[index] });
            queuedCount += dispatched[index].length;
          }
        } else if (error === undefined) {
          error = result.reason;
        }
      });
      return { movedIDs, queuedIDs, queuedGroups, queuedCount, error };
    }
    const movedIDs: number[] = [];
    let error: unknown;
    for (const messageID of messageIDs) {
      try {
        await api.moveMessage(csrf, messageID, target.id);
        movedIDs.push(messageID);
      } catch (err) {
        if (error === undefined) error = err;
      }
    }
    return { movedIDs, queuedIDs: [], queuedGroups: [], queuedCount: 0, error };
  }

  function optimisticallyDismiss(ids: number[]) {
    ids.forEach((id) => selfDismissedIDs.current.add(id));
    setDismissedIDs((current) => new Set([...current, ...ids]));
    setSelectedIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function restoreDismissed(ids: number[]) {
    ids.forEach((id) => selfDismissedIDs.current.delete(id));
    setDismissedIDs((current) => {
      const next = new Set(current);
      let changed = false;
      ids.forEach((id) => {
        if (next.delete(id)) changed = true;
      });
      return changed ? next : current;
    });
  }

  function settleSwipeRow(messageID: number) {
    setSwipeState((current) => current?.id === messageID
      ? { ...current, deltaX: 0, phase: "settling", committed: false }
      : current);
    clearSwipeStateAfter(messageID, messageSwipeSettleMS);
  }

  function beginSwipeDismiss(messageID: number, ids: number[], direction: "start" | "end") {
    const existingTimer = swipeDismissTimers.current.get(messageID);
    if (existingTimer !== undefined) window.clearTimeout(existingTimer);
    const reduceMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    if (reduceMotion) {
      optimisticallyDismiss(ids);
      setSwipeState((current) => current?.id === messageID ? null : current);
      return;
    }
    const row = rowRefs.current.get(messageID);
    const bounds = row?.getBoundingClientRect();
    const distance = Math.max(bounds?.width || 0, window.innerWidth) + 24;
    const rowHeight = Math.ceil(bounds?.height || row?.offsetHeight || 72);
    const exitDelta = direction === "start" ? distance : -distance;
    setSelectedIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
    setSwipeState((current) => current?.id === messageID
      ? {
          ...current,
          deltaX: exitDelta,
          visualDeltaX: exitDelta,
          direction,
          phase: "exiting",
          committed: true,
          rowHeight
        }
      : current);
    const timer = window.setTimeout(() => {
      swipeDismissTimers.current.delete(messageID);
      optimisticallyDismiss(ids);
      setSwipeState((current) => current?.id === messageID ? null : current);
    }, messageSwipeExitMS);
    swipeDismissTimers.current.set(messageID, timer);
  }

  function cancelSwipeDismiss(messageID: number) {
    const timer = swipeDismissTimers.current.get(messageID);
    if (timer !== undefined) {
      window.clearTimeout(timer);
      swipeDismissTimers.current.delete(messageID);
      settleSwipeRow(messageID);
    }
  }

  /**
   * watchQueuedMove keeps queued rows hidden while their background runs work
   * through them, then settles each group against its own runs: messages a
   * finished run proves gone leave the list, and messages a failed run left
   * where they were come back with the reason.
   */
  async function watchQueuedMove(groups: QueuedMoveGroup[], destLabel: string) {
    const watched = groups.filter((group) => group.ids.length > 0);
    const ids = uniquePositiveIDs(watched.flatMap((group) => group.ids));
    if (ids.length === 0) return;
    const runIDs = uniquePositiveIDs(watched.flatMap((group) => group.runIDs));
    if (runIDs.length === 0) {
      removePendingSwipeMoveIDs(ids);
      return;
    }
    const deadline = Date.now() + queuedMoveWatchLimitMS;
    while (Date.now() < deadline) {
      await new Promise((resolve) => window.setTimeout(resolve, queuedMoveWatchIntervalMS));
      if (unmounted.current) return;
      let runs: SyncRun[];
      try {
        runs = (await Promise.all(runIDs.map((runID) => api.syncRun(String(runID))))).map((data) => data.sync_run);
      } catch {
        // A failed status request says nothing about the move: try again.
        continue;
      }
      if (unmounted.current) return;
      if (runs.some((run) => run.status === "running")) continue;
      removePendingSwipeMoveIDs(ids);
      // Paired by request order rather than by the id a run reports back, so a
      // group is always read against the runs it was actually handed to.
      const runByID = new Map<number, SyncRun>();
      runIDs.forEach((runID, index) => {
        const run = runs[index];
        if (run) runByID.set(runID, run);
      });
      const proven: number[] = [];
      const returned: number[] = [];
      let failure: SyncRun | undefined;
      for (const group of watched) {
        // A group with no run behind it has nothing proving anything, so it is
        // never settled as moved: an empty list would otherwise pass `every`.
        const answered = group.runIDs.length > 0 && group.runIDs.every((runID) => runByID.has(runID));
        const failed = group.runIDs.map((runID) => runByID.get(runID)).find((run) => run !== undefined && run.status !== "ok");
        if (answered && failed === undefined) {
          // These runs are done, so their rows have left this list for real:
          // take them out of its own data now rather than leaving that to the
          // reload. The reload is a round trip away, and the folder refresh a
          // finished run queues competes with it, so waiting for it to answer is
          // what put moved mail back in front of the reader a minute after they
          // filed it.
          proven.push(...group.ids);
          continue;
        }
        returned.push(...group.ids);
        if (failure === undefined && failed !== undefined) failure = failed;
      }
      // A message two groups claim, one proven and one not, is put back: showing
      // mail that has already moved is recoverable, hiding mail that never did
      // is not. The reload right after settles the honest case either way.
      const returnedSet = new Set(returned);
      const provenIDs = proven.filter((id) => !returnedSet.has(id));
      if (provenIDs.length > 0) onMessagesMoved(provenIDs);
      if (returned.length > 0) {
        restoreDismissed(uniquePositiveIDs(returned));
        const reason = failure ? failure.error || failure.status : "the run did not report a result";
        addToast(`Move to ${destLabel} did not finish: ${reason}.`, "error");
      }
      onListChanged?.();
      return;
    }
    // Past the watch window the rows stop being hidden on trust: show whatever
    // the folder still holds rather than keeping them invisible for the session.
    // Nothing else releases them - a dismissal now outlives the mutation - so
    // this is the one place that has to put an unproven move back on screen.
    removePendingSwipeMoveIDs(ids);
    restoreDismissed(ids);
    onListChanged?.();
  }

  function removePendingSwipeMoveIDs(ids: number[]) {
    setPendingSwipeMoveIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function removePendingSwipeSnoozeIDs(ids: number[]) {
    setPendingSwipeSnoozeIDs((current) => {
      const next = new Set(current);
      ids.forEach((id) => next.delete(id));
      return next;
    });
  }

  function removePendingSwipeReadState(id: number) {
    setPendingSwipeReadStates((current) => {
      if (!current.has(id)) return current;
      const next = new Map(current);
      next.delete(id);
      return next;
    });
  }

  function deferSwipeMutation(
    conversationID: number,
    message: string,
    onUndo: () => void,
    onCommit: (keepalive: boolean) => Promise<void>
  ): boolean {
    if (pendingSwipeActionIDs.current.has(conversationID)) return false;
    pendingSwipeActionIDs.current.add(conversationID);
    let settled = false;
    addToast(message, "success", {
      onUndo: () => {
        if (settled) return;
        settled = true;
        pendingSwipeActionIDs.current.delete(conversationID);
        onUndo();
      },
      onCommit: (reason) => {
        if (settled) return;
        settled = true;
        void onCommit(reason === "background")
          .catch((err) => addToast(`Swipe action failed: ${messageFromError(err)}`, "error"))
          .finally(() => pendingSwipeActionIDs.current.delete(conversationID));
      }
    });
    return true;
  }

  async function snoozeConversations(items: Conversation[], until: Date) {
    const ids = uniquePositiveIDs(items.map((conversation) => conversation.message.id));
    if (ids.length === 0) return;
    if (selectionBusy) {
      addToast("Another action is still running. Try snoozing again in a moment.", "error");
      return;
    }
    if (!snoozedView) optimisticallyDismiss(ids);
    clearSelection();
    setSnoozeBusy(true);
    try {
      const results = await Promise.allSettled(ids.map((id) => api.snoozeMessage(csrf, id, until)));
      const failed = ids.filter((_, index) => results[index].status === "rejected");
      if (!snoozedView && failed.length > 0) restoreDismissed(failed);
      const succeeded = ids.length - failed.length;
      if (succeeded > 0) {
        addToast(`${succeeded === 1 ? "Message" : `${succeeded.toLocaleString()} messages`} snoozed until ${displaySnoozeUntil(until, datePrefs)}.`);
        onListChanged?.();
      }
      if (failed.length > 0) {
        const first = results.find((result) => result.status === "rejected");
        const reason = first?.status === "rejected" ? messageFromError(first.reason) : "Request failed";
        addToast(`${failed.length.toLocaleString()} snooze ${failed.length === 1 ? "request" : "requests"} failed: ${reason}`, "error");
        throw first?.status === "rejected" ? first.reason : new Error(reason);
      }
    } finally {
      setSnoozeBusy(false);
    }
  }

  async function unsnoozeConversations(items: Conversation[]) {
    const ids = uniquePositiveIDs(items.map((conversation) => conversation.message.id));
    if (ids.length === 0) return;
    if (selectionBusy) {
      addToast("Another action is still running. Try unsnoozing again in a moment.", "error");
      return;
    }
    optimisticallyDismiss(ids);
    clearSelection();
    setSnoozeBusy(true);
    try {
      const results = await Promise.allSettled(ids.map((id) => api.unsnoozeMessage(csrf, id)));
      const failed = ids.filter((_, index) => results[index].status === "rejected");
      if (failed.length > 0) restoreDismissed(failed);
      const succeeded = ids.length - failed.length;
      if (succeeded > 0) {
        addToast(`${succeeded === 1 ? "Message" : `${succeeded.toLocaleString()} messages`} returned to mail.`);
        onListChanged?.();
      }
      if (failed.length > 0) {
        const first = results.find((result) => result.status === "rejected");
        const reason = first?.status === "rejected" ? messageFromError(first.reason) : "Request failed";
        addToast(`${failed.length.toLocaleString()} unsnooze ${failed.length === 1 ? "request" : "requests"} failed: ${reason}`, "error");
      }
    } finally {
      setSnoozeBusy(false);
    }
  }

  function markConversationRead(conversation: Conversation, read: boolean) {
    const ids = conversationTransferMessageIDs(conversation);
    const previous = conversation.is_read;
    if (previous === read) {
      addToast(`Message is already ${read ? "read" : "unread"}.`);
      return;
    }
    const rowID = conversation.message.id;
    const registered = deferSwipeMutation(
      rowID,
      `Message marked ${read ? "read" : "unread"}.`,
      () => {
        removePendingSwipeReadState(rowID);
        onReadStatesChange([{ id: rowID, read: previous }]);
      },
      async (keepalive) => {
        try {
          await api.bulkRead(csrf, ids, read, { keepalive });
          onReadStatesChange([{ id: rowID, read }]);
        } catch (err) {
          onReadStatesChange([{ id: rowID, read: previous }]);
          addToast(`${read ? "Mark read" : "Mark unread"} failed: ${messageFromError(err)}`, "error");
        } finally {
          removePendingSwipeReadState(rowID);
        }
      }
    );
    if (registered) {
      setPendingSwipeReadStates((current) => new Map(current).set(rowID, read));
      onReadStatesChange([{ id: rowID, read }]);
      clearSelection();
    }
  }

  function snoozeConversationBySwipe(conversation: Conversation, until: Date, direction: "start" | "end"): boolean {
    const ids = [conversation.message.id];
    const rowID = conversation.message.id;
    const registered = deferSwipeMutation(
      rowID,
      `Message snoozed until ${displaySnoozeUntil(until, datePrefs)}.`,
      () => {
        cancelSwipeDismiss(rowID);
        if (!snoozedView) {
          removePendingSwipeSnoozeIDs(ids);
          restoreDismissed(ids);
        }
      },
      async (keepalive) => {
        try {
          await api.snoozeMessage(csrf, rowID, until, { keepalive });
          if (!snoozedView) {
            onMessagesMoved(ids);
            removePendingSwipeSnoozeIDs(ids);
            onListChanged?.();
          }
        } catch (err) {
          cancelSwipeDismiss(rowID);
          if (!snoozedView) {
            removePendingSwipeSnoozeIDs(ids);
            restoreDismissed(ids);
          }
          addToast(`Snooze failed: ${messageFromError(err)}`, "error");
        }
      }
    );
    if (!registered) return false;
    if (!snoozedView) {
      setPendingSwipeSnoozeIDs((current) => new Set([...current, ...ids]));
      beginSwipeDismiss(rowID, ids, direction);
    }
    clearSelection();
    return !snoozedView;
  }

  // Shared single-row move for swipes and the pointer row actions. `direction`
  // picks the dismissal: a swipe slides the row out toward the finger, while a
  // row action (direction null) collapses it the way bulk delete does.
  function moveConversation(conversation: Conversation, action: RowMoveAction, direction: "start" | "end" | null): boolean {
    const accountIDs = conversationTransferAccountIDs(conversation);
    if (accountIDs.length !== 1) {
      addToast(`Cannot ${rowMoveVerb(action)} a conversation containing messages from multiple accounts.`, "error");
      return false;
    }
    const accountID = accountIDs[0];
    const target = action === "trash"
      ? trashMailboxForAccount(mailboxes, accountID)
      : action === "spam"
        ? junkMailboxForAccount(mailboxes, accountID)
        : archiveMailboxForAccount(mailboxes, archiveMailboxes, accountID);
    if (!target) {
      addToast(rowMoveMissingTargetHint(action), "error");
      return false;
    }
    if (conversation.message.mailbox_id === target.id) {
      addToast(`This conversation is already in ${target.name}.`);
      return false;
    }
    const messageIDs = conversationTransferMessageIDs(conversation);
    const dismissedIDs = messageIDs;
    const registered = deferSwipeMutation(
      conversation.message.id,
      `Moved ${messageCountLabel(messageIDs.length)} to ${target.name}.`,
      () => {
        cancelSwipeDismiss(conversation.message.id);
        removePendingSwipeMoveIDs(dismissedIDs);
        restoreDismissed(dismissedIDs);
      },
      async (keepalive) => {
        // Deleting a snoozed row dismisses its reminder too. On a background
        // commit the request has to leave before any await, or unload cancels
        // it — but only once the move itself fits the keepalive chunk budget,
        // so a truncated move never drops the reminder of a message that stays.
        // Trash and spam end a conversation, so they dismiss its snooze reminder
        // too; archive only files it away, and a reminder there still makes sense.
        const unsnooze = snoozedView && action !== "archive";
        const keepaliveMoveComplete = messageIDs.length <= bulkMessageIDLimit * keepaliveMoveChunkBudget;
        if (unsnooze && keepalive && keepaliveMoveComplete) void api.unsnoozeMessage(csrf, conversation.message.id, { keepalive: true }).catch(() => undefined);
        const { movedIDs, queuedIDs, queuedGroups, error } = await executeMailboxMove(target, messageIDs, keepalive);
        const queued = new Set(queuedIDs);
        removePendingSwipeMoveIDs(dismissedIDs.filter((id) => !queued.has(id)));
        if (!keepalive && queuedGroups.length > 0) {
          void watchQueuedMove(queuedGroups, target.name);
        }
        // The reminder belongs to this row's own message, so a partial move that
        // relocated only sibling thread messages must leave it in place.
        if (unsnooze && !keepalive && movedIDs.includes(conversation.message.id)) void api.unsnoozeMessage(csrf, conversation.message.id).catch(() => undefined);
        // Queued messages are held back from onMessagesMoved for the same
        // reason as in the bulk path: they have not left the folder yet, and a
        // row taken out of the list loses the dismissal hiding it.
        const movedSet = new Set(movedIDs);
        const settledIDs = movedIDs.filter((id) => !queued.has(id));
        const stayedIDs = messageIDs.filter((id) => !movedSet.has(id));
        if (settledIDs.length > 0) onMessagesMoved(settledIDs);
        // A move that relocated part of the thread leaves the rest here, and the
        // row goes on standing for it, so the row comes back with the messages
        // that stayed. The reload below is what puts it on screen again:
        // reporting the moved messages took the whole row out of the list.
        if (stayedIDs.length > 0) {
          cancelSwipeDismiss(conversation.message.id);
          restoreDismissed(uniquePositiveIDs([conversation.message.id, ...stayedIDs]));
        }
        if (movedIDs.length > 0) onListChanged?.();
        if (error === undefined) return;
        const partial = movedIDs.length > 0 ? `${movedIDs.length.toLocaleString()} moved, but the remaining action failed` : `${rowMoveLabel(action)} failed`;
        addToast(`${partial}: ${messageFromError(error)}`, "error");
      }
    );
    if (!registered) return false;
    setPendingSwipeMoveIDs((current) => new Set([...current, ...dismissedIDs]));
    if (direction) beginSwipeDismiss(conversation.message.id, dismissedIDs, direction);
    else optimisticallyDismiss(dismissedIDs);
    return true;
  }

  async function executeSwipeAction(
    conversation: Conversation,
    action: SwipeAction,
    snoozePreset: SwipePreferences["left_snooze_preset"],
    direction: "start" | "end"
  ): Promise<boolean> {
    if (selectionBusy || pendingSwipeActionIDs.current.has(conversation.message.id)) return false;
    setSwipeActionBusy(true);
    try {
      switch (action) {
      case "mark_read":
        await markConversationRead(conversation, true);
        return false;
      case "mark_unread":
        await markConversationRead(conversation, false);
        return false;
      case "snooze":
        return snoozeConversationBySwipe(conversation, swipeSnoozeUntil(snoozePreset), direction);
      case "trash":
      case "archive":
        return moveConversation(conversation, action, direction);
      }
    } finally {
      setSwipeActionBusy(false);
    }
    return false;
  }

  function startRowSwipe(event: TouchEvent<HTMLDivElement>, conversation: Conversation) {
    if (selectionBusy || swipeState || pendingSwipeActionIDs.current.has(conversation.message.id) || !nativeTouchDrag || event.touches.length !== 1 || (event.target as HTMLElement).closest("button,input,label")) return;
    const touch = event.touches[0];
    swipeSession.current = { id: conversation.message.id, startX: touch.clientX, startY: touch.clientY, lastX: touch.clientX, lastY: touch.clientY, active: false, blocked: false };
  }

  function moveRowSwipe(event: TouchEvent<HTMLDivElement>) {
    const session = swipeSession.current;
    if (!session || event.touches.length !== 1) return;
    if (document.documentElement.classList.contains("rolltop-touch-message-dragging")) {
      swipeSession.current = null;
      setSwipeState(null);
      return;
    }
    const touch = event.touches[0];
    session.lastX = touch.clientX;
    session.lastY = touch.clientY;
    const deltaX = touch.clientX - session.startX;
    const deltaY = touch.clientY - session.startY;
    if (!session.active && !session.blocked) {
      if (Math.abs(deltaY) > 10 && Math.abs(deltaY) >= Math.abs(deltaX)) session.blocked = true;
      else if (Math.abs(deltaX) > 12 && Math.abs(deltaX) > Math.abs(deltaY) * 1.15) session.active = true;
    }
    if (!session.active) return;
    event.preventDefault();
    const clampedDeltaX = Math.max(-messageSwipeMaxDistance, Math.min(messageSwipeMaxDistance, deltaX));
    setSwipeState({
      id: session.id,
      deltaX: clampedDeltaX,
      visualDeltaX: clampedDeltaX,
      direction: clampedDeltaX > 0 ? "start" : "end",
      phase: "tracking",
      committed: false
    });
  }

  function clearSwipeStateAfter(messageID: number, delay: number) {
    if (swipeCompletionTimer.current !== null) window.clearTimeout(swipeCompletionTimer.current);
    swipeCompletionTimer.current = window.setTimeout(() => {
      swipeCompletionTimer.current = null;
      setSwipeState((current) => current?.id === messageID ? null : current);
    }, delay);
  }

  function finishRowSwipe(conversation: Conversation) {
    const session = swipeSession.current;
    swipeSession.current = null;
    if (document.documentElement.classList.contains("rolltop-touch-message-dragging")) {
      setSwipeState(null);
      return;
    }
    if (!session || !session.active) {
      setSwipeState(null);
      return;
    }
    const deltaX = session.lastX - session.startX;
    suppressRowClickUntil.current = Date.now() + 450;
    const direction = deltaX > 0 ? "start" : "end";
    const clampedDeltaX = Math.max(-messageSwipeMaxDistance, Math.min(messageSwipeMaxDistance, deltaX));
    if (Math.abs(deltaX) < messageSwipeCommitDistance) {
      setSwipeState({
        id: session.id,
        deltaX: 0,
        visualDeltaX: clampedDeltaX,
        direction,
        phase: "settling",
        committed: false
      });
      clearSwipeStateAfter(session.id, messageSwipeSettleMS);
      return;
    }
    const committedDeltaX = direction === "start" ? messageSwipeMaxDistance : -messageSwipeMaxDistance;
    const action = direction === "start" ? effectiveSwipePreferences.right_action : effectiveSwipePreferences.left_action;
    const snoozePreset = direction === "start" ? effectiveSwipePreferences.right_snooze_preset : effectiveSwipePreferences.left_snooze_preset;
    setSwipeState({
      id: session.id,
      deltaX: committedDeltaX,
      visualDeltaX: committedDeltaX,
      direction,
      phase: "committing",
      committed: true
    });
    if (swipeCompletionTimer.current !== null) window.clearTimeout(swipeCompletionTimer.current);
    swipeCompletionTimer.current = window.setTimeout(() => {
      swipeCompletionTimer.current = null;
      void executeSwipeAction(conversation, action, snoozePreset, direction)
        .then((dismissStarted) => {
          if (!dismissStarted) settleSwipeRow(session.id);
        })
        .catch(() => settleSwipeRow(session.id));
    }, messageSwipeCommitHoldMS);
  }

  // Pointer row actions reuse the swipe and selection mutation paths, so undo
  // toasts, optimistic dismissal, and busy gating behave identically. A row that
  // is already committing another action ignores them until it settles.
  function rowActionBlocked(conversation: Conversation): boolean {
    // Whole-filter mode included: while the selection is the filter itself, a
    // single-row action would contradict what the toolbar says is selected.
    return selectionBusy
      || scopeSelected
      || pendingSwipeActionIDs.current.has(conversation.message.id)
      || hiddenMessageIDs.has(conversation.message.id);
  }

  function replyToConversation(conversation: Conversation) {
    // The list the reply was started from travels with it, so finishing the
    // reply comes back here rather than to whichever list the app opens on.
    navigate(composeURL({ replyID: conversation.message.id, backURL: returnURL }));
  }

  function moveConversationByRowAction(conversation: Conversation, action: RowMoveAction) {
    if (rowActionBlocked(conversation)) return;
    moveConversation(conversation, action, null);
  }

  function toggleConversationRead(conversation: Conversation) {
    if (rowActionBlocked(conversation)) return;
    markConversationRead(conversation, !conversation.is_read);
  }

  function openRow(event: MouseEvent<HTMLDivElement>, href: string) {
    if (Date.now() < suppressRowClickUntil.current) return;
    if ((event.target as HTMLElement).closest("button,input,label")) return;
    navigate(href);
  }

  function openRowWithKeyboard(event: KeyboardEvent<HTMLDivElement>, href: string) {
    if (event.currentTarget !== event.target) return;
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      navigate(href);
    }
  }

  if (visible.length === 0) {
    return emptyState ?? <div className="panel muted">No messages here.</div>;
  }
  const arrivalActive = visible.some((conversation) => highlightMessageIDs?.has(conversation.message.id));
  const selectedConversations = visible.filter((conversation) => selectedIDs.has(conversation.message.id));
  const allOnPageSelected = selectedConversations.length === visible.length;
  const canMarkRead = selectedConversations.some((conversation) => !conversation.is_read);
  const canMarkUnread = selectedConversations.some((conversation) => conversation.is_read);
  // Whole-filter selection is only offered once the page itself is fully
  // selected and the filter is known to reach past it. A folder that fits on one
  // page has nothing more to offer, so the row selection already covers it.
  const scopeReachesPastPage = Boolean(listScope) && (listScope?.total === undefined || listScope.total > visible.length);
  const canSelectWholeScope = Boolean(listScope) && !snoozedView && allOnPageSelected && scopeReachesPastPage;
  const scopeButtonLabel = !listScope
    ? ""
    : listScope.query
      ? `Select all messages matching ${listScope.label}`
      : listScope.total === undefined
        ? `Select all messages in ${listScope.label}`
        : `Select all ${listScope.total.toLocaleString()} messages in ${listScope.label}`;
  const scopeSelectedLabel = !listScope
    ? ""
    : listScope.query
      ? `All messages matching ${listScope.label} are selected`
      : `All messages in ${listScope.label} are selected`;
  const pageOnlyHint = "Only Delete covers the whole filter. Clear the selection to act on single messages.";
  return (
    <div className={`message-table ${arrivalActive ? "mail-arrival-shift" : ""}`}>
      {selectedConversations.length > 0 || scopeSelected ? (
        <div className="selection-action-bar" role="toolbar" aria-label="Selected message actions" aria-busy={selectionBusy}>
          <div className="selection-action-summary">
            <button className="selection-clear" type="button" onClick={clearSelection} title="Clear selection" aria-label="Clear selection">
              <Icon name="close" />
            </button>
            {scopeSelected ? (
              <span className="selection-count selection-scope-count" aria-live="polite">
                <Icon name="select_all" />
                <strong>{listScope?.total !== undefined && !listScope.query ? listScope.total.toLocaleString() : "All"}</strong>
                <span>selected</span>
              </span>
            ) : (
              <span className="selection-count" aria-live="polite">
                <strong>{selectedConversations.length.toLocaleString()}</strong>
                <span>selected</span>
              </span>
            )}
            {scopeSelected ? (
              <span className="selection-page-status">{scopeSelectedLabel}</span>
            ) : allOnPageSelected ? (
              <>
                <span className="selection-page-status">All {visible.length.toLocaleString()} on this page</span>
                {canSelectWholeScope ? (
                  <button
                    className="selection-page-button selection-scope-button"
                    type="button"
                    onClick={selectWholeScope}
                    disabled={selectionBusy}
                    title={scopeButtonLabel}
                    aria-label={scopeButtonLabel}
                  >
                    <Icon name="select_all" />
                    <span>{listScope?.query || listScope?.total === undefined
                      ? "Select all matching"
                      : `Select all ${listScope.total.toLocaleString()}`}</span>
                  </button>
                ) : null}
              </>
            ) : (
              <button
                className="selection-page-button"
                type="button"
                onClick={selectAllOnPage}
                disabled={selectionBusy}
                title={`Select all ${visible.length.toLocaleString()} messages on this page`}
                aria-label={`Select all ${visible.length.toLocaleString()} messages on this page`}
              >
                <Icon name="select_all" />
                <span>Select all {visible.length.toLocaleString()}</span>
              </button>
            )}
          </div>
          <div className="selection-actions">
            <button
              type="button"
              disabled={selectionBusy || scopeSelected || !canMarkRead}
              onClick={() => void markSelectedRead(true)}
              title={scopeSelected ? pageOnlyHint : "Mark selected messages read"}
            >
              <Icon name="mail_open" />
              <span>Mark read</span>
            </button>
            <button
              type="button"
              disabled={selectionBusy || scopeSelected || !canMarkUnread}
              onClick={() => void markSelectedRead(false)}
              title={scopeSelected ? pageOnlyHint : "Mark selected messages unread"}
            >
              <Icon name="mail" />
              <span>Mark unread</span>
            </button>
      {snoozedView ? (
        <button type="button" disabled={selectionBusy} onClick={() => void unsnoozeConversations(selectedConversations)} title="Unsnooze selected messages">
          <Icon name="clock" />
          <span>Unsnooze</span>
        </button>
      ) : (
        <SnoozeControl datePrefs={datePrefs} disabled={selectionBusy || scopeSelected} onSnooze={(until) => snoozeConversations(selectedConversations, until)} />
      )}
            <button
              type="button"
              className="selection-delete"
              ref={scopeDeleteTrigger}
              disabled={selectionBusy}
              onClick={() => scopeSelected ? setScopeDeletePending(true) : void deleteSelected()}
              title={scopeSelected ? "Move every message matching this filter to Trash" : "Move selected messages to Trash"}
            >
              <Icon name="delete" />
              <span>{scopeDeleteBusy ? "Deleting" : "Delete"}</span>
            </button>
          </div>
        </div>
      ) : null}
      {scopeDeletePending && listScope ? (
        <div className="confirm-backdrop" role="presentation" onClick={closeScopeDeleteDialog}>
          <section
            className="confirm-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="scope-delete-title"
            onClick={(event) => event.stopPropagation()}
            onKeyDown={handleScopeDeleteKeys}
          >
            <h2 id="scope-delete-title">Delete everything this filter matches?</h2>
            <p>
              Every message {listScope.query ? `matching ${listScope.label}` : `in ${listScope.label}`} moves into its
              account's Trash folder{listScope.total !== undefined && !listScope.query ? ` — about ${listScope.total.toLocaleString()} messages` : ""}.
              This is not limited to the page you can see, it runs in the background, and it cannot be undone.
            </p>
            <div className="actions">
              <button className="secondary" type="button" ref={scopeDeleteCancel} onClick={closeScopeDeleteDialog}>Cancel</button>
              <button type="button" ref={scopeDeleteConfirm} onClick={() => void deleteWholeScope()}>Move all to Trash</button>
            </div>
          </section>
        </div>
      ) : null}
      {visible.map((conversation, index) => {
        const msg = conversation.message;
        const matchTerms = conversation.match_terms || [];
        const href = openAsDraft
          ? composeURL({ draftID: msg.id, backURL: returnURL })
          : messageURL(msg.id, searchQuery, matchTerms, returnURL, searchQuery ? msg.id : 0);
        const attachmentNames = conversation.attachment_names || [];
        const attachmentMatches = conversation.attachment_matches || [];
        const previewText = messageSecurityPreviewText(messageSecurityPlugins, conversation.snippet, msg);
        const securitySnippetClass = messageSecuritySnippetClassName(messageSecurityPlugins, msg);
        const securityIndicators = messageSecurityIndicators(messageSecurityPlugins, { location: "message-list", message: msg, state: msg });
        const annotationNodes = messageAnnotationNodes(messageSecurityPlugins, msg);
        // Whole-filter mode covers rows this page has not even loaded, so every
        // visible row reads as selected until the mode is left again.
        const selected = scopeSelected || selectedIDs.has(msg.id);
        const touchMessageIDs = selected && selectedDragMessageIDs.length > 0 ? selectedDragMessageIDs : conversationTransferMessageIDs(conversation);
        const touchAccountIDs = selected && selectedDragAccountIDs.length > 0 ? selectedDragAccountIDs : conversationTransferAccountIDs(conversation);
        const movingOut = hiddenMessageIDs.has(msg.id);
        const rowActionsDisabled = selectionBusy || scopeSelected || movingOut || pendingSwipeActionIDs.current.has(msg.id);
        // Report spam stays visible with its reason in the tooltip when it
        // cannot run, rather than appearing on some rows and not others.
        const rowJunkMailbox = junkMailboxForAccount(mailboxes, msg.account_id);
        const rowSpamState = !rowJunkMailbox
          ? { disabled: true, title: "This account has no Junk folder to report spam into" }
          : msg.mailbox_id === rowJunkMailbox.id
            ? { disabled: true, title: `Already in ${rowJunkMailbox.name}` }
            : { disabled: rowActionsDisabled, title: `Report spam (${rowJunkMailbox.name})` };
        const activeSwipe = swipeState?.id === msg.id ? swipeState : null;
        const swipeDelta = activeSwipe?.deltaX || 0;
        const swipeReady = Boolean(activeSwipe?.committed || (activeSwipe && Math.abs(activeSwipe.visualDeltaX) >= messageSwipeCommitDistance));
        const swipeStyle = activeSwipe ? messageSwipeAffordanceStyle(activeSwipe) : undefined;
        const participants = participantSource(conversation, showRecipients);
        const participantText = showRecipients
          ? `To: ${participants || "undisclosed recipients"}`
          : (participants || "Unknown sender");
        const sectionHeading = sectionHeadings[index] || "";
        return (
      <Fragment key={msg.id}>
      {sectionHeading ? (
        <div className="message-date-heading" role="heading" aria-level={2}>{sectionHeading}</div>
      ) : null}
      <div
        className={`message-swipe-shell ${activeSwipe ? `revealing-${activeSwipe.direction} swipe-phase-${activeSwipe.phase}` : ""} ${swipeReady ? "swipe-action-ready" : ""}`}
        style={swipeStyle}
      >
      <div className="message-swipe-actions" aria-hidden="true">
        <span className={`message-swipe-action message-swipe-action-start swipe-action-${rightSwipePresentation.className}`}>
          <span className="message-swipe-action-content">
            <span className="message-swipe-action-icon"><Icon name={rightSwipePresentation.icon} /></span>
            <small>{rightSwipePresentation.label}</small>
          </span>
        </span>
        <span className={`message-swipe-action message-swipe-action-end swipe-action-${leftSwipePresentation.className}`}>
          <span className="message-swipe-action-content">
            <span className="message-swipe-action-icon"><Icon name={leftSwipePresentation.icon} /></span>
            <small>{leftSwipePresentation.label}</small>
          </span>
        </span>
      </div>
      <div
            className={`message-row ${conversation.is_read ? "read" : "unread"} ${selected ? "selected" : ""} ${keyboardIndex === index ? "keyboard-focused" : ""} ${movingOut ? "moving-out" : ""} ${highlightMessageIDs?.has(msg.id) ? "new-delivery" : ""}`}
      style={activeSwipe ? { transform: `translateX(${swipeDelta}px)` } : undefined}
            draggable
            ref={(node) => {
              if (node) rowRefs.current.set(msg.id, node);
              else rowRefs.current.delete(msg.id);
            }}
            data-rolltop-message-drag="true"
            data-rolltop-list-index={index}
            data-rolltop-touch-drag={nativeTouchDrag ? "true" : undefined}
            data-rolltop-touch-message-ids={nativeTouchDrag ? touchMessageIDs.join(",") : undefined}
            data-rolltop-touch-account-ids={nativeTouchDrag ? touchAccountIDs.join(",") : undefined}
            role="link"
            tabIndex={0}
            onClick={(event) => openRow(event, href)}
            onFocus={() => {
              keyboardIndexRef.current = index;
              setKeyboardIndex(index);
            }}
            onKeyDown={(event) => openRowWithKeyboard(event, href)}
            onDragStart={(event) => startMessageDrag(event, conversation)}
      onTouchStart={(event) => startRowSwipe(event, conversation)}
      onTouchMove={moveRowSwipe}
      onTouchEnd={() => finishRowSwipe(conversation)}
      onTouchCancel={() => { swipeSession.current = null; setSwipeState(null); }}
          >
            <label
              className={`message-select ${selected && selectedIDs.size > 1 ? "group-drag-source" : ""}`}
              draggable={selected}
              onClick={(event) => event.stopPropagation()}
              title={selected && selectedIDs.size > 1 ? `Drag ${selectedIDs.size.toLocaleString()} selected messages or clear selection` : "Select message"}
            >
              <input
                type="checkbox"
                checked={selected}
                disabled={pendingSwipeActionIDs.current.has(msg.id)}
                aria-label={`Select ${msg.subject || "message"}`}
                onClick={(event) => selectMessage(event, index, msg.id)}
                onChange={() => undefined}
              />
            </label>
            <span className={`sender-avatar avatar-hue-${senderAvatarHue(participants)}`} aria-hidden="true">
              {displayInitial(participants)}
            </span>
            <span className="sender">
              <span className="sender-name">
                <HighlightedText text={participantText} query={searchQuery} terms={matchTerms} />
              </span>
              {conversation.count > 1 ? <span className="thread-count">({conversation.count})</span> : null}
            </span>
            <span className="subject">
              <span className="subject-line">
                <strong>
                  <HighlightedText text={msg.subject || "(no subject)"} query={searchQuery} terms={matchTerms} />
                </strong>
                {securityIndicators}
                {annotationNodes}
                {attachmentNames.length > 0 ? (
                  <span className={`attachment-preview ${attachmentMatches.length > 0 || conversation.attachment_content_matched ? "matched" : ""}`}>
                    <Icon name="attach_file" />
                    <HighlightedText
                      text={attachmentMatches.length > 0 ? attachmentMatches.join(", ") : attachmentNames.join(", ")}
                      query={searchQuery}
                      terms={matchTerms}
                    />
                  </span>
                ) : conversation.has_attachments ? <Icon name="attach_file" /> : null}
              </span>
              <span className={`snippet ${securitySnippetClass}`}>
                <HighlightedText text={previewText} query={securitySnippetClass ? "" : searchQuery} terms={securitySnippetClass ? [] : matchTerms} />
              </span>
            </span>
      <span className={`date ${snoozedView ? "snoozed-date" : ""}`}>
        {snoozedView ? (
          <button className="snooze-row-action" type="button" disabled={selectionBusy} onClick={() => void unsnoozeConversations([conversation])} title="Unsnooze" aria-label="Unsnooze">
            <Icon name="clock" />
          </button>
        ) : null}
        <span>{displayTime(rowDate(conversation, snoozedView), datePrefs)}</span>
      </span>
      <div className="message-row-actions" role="group" aria-label={`Actions for ${msg.subject || "message"}`}>
        {openAsDraft ? null : (
          <button className="message-row-action" type="button" disabled={rowActionsDisabled} onClick={() => replyToConversation(conversation)} title="Reply" aria-label="Reply">
            <Icon name="reply" />
          </button>
        )}
        {openAsDraft || snoozedView ? null : (
          <button className="message-row-action" type="button" disabled={rowActionsDisabled} onClick={() => moveConversationByRowAction(conversation, "archive")} title="Archive" aria-label="Archive">
            <Icon name="archive" />
          </button>
        )}
        <button className="message-row-action row-action-delete" type="button" disabled={rowActionsDisabled} onClick={() => moveConversationByRowAction(conversation, "trash")} title="Move to trash" aria-label="Move to trash">
          <Icon name="delete" />
        </button>
        {openAsDraft ? null : (
          <button
            className="message-row-action"
            type="button"
            disabled={rowSpamState.disabled}
            onClick={() => moveConversationByRowAction(conversation, "spam")}
            title={rowSpamState.title}
            aria-label="Report spam"
          >
            <Icon name="spam" />
          </button>
        )}
        {openAsDraft ? null : (
          <button
            className="message-row-action"
            type="button"
            disabled={rowActionsDisabled}
            onClick={() => toggleConversationRead(conversation)}
            title={conversation.is_read ? "Mark unread" : "Mark read"}
            aria-label={conversation.is_read ? "Mark unread" : "Mark read"}
          >
            <Icon name={conversation.is_read ? "mail" : "mail_open"} />
          </button>
        )}
        {openAsDraft ? null : snoozedView ? (
          // The toolbar covers the date cell's unsnooze button, so it carries
          // the same action while a pointer is over the row.
          <button className="message-row-action" type="button" disabled={rowActionsDisabled} onClick={() => void unsnoozeConversations([conversation])} title="Unsnooze" aria-label="Unsnooze">
            <Icon name="clock" />
          </button>
        ) : (
          <SnoozeControl
            className="message-row-action"
            iconOnly
            datePrefs={datePrefs}
            disabled={rowActionsDisabled}
            onSnooze={(until) => snoozeConversations([conversation], until)}
          />
        )}
      </div>
      </div>
          </div>
      </Fragment>
        );
      })}
    </div>
  );
}
