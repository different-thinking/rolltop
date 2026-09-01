// File overview: Authenticated application chrome: top bar, search entry, folder sidebar and the
// control that hides it, mobile drawer, drag-to-folder handling, sync status, and the floating
// compose affordance every layout without a sidebar falls back on.

import { Fragment, useMemo, useState, useEffect, useLayoutEffect, useRef } from "react";
import type { DragEvent, FormEvent, MouseEvent, ReactNode } from "react";
import { api } from "../../api";
import type { AppShellProps, LocationState, MailCategoryTarget, MessageTransferAction, MoveTarget, SecurityUnlockState } from "../../appTypes";
import type { Bootstrap, Mailbox, MailCategorySummary, SyncRun, User } from "../../types";
import { Icon, LogoMark } from "../../components/Icon";
import { androidNativeAvailable, shouldAdvertiseAndroidApp } from "../../lib/androidNative";
import { folderTree, folderTreeUnreadCount, nodeContainsMailbox, type FolderNode } from "../../lib/folders";
import { messageCountLabel } from "../../lib/format";
import { shouldIgnoreMailShortcut } from "../../lib/keyboard";
import { mailRoute, mailRouteView, mailURL, organizerRoute, organizerURL, searchRoute, searchURL, currentLocation, type PluginAppRoute } from "../../lib/routes";
import type { OrganizerRoute } from "../../lib/routes";
import { maxSidebarShortcuts, useSidebarShortcuts } from "../../lib/sidebarShortcuts";
import { loadCollapsedAccounts, loadSidebarHidden, saveCollapsedAccounts, saveSidebarHidden } from "../../lib/sidebarLocal";
import { createPluginSet } from "../../plugins/registry";
import { SearchAutocomplete, useSearchAutocomplete } from "./SearchAutocomplete";
import { useExpectedDeliveries, type ExpectedDeliveries } from "../deliveries/useExpectedDeliveries";
import { useDueInvoices, type DueInvoices } from "../invoices/useDueInvoices";

/**
 * AppShell renders everything that survives route changes after login: topbar,
 * folder navigation, sync widget, account warnings, mobile drawer state, and the
 * floating compose action. Children supply only the current content view.
 */
export function AppShell({
  user,
  csrf,
  mailboxes,
  latestSyncRun,
  activeSyncRuns,
  unfinishedMoveRun,
  syncRunning,
  serverStartedAt,
  serverUptimeSeconds,
  buildVersion,
  buildDate,
  buildLabel,
  buildCommit,
  accountNeedsPassword,
  accountNotice,
  databaseUnavailable,
  enabledPlugins,
  pluginAppLinks,
  mailCategories,
  mailCategoriesPending,
  mailGeneration,
  location,
  navigate,
  onMoveMessages,
  onFileMessagesInCategory,
  openCompose,
  refreshChrome,
  notificationsEnabled,
  toggleNotifications,
  securityUnlockAvailable,
  securityUnlock,
  openSecurityUnlock,
  lockSecurity,
  logout,
  children
}: AppShellProps) {
  const [mobileSidebarOpen, setMobileSidebarOpen] = useState(false);
  const [sidebarHidden, setSidebarHidden] = useState(() => loadSidebarHidden(user.id));
  const [messageDragActive, setMessageDragActive] = useState(false);
  const [touchDragPreview, setTouchDragPreview] = useState<TouchDragPreview | null>(null);
  const [touchDrop, setTouchDrop] = useState<TouchDropTarget | null>(null);
  const [touchRevealedAccounts, setTouchRevealedAccounts] = useState<string[]>([]);
  const appRef = useRef<HTMLDivElement>(null);
  const dragOpenedSidebar = useRef(false);
  const nativeDragInProgress = useRef(false);
  const nativeDragActivationTimer = useRef<number | null>(null);
  const mobileSidebarOpenRef = useRef(mobileSidebarOpen);
  const onMoveMessagesRef = useRef(onMoveMessages);
  const onFileMessagesInCategoryRef = useRef(onFileMessagesInCategory);

  // The Android touch listeners are installed once and read these refs when a
  // drop lands, so they have to hold committed values. Writing them during
  // render would let a discarded render hand a drop a callback or a sidebar
  // state that never reached the screen.
  useLayoutEffect(() => {
    mobileSidebarOpenRef.current = mobileSidebarOpen;
    onMoveMessagesRef.current = onMoveMessages;
    onFileMessagesInCategoryRef.current = onFileMessagesInCategory;
  });

  useEffect(() => {
    return () => {
      if (nativeDragActivationTimer.current !== null) window.clearTimeout(nativeDragActivationTimer.current);
    };
  }, []);

  // WebView's native touch drag is canceled by drawer hit-test changes, so the
  // Android shell uses a long-press gesture while mouse/desktop drag stays native.
  useEffect(() => {
    const root = appRef.current;
    if (!root || !androidNativeAvailable()) return;
    const appRoot = root;

    let session: AndroidTouchDragSession | null = null;
    let sessionEventTarget: HTMLElement | null = null;
    let sessionListenersAttached = false;
    let suspendedNativeDragElements: HTMLElement[] = [];
    let suppressCompatibilityClick = false;
    let compatibilityClickX = 0;
    let compatibilityClickY = 0;
    let compatibilityClickTimer: number | null = null;
    let touchClassRemovalTimer: number | null = null;

    function clearCompatibilityClickTimer() {
      if (compatibilityClickTimer === null) return;
      window.clearTimeout(compatibilityClickTimer);
      compatibilityClickTimer = null;
    }

    function expireCompatibilityClickGuard() {
      clearCompatibilityClickTimer();
      compatibilityClickTimer = window.setTimeout(() => {
        compatibilityClickTimer = null;
        suppressCompatibilityClick = false;
      }, 400);
    }

    function clearTouchClassRemovalTimer() {
      if (touchClassRemovalTimer === null) return;
      window.clearTimeout(touchClassRemovalTimer);
      touchClassRemovalTimer = null;
    }

    function removeTouchDragClassAfterEvent() {
      clearTouchClassRemovalTimer();
      touchClassRemovalTimer = window.setTimeout(() => {
        touchClassRemovalTimer = null;
        document.documentElement.classList.remove("rolltop-touch-message-dragging");
      }, 0);
    }

    function clearHoldTimer() {
      if (!session || session.holdTimer === null) return;
      window.clearTimeout(session.holdTimer);
      session.holdTimer = null;
    }

    function suspendNativeDrag(source: HTMLElement) {
      restoreNativeDrag();
      const candidates = [source, ...Array.from(source.querySelectorAll<HTMLElement>("[draggable='true']"))];
      suspendedNativeDragElements = candidates.filter((element, index) => element.draggable && candidates.indexOf(element) === index);
      suspendedNativeDragElements.forEach((element) => { element.draggable = false; });
    }

    function restoreNativeDrag() {
      suspendedNativeDragElements.forEach((element) => { element.draggable = true; });
      suspendedNativeDragElements = [];
    }

    function closeAutoOpenedSidebar() {
      if (!dragOpenedSidebar.current) return;
      dragOpenedSidebar.current = false;
      mobileSidebarOpenRef.current = false;
      setMobileSidebarOpen(false);
    }

    function resetTouchDrag() {
      const wasActive = session?.active === true;
      if (session) {
        compatibilityClickX = session.lastX;
        compatibilityClickY = session.lastY;
      }
      clearHoldTimer();
      session = null;
      detachSessionListeners();
      restoreNativeDrag();
      if (!wasActive) return;
      removeTouchDragClassAfterEvent();
      setMessageDragActive(false);
      setTouchDragPreview(null);
      setTouchDrop(null);
      setTouchRevealedAccounts((current) => current.length > 0 ? [] : current);
      closeAutoOpenedSidebar();
      expireCompatibilityClickGuard();
    }

    function activateTouchDrag() {
      if (!session || session.active) return;
      session.holdTimer = null;
      session.active = true;
      session.startX = session.lastX;
      session.startY = session.lastY;
      suppressCompatibilityClick = true;
      compatibilityClickX = session.lastX;
      compatibilityClickY = session.lastY;
      clearCompatibilityClickTimer();
      clearTouchClassRemovalTimer();
      document.documentElement.classList.add("rolltop-touch-message-dragging");
      setMessageDragActive(true);
      setTouchDragPreview(touchPreviewAt(session.lastX, session.lastY, session.messageIDs.length));
      if (!mobileSidebarOpenRef.current) {
        dragOpenedSidebar.current = true;
        mobileSidebarOpenRef.current = true;
        setMobileSidebarOpen(true);
      }
    }

    function start(event: TouchEvent) {
      if (!session) {
        suppressCompatibilityClick = false;
        clearCompatibilityClickTimer();
      }
      if (event.touches.length !== 1) {
        if (session) resetTouchDrag();
        return;
      }
      if (session) return;
      const target = event.target;
      const source = target instanceof Element ? target.closest<HTMLElement>("[data-rolltop-touch-drag='true']") : null;
      if (!source || !appRoot.contains(source)) return;
      const messageIDs = positiveIDs(source.dataset.rolltopTouchMessageIds);
      if (messageIDs.length === 0) return;
      const touch = event.touches[0];
      suspendNativeDrag(source);
      session = {
        identifier: touch.identifier,
        startX: touch.clientX,
        startY: touch.clientY,
        lastX: touch.clientX,
        lastY: touch.clientY,
        messageIDs,
        accountIDs: positiveIDs(source.dataset.rolltopTouchAccountIds),
        active: false,
        movedAfterActivation: false,
        holdTimer: window.setTimeout(activateTouchDrag, androidTouchDragHoldMS)
      };
      attachSessionListeners(source);
    }

    function move(event: TouchEvent) {
      if (!session) return;
      if (event.touches.length !== 1) {
        resetTouchDrag();
        return;
      }
      const touch = touchWithIdentifier(event.touches, session.identifier);
      if (!touch) return;
      session.lastX = touch.clientX;
      session.lastY = touch.clientY;
      if (!session.active) {
        if (Math.hypot(touch.clientX - session.startX, touch.clientY - session.startY) > androidTouchDragSlop) {
          resetTouchDrag();
        }
        return;
      }
      if (event.cancelable) event.preventDefault();
      setTouchDragPreview(touchPreviewAt(touch.clientX, touch.clientY, session.messageIDs.length));
      if (Math.hypot(touch.clientX - session.startX, touch.clientY - session.startY) > androidTouchDragSlop) {
        session.movedAfterActivation = true;
      }
      setTouchDrop(session.movedAfterActivation ? touchDropTargetAt(touch.clientX, touch.clientY) : null);
      // Touch drags cannot fire dragenter, so dwelling on a collapsed account
      // header is what opens it up as a drop target.
      const accountKey = session.movedAfterActivation ? touchAccountKeyAt(touch.clientX, touch.clientY) : null;
      if (accountKey) {
        setTouchRevealedAccounts((current) => current.includes(accountKey) ? current : [...current, accountKey]);
      }
    }

    function finish(event: TouchEvent) {
      if (!session) return;
      const touch = touchWithIdentifier(event.changedTouches, session.identifier);
      if (!touch) return;
      if (!session.active) {
        resetTouchDrag();
        return;
      }
      if (event.cancelable) event.preventDefault();
      const dropTarget = session.movedAfterActivation ? touchDropTargetAt(touch.clientX, touch.clientY) : null;
      const messageIDs = session.messageIDs;
      const accountIDs = session.accountIDs;
      resetTouchDrag();
      if (!dropTarget) return;
      if (dropTarget.kind === "category") {
        onFileMessagesInCategoryRef.current(messageIDs, { name: dropTarget.name, label: dropTarget.label });
        return;
      }
      const crossAccount = accountIDs.some((accountID) => dropTarget.accountID > 0 && accountID !== dropTarget.accountID);
      onMoveMessagesRef.current(messageIDs, { id: dropTarget.id, name: dropTarget.name }, crossAccount ? "copy" : "move");
    }

    function cancel() {
      resetTouchDrag();
    }

    function suppressContextMenu(event: Event) {
      if (session || suppressCompatibilityClick) event.preventDefault();
    }

    function suppressGeneratedClick(event: Event) {
      if (!suppressCompatibilityClick || !(event instanceof globalThis.MouseEvent)) return;
      if (Math.hypot(event.clientX - compatibilityClickX, event.clientY - compatibilityClickY) > 40) return;
      event.preventDefault();
      event.stopImmediatePropagation();
      suppressCompatibilityClick = false;
      clearCompatibilityClickTimer();
    }

    function suppressNativeTouchDrag(event: Event) {
      if (!session) return;
      event.preventDefault();
      event.stopImmediatePropagation();
    }

    function handleVisibilityChange() {
      if (document.visibilityState !== "visible") resetTouchDrag();
    }

    function attachSessionListeners(source: HTMLElement) {
      if (sessionListenersAttached) return;
      sessionListenersAttached = true;
      sessionEventTarget = source;
      source.addEventListener("touchmove", move, { passive: false });
      source.addEventListener("touchend", finish, { passive: false });
      source.addEventListener("touchcancel", cancel, { passive: true });
    }

    function detachSessionListeners() {
      if (!sessionListenersAttached || !sessionEventTarget) return;
      sessionListenersAttached = false;
      sessionEventTarget.removeEventListener("touchmove", move);
      sessionEventTarget.removeEventListener("touchend", finish);
      sessionEventTarget.removeEventListener("touchcancel", cancel);
      sessionEventTarget = null;
    }

    document.addEventListener("touchstart", start, { passive: true, capture: true });
    document.addEventListener("contextmenu", suppressContextMenu, true);
    document.addEventListener("click", suppressGeneratedClick, true);
    document.addEventListener("dragstart", suppressNativeTouchDrag, true);
    document.addEventListener("visibilitychange", handleVisibilityChange);
    window.addEventListener("blur", cancel);
    return () => {
      clearHoldTimer();
      clearCompatibilityClickTimer();
      clearTouchClassRemovalTimer();
      detachSessionListeners();
      restoreNativeDrag();
      document.documentElement.classList.remove("rolltop-touch-message-dragging");
      document.removeEventListener("touchstart", start, true);
      document.removeEventListener("contextmenu", suppressContextMenu, true);
      document.removeEventListener("click", suppressGeneratedClick, true);
      document.removeEventListener("dragstart", suppressNativeTouchDrag, true);
      document.removeEventListener("visibilitychange", handleVisibilityChange);
      window.removeEventListener("blur", cancel);
    };
  }, []);

  function clearNativeDragActivationTimer() {
    if (nativeDragActivationTimer.current === null) return;
    window.clearTimeout(nativeDragActivationTimer.current);
    nativeDragActivationTimer.current = null;
  }

  // The shell outlives a login change, so the stored preference is re-read when
  // the user does. Without this the next reader inherits whatever the previous
  // one left on screen, under their own storage key.
  useEffect(() => {
    setSidebarHidden(loadSidebarHidden(user.id));
  }, [user.id]);

  function toggleSidebar() {
    const next = !sidebarHidden;
    setSidebarHidden(next);
    saveSidebarHidden(user.id, next);
  }

  function openMobileSidebar() {
    clearNativeDragActivationTimer();
    dragOpenedSidebar.current = false;
    mobileSidebarOpenRef.current = true;
    setMobileSidebarOpen(true);
  }

  function closeMobileSidebar() {
    clearNativeDragActivationTimer();
    dragOpenedSidebar.current = false;
    mobileSidebarOpenRef.current = false;
    setMobileSidebarOpen(false);
  }

  function beginMessageDrag(event: DragEvent<HTMLDivElement>) {
    if (!isRolltopMessageDrag(event)) return;
    clearNativeDragActivationTimer();
    nativeDragInProgress.current = true;
    nativeDragActivationTimer.current = window.setTimeout(() => {
      nativeDragActivationTimer.current = null;
      if (!nativeDragInProgress.current) return;
      setMessageDragActive(true);
      if (!window.matchMedia("(max-width: 760px)").matches || mobileSidebarOpenRef.current) return;
      dragOpenedSidebar.current = true;
      mobileSidebarOpenRef.current = true;
      setMobileSidebarOpen(true);
    }, 0);
  }

  function endMessageDrag(event: DragEvent<HTMLDivElement>) {
    if (!isRolltopMessageDrag(event)) return;
    if (!nativeDragInProgress.current) return;
    nativeDragInProgress.current = false;
    clearNativeDragActivationTimer();
    setMessageDragActive(false);
    if (!dragOpenedSidebar.current) return;
    dragOpenedSidebar.current = false;
    mobileSidebarOpenRef.current = false;
    setMobileSidebarOpen(false);
  }

  // The floating Compose button stands in for the sidebar's own wherever the
  // sidebar is not on the page - the phone drawer, and a desktop that hid it.
  function composeFromFab() {
    closeMobileSidebar();
    openCompose("");
  }

  return (
    <>
      <Topbar
        user={user}
        mailGeneration={mailGeneration}
        mailboxes={mailboxes}
        enabledPlugins={enabledPlugins}
        location={location}
        activeSyncRuns={activeSyncRuns}
        navigate={navigate}
        notificationsEnabled={notificationsEnabled}
        toggleNotifications={toggleNotifications}
        securityUnlockAvailable={securityUnlockAvailable}
        securityUnlock={securityUnlock}
        openSecurityUnlock={openSecurityUnlock}
        lockSecurity={lockSecurity}
        logout={logout}
        onMenu={openMobileSidebar}
        sidebarHidden={sidebarHidden}
        onToggleSidebar={toggleSidebar}
      />
      <div
        ref={appRef}
        className={`app ${sidebarHidden ? "sidebar-hidden" : ""} ${messageDragActive ? "message-drag-active" : ""}`}
        onDragStart={beginMessageDrag}
        onDragEnd={endMessageDrag}
      >
        {mobileSidebarOpen && !messageDragActive ? (
          <button className="mobile-sidebar-scrim" type="button" aria-label="Close folders" onClick={closeMobileSidebar} />
        ) : null}
        <Sidebar
          key={user.id}
          userID={user.id}
          mailboxes={mailboxes}
          csrf={csrf}
          latestSyncRun={latestSyncRun}
          activeSyncRuns={activeSyncRuns}
          unfinishedMoveRun={unfinishedMoveRun}
          syncRunning={syncRunning}
          serverStartedAt={serverStartedAt}
          serverUptimeSeconds={serverUptimeSeconds}
          buildVersion={buildVersion}
          buildDate={buildDate}
          buildLabel={buildLabel}
          buildCommit={buildCommit}
          mailCategories={mailCategories}
          mailCategoriesPending={mailCategoriesPending}
          pluginAppLinks={pluginAppLinks}
          currentPath={location.path}
          navigate={navigate}
          openCompose={openCompose}
          refreshChrome={refreshChrome}
          onMoveMessages={onMoveMessages}
          onFileMessagesInCategory={onFileMessagesInCategory}
          mobileOpen={mobileSidebarOpen}
          dragActive={messageDragActive}
          touchDrop={touchDrop}
          touchRevealedAccounts={touchRevealedAccounts}
          onClose={closeMobileSidebar}
        />
        <main className={`content ${mailRouteView(location.path, Boolean(user.is_admin), pluginAppLinks) ? "measured" : ""}`}>
          {databaseUnavailable ? <DatabaseUnavailableBanner isAdmin={Boolean(user.is_admin)} navigate={navigate} /> : null}
          {accountNeedsPassword ? <AccountCredentialBanner notice={accountNotice} navigate={navigate} /> : null}
          {children}
        </main>
        {touchDragPreview ? (
          <div
            className="message-touch-drag-preview"
            style={{ left: touchDragPreview.left, top: touchDragPreview.top }}
            aria-hidden="true"
          >
            <Icon name="mail" weight="bold" />
            <span>{messageCountLabel(touchDragPreview.count)}</span>
          </div>
        ) : null}
      </div>
      <button className="compose-fab" type="button" onClick={composeFromFab} aria-label="Compose">
        <Icon name="edit" weight="bold" />
        <span>Compose</span>
      </button>
    </>
  );
}

function isRolltopMessageDrag(event: DragEvent<HTMLElement>) {
  const target = event.target;
  if (target instanceof Element && target.closest("[data-rolltop-message-drag]")) return true;
  const types = Array.from(event.dataTransfer.types);
  return types.includes("application/x-rolltop-message-transfer") ||
    types.includes("application/x-rolltop-messages") ||
    types.includes("application/x-rolltop-message");
}

const androidTouchDragHoldMS = 180;
const androidTouchDragSlop = 12;

type TouchDragPreview = {
  left: number;
  top: number;
  count: number;
};

type AndroidTouchDragSession = {
  identifier: number;
  startX: number;
  startY: number;
  lastX: number;
  lastY: number;
  messageIDs: number[];
  accountIDs: number[];
  active: boolean;
  movedAfterActivation: boolean;
  holdTimer: number | null;
};

/**
 * TouchDropTarget is what a finger is currently over. A folder takes the mail
 * itself; a category takes a statement about its senders, so the two carry
 * different identities rather than a shared numeric ID.
 */
type TouchDropTarget =
  | { kind: "mailbox"; id: number; name: string; accountID: number }
  | { kind: "category"; name: string; label: string };

/**
 * draggedMessages reads what a message drag carries. The three payload formats
 * are tried in order of how much they say: the full transfer object, the bare ID
 * list, then the single-ID fallback that a drop from an older tab still uses.
 */
function draggedMessages(event: DragEvent): { ids: number[]; accountIDs: number[] } {
  const transfer = event.dataTransfer.getData("application/x-rolltop-message-transfer");
  if (transfer) {
    try {
      const parsed = JSON.parse(transfer) as { ids?: unknown; account_ids?: unknown };
      const ids = Array.isArray(parsed.ids) ? numericIDs(parsed.ids) : [];
      if (ids.length > 0) {
        return { ids, accountIDs: Array.isArray(parsed.account_ids) ? numericIDs(parsed.account_ids) : [] };
      }
    } catch {
      // A malformed payload falls through to the simpler formats below.
    }
  }
  const bulk = event.dataTransfer.getData("application/x-rolltop-messages");
  if (bulk) {
    try {
      const parsed = JSON.parse(bulk) as unknown;
      const ids = Array.isArray(parsed) ? numericIDs(parsed) : [];
      if (ids.length > 0) return { ids, accountIDs: [] };
    } catch {
      // Same here: an unreadable list is no list, not a failed drop.
    }
  }
  // Only Rolltop's own formats are read back. The drag also carries text/plain
  // so mail can be dropped into other apps, but accepting it here would let any
  // dragged text that happens to parse as a number file or move that message.
  const messageID = Number.parseInt(event.dataTransfer.getData("application/x-rolltop-message"), 10);
  return { ids: Number.isFinite(messageID) && messageID > 0 ? [messageID] : [], accountIDs: [] };
}

function numericIDs(values: unknown[]): number[] {
  return values.map((value) => Number(value)).filter((value) => Number.isFinite(value) && value > 0);
}

function positiveIDs(raw: string | undefined): number[] {
  if (!raw) return [];
  return Array.from(new Set(raw.split(",")
    .map((value) => Number.parseInt(value, 10))
    .filter((value) => Number.isFinite(value) && value > 0)));
}

function touchWithIdentifier(touches: TouchList, identifier: number): Touch | null {
  for (let index = 0; index < touches.length; index += 1) {
    if (touches[index].identifier === identifier) return touches[index];
  }
  return null;
}

function touchDropTargetAt(x: number, y: number): TouchDropTarget | null {
  const element = document.elementFromPoint(x, y);
  const category = element?.closest<HTMLElement>("[data-rolltop-drop-category]");
  if (category) {
    const name = category.dataset.rolltopDropCategory || "";
    if (!name) return null;
    return { kind: "category", name, label: category.dataset.rolltopDropCategoryLabel || name };
  }
  const target = element?.closest<HTMLElement>("[data-rolltop-drop-mailbox-id]");
  if (!target) return null;
  const id = Number.parseInt(target.dataset.rolltopDropMailboxId || "", 10);
  if (!Number.isFinite(id) || id <= 0) return null;
  const accountID = Number.parseInt(target.dataset.rolltopDropAccountId || "", 10);
  return {
    kind: "mailbox",
    id,
    name: target.dataset.rolltopDropMailboxName || "Folder",
    accountID: Number.isFinite(accountID) && accountID > 0 ? accountID : 0
  };
}

function touchAccountKeyAt(x: number, y: number): string | null {
  const target = document.elementFromPoint(x, y)?.closest<HTMLElement>("[data-rolltop-drop-account-key]");
  return target?.dataset.rolltopDropAccountKey || null;
}

function touchPreviewAt(x: number, y: number, count: number): TouchDragPreview {
  const width = 156;
  return {
    left: Math.max(8, Math.min(window.innerWidth - width - 8, x + 16)),
    top: Math.max(72, y - 58),
    count
  };
}

// This banner is intentionally high in the shell so a broken master key or
// undecryptable IMAP password is visible on every authenticated page.
function DatabaseUnavailableBanner({ isAdmin, navigate }: { isAdmin: boolean; navigate: (url: string) => void }) {
  return (
    <section className="account-alert" role="alert">
      <Icon name="report" weight="duotone" />
      <div>
        <strong>Mail database unavailable</strong>
        <span>
          This account's database is damaged, so no mail can be shown. Nothing has been deleted: messages are
          restored from the mail server once the database is repaired.
        </span>
      </div>
      {isAdmin ? <button type="button" onClick={() => navigate("/admin/database")}>Repair database</button> : null}
    </section>
  );
}

function AccountCredentialBanner({ notice, navigate }: { notice: string; navigate: (url: string) => void }) {
  return (
    <section className="account-alert" role="alert">
      <Icon name="report" weight="duotone" />
      <div>
        <strong>IMAP password required</strong>
        <span>{notice || "The saved IMAP password cannot be decrypted. Re-enter it to restore sync and full-message loading."}</span>
      </div>
      <button type="button" onClick={() => navigate("/settings/account")}>Re-enter password</button>
    </section>
  );
}

function useServerUptimeLabel(startedAt: string, fallbackSeconds: number) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 60_000);
    return () => window.clearInterval(timer);
  }, [startedAt]);

  const started = Date.parse(startedAt || "");
  const seconds = Number.isFinite(started)
    ? Math.max(0, Math.floor((now - started) / 1000))
    : Math.max(0, Math.floor(fallbackSeconds || 0));
  return formatUptime(seconds);
}

function formatUptime(totalSeconds: number) {
  if (!Number.isFinite(totalSeconds) || totalSeconds <= 0) return "";
  const days = Math.floor(totalSeconds / 86_400);
  const hours = Math.floor((totalSeconds % 86_400) / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${Math.max(1, minutes)}m`;
}

function buildDisplayLabel(version: string, buildDate: string, fallbackLabel: string) {
  const trimmedVersion = version.trim();
  if (trimmedVersion && trimmedVersion.toLowerCase() !== "latest") return trimmedVersion;
  const parsed = Date.parse(buildDate || "");
  if (Number.isFinite(parsed)) {
    return `built ${new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", year: "numeric" }).format(parsed)}`;
  }
  return fallbackLabel.trim();
}

// Topbar owns the search input because search is global navigation, not part of
// a specific mailbox or message view.
function Topbar({
  user,
  mailGeneration,
  mailboxes,
  enabledPlugins,
  location,
  activeSyncRuns,
  navigate,
  notificationsEnabled,
  toggleNotifications,
  securityUnlockAvailable,
  securityUnlock,
  openSecurityUnlock,
  lockSecurity,
  logout,
  onMenu,
  sidebarHidden,
  onToggleSidebar
}: {
  user: User;
  mailGeneration: number;
  mailboxes: Mailbox[];
  enabledPlugins: string[];
  location: LocationState;
  /** Only for the count on the Syncs & tasks row; the page itself fetches its own state. */
  activeSyncRuns: SyncRun[];
  navigate: (url: string) => void;
  notificationsEnabled: boolean;
  toggleNotifications: () => Promise<void>;
  securityUnlockAvailable: boolean;
  securityUnlock: SecurityUnlockState;
  openSecurityUnlock: (identityID?: number, onUnlocked?: (state: SecurityUnlockState) => void, recipientKeyIDs?: string[], fallbackEmail?: string) => void;
  lockSecurity: () => void;
  logout: () => Promise<void>;
  onMenu: () => void;
  sidebarHidden: boolean;
  onToggleSidebar: () => void;
}) {
  const [query, setQuery] = useState(() => searchRoute(currentLocation().path).query);
  const [focused, setFocused] = useState(false);
  const searchInputRef = useRef<HTMLInputElement>(null);
  const accountMenuRef = useRef<HTMLDetailsElement>(null);
  const pluginKey = enabledPlugins.join("|");
  const pluginSet = useMemo(() => createPluginSet(enabledPlugins), [pluginKey]);
  const securityUnlocked = securityUnlock.keys.length > 0 && securityUnlock.unlockedUntil > Date.now();
  // Nothing is rendered on a day with no parcel due, which is most days. A
  // permanently visible icon that is usually empty is chrome; a chip that is
  // only there when something is coming is the notice it is meant to be.
  const expectedDeliveries = useExpectedDeliveries(mailGeneration);
  const dueInvoices = useDueInvoices(mailGeneration);
  const autocomplete = useSearchAutocomplete({
    query,
    focused,
    inputRef: searchInputRef,
    mailboxes,
    pluginSet,
    setQuery
  });

  useEffect(() => {
    setQuery(searchRoute(location.path).query);
  }, [location.path]);

  useEffect(() => {
    function focusSearch(event: KeyboardEvent) {
      if (event.key !== "/" || shouldIgnoreMailShortcut(event)) return;
      event.preventDefault();
      searchInputRef.current?.focus();
      searchInputRef.current?.select();
    }
    window.addEventListener("keydown", focusSearch);
    return () => window.removeEventListener("keydown", focusSearch);
  }, []);

  function submit(event: FormEvent) {
    event.preventDefault();
    const trimmed = query.trim();
    if (trimmed === "") {
      navigate("/mail");
      return;
    }
    navigate(searchURL(trimmed));
  }

  function closeAccountMenu() {
    if (accountMenuRef.current) accountMenuRef.current.open = false;
  }

  function menuNavigate(url: string) {
    closeAccountMenu();
    navigate(url);
  }

  async function menuToggleNotifications() {
    await toggleNotifications();
    closeAccountMenu();
  }

  async function menuLogout() {
    closeAccountMenu();
    await logout();
  }

  const accountLabel = user.name || user.email;

  return (
    <header className="topbar">
      <button className="ghost mobile-menu-button" type="button" title="Folders" aria-label="Folders" onClick={onMenu}>
        <Icon name="menu" />
      </button>
      <button
        className="ghost sidebar-toggle"
        type="button"
        title={sidebarHidden ? "Show folders" : "Hide folders"}
        aria-label={sidebarHidden ? "Show folders" : "Hide folders"}
        onClick={onToggleSidebar}
      >
        <Icon name="sidebar" />
      </button>
      <a
        href="/mail"
        className="brand"
        onClick={(event) => {
          event.preventDefault();
          navigate("/mail");
        }}
      >
        <LogoMark />
        <span className="brand-wordmark">rolltop</span>
      </a>
      <form className="top-search" onSubmit={submit}>
        <Icon name="search" />
        <input
          ref={searchInputRef}
          type="search"
          placeholder="Search mail"
          value={query}
          onFocus={() => setFocused(true)}
          onBlur={() => window.setTimeout(() => setFocused(false), 120)}
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={autocomplete.onKeyDown}
          autoComplete="off"
        />
        {focused ? <SearchAutocomplete items={autocomplete.items} activeIndex={autocomplete.activeIndex} onChoose={autocomplete.choose} /> : null}
      </form>
      {/* The chip belongs in the actions column and not beside the search:
          the topbar is a five-column grid on a desktop, the spare column
          between the two is minmax(0, 1fr) and squeezes anything put in it to
          nothing, and this column is the max-content one. */}
      <nav className="top-actions" aria-label="Account">
        {expectedDeliveries.count > 0 ? (
          <button
            className="delivery-chip"
            type="button"
            title={deliveryChipTitle(expectedDeliveries)}
            onClick={() => navigate("/deliveries")}
          >
            <Icon name="package" weight="fill" />
            <span className="delivery-chip-label">{deliveryChipLabel(expectedDeliveries)}</span>
          </button>
        ) : null}
        {dueInvoices.count > 0 ? (
          <button
            className={dueInvoices.chased > 0 ? "invoice-chip-button chased" : "invoice-chip-button"}
            type="button"
            title={invoiceChipTitle(dueInvoices)}
            onClick={() => navigate("/invoices")}
          >
            <Icon name="receipt" weight="fill" />
            <span className="invoice-chip-button-label">{invoiceChipLabel(dueInvoices)}</span>
          </button>
        ) : null}
        {securityUnlockAvailable ? (
          <button
            className={securityUnlocked ? "ghost security-lock-toggle active" : "ghost security-lock-toggle"}
            type="button"
            title={securityUnlocked ? "Lock security keys" : "Unlock security key"}
            onClick={securityUnlocked ? lockSecurity : () => openSecurityUnlock()}
          >
            <Icon name={securityUnlocked ? "lock_open" : "lock"} weight={securityUnlocked ? "bold" : "regular"} />
          </button>
        ) : null}
        <details className="account-menu" ref={accountMenuRef}>
          <summary className="user-chip account-menu-summary" title={accountLabel} aria-label="Account menu">
            <span>{accountLabel}</span>
            <Icon name="expand_more" />
          </summary>
          <div className="account-menu-panel" role="menu">
            <div className="account-menu-identity">
              <strong>{accountLabel}</strong>
              <small>{user.email}</small>
            </div>
            {!androidNativeAvailable() ? (
              <button
                className={notificationsEnabled ? "account-menu-row account-menu-notifications active" : "account-menu-row account-menu-notifications"}
                type="button"
                role="switch"
                aria-checked={notificationsEnabled}
                onClick={() => void menuToggleNotifications()}
              >
                <Icon name="notifications" weight={notificationsEnabled ? "bold" : "regular"} />
                <span><strong>Browser notifications</strong><small>{notificationsEnabled ? "Enabled for new mail" : "Paused for this browser"}</small></span>
                <span className="notification-toggle-track"><span /></span>
              </button>
            ) : null}
            {/* Background work is a question about the whole installation, not
                about the folder it happens to be touching, so it is asked from
                here rather than from a row between the mailboxes. */}
            <button className="account-menu-row" type="button" role="menuitem" onClick={() => menuNavigate("/activity")}>
              <Icon name="sync" />
              <span>
                <strong>Syncs &amp; tasks</strong>
                <small>{activeSyncRuns.length > 0 ? `${activeSyncRuns.length.toLocaleString()} running now` : "Every sync and background task"}</small>
              </span>
              {activeSyncRuns.length > 0 ? <span className="account-menu-count">{activeSyncRuns.length.toLocaleString()}</span> : null}
            </button>
            <button className="account-menu-row" type="button" role="menuitem" onClick={() => menuNavigate("/settings/account")}>
              <Icon name="settings" />
              <span><strong>Settings</strong><small>Profile, servers, folders, and identities</small></span>
            </button>
            {user.is_admin ? (
              <button className="account-menu-row" type="button" role="menuitem" onClick={() => menuNavigate("/admin/users")}>
                <Icon name="group" />
                <span><strong>Admin panel</strong><small>Users and server-wide controls</small></span>
              </button>
            ) : null}
            {user.is_admin ? (
              <button className="account-menu-row" type="button" role="menuitem" onClick={() => menuNavigate("/admin/database")}>
                <Icon name="archive" />
                <span><strong>Database</strong><small>Integrity, backups, and repair</small></span>
              </button>
            ) : null}
            <button className="account-menu-row danger" type="button" role="menuitem" onClick={() => void menuLogout()}>
              <Icon name="logout" />
              <span><strong>Log out</strong><small>End this browser session</small></span>
            </button>
          </div>
        </details>
      </nav>
    </header>
  );
}

// deliveryChipLabel names one parcel by its carrier and several by their count.
// "DHL" is more use at a glance than "1 parcel": it is the thing the reader is
// waiting for, and it is what the doorbell will say.
function deliveryChipLabel(expected: ExpectedDeliveries): string {
  if (expected.count === 1) return `Today: ${expected.carrierLabel || "1 parcel"}`;
  return `Today: ${expected.count.toLocaleString()} parcels`;
}

function deliveryChipTitle(expected: ExpectedDeliveries): string {
  const what = expected.count === 1
    ? `A parcel${expected.carrierLabel ? ` from ${expected.carrierLabel}` : ""} is expected today`
    : `${expected.count.toLocaleString()} parcels are expected today`;
  return `${what} - see every shipment`;
}

// invoiceChipLabel says the one thing that changes a reader's afternoon first.
// Being chased outranks being due, because an overdue notice is a deadline that
// has already passed and somebody has noticed.
function invoiceChipLabel(due: DueInvoices): string {
  if (due.chased > 0) return due.chased === 1 ? "1 overdue notice" : `${due.chased.toLocaleString()} overdue notices`;
  if (due.count === 1) return `Due: ${due.issuer || "1 invoice"}`;
  return `${due.count.toLocaleString()} invoices due`;
}

function invoiceChipTitle(due: DueInvoices): string {
  // "Due" here means due today or already overdue: unlike a parcel, a bill
  // whose day has passed has not stopped being today's business.
  const what = due.count === 1
    ? `An invoice${due.issuer ? ` from ${due.issuer}` : ""} is due`
    : `${due.count.toLocaleString()} invoices are due`;
  const chased = due.chased > 0
    ? `, ${due.chased.toLocaleString()} of them chased`
    : "";
  return `${what}${chased} - see every invoice`;
}

// Sidebar turns flat mailbox summaries into a tree, supports folder navigation,
// and accepts dragged message IDs from the message list.
function Sidebar({
  userID,
  mailboxes,
  csrf,
  latestSyncRun,
  activeSyncRuns,
  unfinishedMoveRun,
  syncRunning,
  serverStartedAt,
  serverUptimeSeconds,
  buildVersion,
  buildDate,
  buildLabel,
  buildCommit,
  mailCategories,
  mailCategoriesPending,
  pluginAppLinks,
  currentPath,
  navigate,
  openCompose,
  refreshChrome,
  onMoveMessages,
  onFileMessagesInCategory,
  mobileOpen,
  dragActive,
  touchDrop,
  touchRevealedAccounts,
  onClose
}: {
  userID: number;
  mailboxes: Mailbox[];
  csrf: string;
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  unfinishedMoveRun: SyncRun | null;
  syncRunning: boolean;
  serverStartedAt: string;
  serverUptimeSeconds: number;
  buildVersion: string;
  buildDate: string;
  buildLabel: string;
  buildCommit: string;
  mailCategories: MailCategorySummary[];
  mailCategoriesPending: number;
  pluginAppLinks: readonly PluginAppLink[];
  currentPath: string;
  navigate: (url: string) => void;
  openCompose: (query?: string) => void;
  refreshChrome: () => Promise<Bootstrap | null>;
  onMoveMessages: (messageIDs: number[], mailbox: MoveTarget, action?: MessageTransferAction) => void;
  onFileMessagesInCategory: (messageIDs: number[], category: MailCategoryTarget) => void;
  mobileOpen: boolean;
  dragActive: boolean;
  touchDrop: TouchDropTarget | null;
  touchRevealedAccounts: string[];
  onClose: () => void;
}) {
  const [dropID, setDropID] = useState<number | null>(null);
  const [dropCategory, setDropCategory] = useState<string | null>(null);
  // A touch drag reports its target from the shell, so the two drop kinds are
  // unpacked once here rather than re-tested at every link that draws itself.
  const touchDropMailboxID = touchDrop?.kind === "mailbox" ? touchDrop.id : null;
  const touchDropCategory = touchDrop?.kind === "category" ? touchDrop.name : null;
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set());
  const [collapsedAccounts, setCollapsedAccounts] = useState<Set<string>>(() => loadCollapsedAccounts(userID));
  const [dragRevealedAccounts, setDragRevealedAccounts] = useState<Set<string>>(() => new Set());
  const uptimeLabel = useServerUptimeLabel(serverStartedAt, serverUptimeSeconds);
  const releaseLabel = buildDisplayLabel(buildVersion, buildDate, buildLabel);
  const uptimeParts = [uptimeLabel ? `Up ${uptimeLabel}` : "", releaseLabel].filter(Boolean);
  const shortCommit = buildCommit.trim().slice(0, 8);
  const uptimeTitle = [
    serverStartedAt ? `Started ${new Date(serverStartedAt).toLocaleString()}` : "Server uptime",
    shortCommit ? `Commit ${shortCommit}` : ""
  ].filter(Boolean).join(" · ");
  const listRoute = mailRoute(currentPath);
  const activeMailbox = listRoute.mailboxID;
  const inboxActive = listRoute.view === "inbox";
  const sentActive = listRoute.view === "sent";
  const draftsActive = listRoute.view === "drafts";
  const allMailActive = (currentPath === "/mail" || currentPath.startsWith("/mail/")) && !activeMailbox && !listRoute.view;
  const snoozedActive = currentPath === "/snoozes";
  const accountGroups = useMemo(() => sidebarAccountGroups(mailboxes), [mailboxes]);
  function openList(url: string) {
    navigate(url);
    onClose();
  }
  // One ordered list drives both the links and their numbers, so a shortcut can
  // never point at a different entry than the badge beside it claims.
  const namedLists: NamedListEntry[] = [
    { url: "/mail/inbox", label: "Inbox", icon: "inbox", active: inboxActive, unread: 0, title: "Everything that is not archived yet, across every account" },
    { url: "/mail", label: "All Mail", icon: "mail", active: allMailActive, unread: 0, title: "Every folder that opts into All Mail" },
    { url: "/mail/sent", label: "Sent", icon: "send", active: sentActive, unread: 0, title: "Sent mail across every account" },
    { url: "/mail/drafts", label: "Drafts", icon: "draft", active: draftsActive, unread: 0, title: "Drafts across every account" },
    { url: "/snoozes", label: "Snoozed", icon: "clock", active: snoozedActive, unread: 0, title: "Threads waiting to come back" },
    ...mailCategories.map((category, index) => ({
      url: mailURL(null, 1, category.name),
      label: category.label,
      icon: category.icon || "label",
      active: listRoute.view === category.name,
      unread: category.unread,
      title: `${category.label}: ${messageCountLabel(category.total)}, decided from each message itself and excluding archived mail. Drop mail here to file its sender under ${category.label}.`,
      section: index === 0 ? "Categories" : undefined,
      category: { name: category.name, label: category.label }
    }))
  ];
  const shortcutHintsVisible = useSidebarShortcuts(namedLists, openList);
  const advertiseAndroidApp = shouldAdvertiseAndroidApp();

  useEffect(() => {
    if (dragActive) return;
    setDropID(null);
    setDropCategory(null);
    setDragRevealedAccounts((current) => current.size > 0 ? new Set() : current);
  }, [dragActive]);

  useEffect(() => {
    saveCollapsedAccounts(userID, collapsedAccounts);
  }, [userID, collapsedAccounts]);

  function open(event: MouseEvent, url: string) {
    event.preventDefault();
    openList(url);
  }

  function canAcceptDraggedMessages(event: DragEvent) {
    const types = Array.from(event.dataTransfer.types);
    return types.includes("application/x-rolltop-message-transfer") || types.includes("application/x-rolltop-messages") || types.includes("application/x-rolltop-message");
  }

  function onDragEnter(event: DragEvent, mailboxID: number) {
    if (!canAcceptDraggedMessages(event)) return;
    event.preventDefault();
    setDropID(mailboxID);
  }

  function dragCopyRequested(event: DragEvent) {
    return event.ctrlKey || event.metaKey || event.dataTransfer.dropEffect === "copy";
  }

  function onDragOver(event: DragEvent, mailboxID: number) {
    if (!canAcceptDraggedMessages(event)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = dragCopyRequested(event) ? "copy" : "move";
    setDropID(mailboxID);
  }

  function onDragLeave(event: DragEvent, mailboxID: number) {
    const nextTarget = event.relatedTarget;
    if (nextTarget instanceof Node && event.currentTarget.contains(nextTarget)) return;
    setDropID((current) => current === mailboxID ? null : current);
  }

  function onCategoryDragEnter(event: DragEvent, category: MailCategoryTarget) {
    if (!canAcceptDraggedMessages(event)) return;
    event.preventDefault();
    setDropCategory(category.name);
  }

  // Filing is a statement about the senders, not a transfer, so a category drop
  // has no copy variant: the modifier keys make no difference to what it does.
  function onCategoryDragOver(event: DragEvent, category: MailCategoryTarget) {
    if (!canAcceptDraggedMessages(event)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = "move";
    setDropCategory(category.name);
  }

  function onCategoryDragLeave(event: DragEvent, category: MailCategoryTarget) {
    const nextTarget = event.relatedTarget;
    if (nextTarget instanceof Node && event.currentTarget.contains(nextTarget)) return;
    setDropCategory((current) => current === category.name ? null : current);
  }

  function onCategoryDrop(event: DragEvent, category: MailCategoryTarget) {
    event.preventDefault();
    setDropCategory(null);
    const { ids } = draggedMessages(event);
    if (ids.length === 0) return;
    onFileMessagesInCategory(ids, category);
    onClose();
  }

  function onDrop(event: DragEvent, mailbox: Mailbox) {
    event.preventDefault();
    setDropID(null);
    const { ids, accountIDs } = draggedMessages(event);
    if (ids.length === 0) return;
    const crossAccount = accountIDs.some((accountID) => mailbox.account_id > 0 && accountID !== mailbox.account_id);
    onMoveMessages(ids, { id: mailbox.id, name: mailbox.name }, crossAccount || dragCopyRequested(event) ? "copy" : "move");
    onClose();
  }

  function toggleGroup(key: string) {
    setExpandedGroups((current) => {
      const next = new Set(current);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  function setAccountCollapsed(key: string, collapsed: boolean) {
    setCollapsedAccounts((current) => {
      if (current.has(key) === collapsed) return current;
      const next = new Set(current);
      if (collapsed) next.add(key);
      else next.delete(key);
      return next;
    });
  }

  // Dragging over a collapsed account reveals its folders for the length of the
  // drag only, so an incidental pass over a header never rewrites the saved layout.
  function revealAccountForDrag(key: string) {
    setDragRevealedAccounts((current) => current.has(key) ? current : new Set(current).add(key));
  }

  // Collapse is the stored preference; the active mailbox and an in-flight drag
  // reveal a group for display without touching what the user saved.
  function accountCollapsed(group: SidebarAccountGroup): boolean {
    if (!collapsedAccounts.has(group.key)) return false;
    if (dragRevealedAccounts.has(group.key) || touchRevealedAccounts.includes(group.key)) return false;
    return !group.folders.some((node) => nodeContainsMailbox(node, activeMailbox));
  }

  function folderLink(mailbox: Mailbox, label = mailbox.name, depth = 0) {
    const active = currentPath.startsWith("/mailbox/") && activeMailbox === String(mailbox.id);
    const count = mailbox.unread_count;
    const url = mailURL(mailbox.id);
    return (
      <a
        href={url}
        className={`folder message-drop-target ${depth > 0 ? "folder-child" : ""} ${active ? "active" : ""} ${dropID === mailbox.id || touchDropMailboxID === mailbox.id ? "drop-target" : ""}`}
        data-rolltop-drop-mailbox-id={mailbox.id}
        data-rolltop-drop-mailbox-name={mailbox.name}
        data-rolltop-drop-account-id={mailbox.account_id}
        style={depth > 0 ? { paddingLeft: `${18 + depth * 18}px` } : undefined}
        key={mailbox.id}
        onClick={(event) => open(event, url)}
        onDragEnter={(event) => onDragEnter(event, mailbox.id)}
        onDragOver={(event) => onDragOver(event, mailbox.id)}
        onDragLeave={(event) => onDragLeave(event, mailbox.id)}
        onDrop={(event) => onDrop(event, mailbox)}
      >
        <span className="folder-name"><Icon name={mailbox.icon || "folder"} weight={active ? "bold" : undefined} />{label}</span>
        {count > 0 ? <span className="folder-count">{count.toLocaleString()}</span> : null}
      </a>
    );
  }

  function folderNode(node: FolderNode, depth = 0): ReactNode {
    if (node.children.length === 0) return folderLink(node.mailbox, node.label, depth);
    const active = currentPath.startsWith("/mailbox/") && activeMailbox === String(node.mailbox.id);
    const count = node.mailbox.unread_count;
    const expandKey = folderExpandKey(node.mailbox);
    const expanded = expandedGroups.has(expandKey) || nodeContainsMailbox(node, activeMailbox);
    const url = mailURL(node.mailbox.id);
    return (
      <div className="folder-tree" key={node.mailbox.id}>
        <div
          className={`folder folder-parent message-drop-target ${depth > 0 ? "folder-child" : ""} ${active ? "active" : ""} ${dropID === node.mailbox.id || touchDropMailboxID === node.mailbox.id ? "drop-target" : ""}`}
          data-rolltop-drop-mailbox-id={node.mailbox.id}
          data-rolltop-drop-mailbox-name={node.mailbox.name}
          data-rolltop-drop-account-id={node.mailbox.account_id}
          style={depth > 0 ? { paddingLeft: `${18 + depth * 18}px` } : undefined}
          onDragEnter={(event) => onDragEnter(event, node.mailbox.id)}
          onDragOver={(event) => onDragOver(event, node.mailbox.id)}
          onDragLeave={(event) => onDragLeave(event, node.mailbox.id)}
          onDrop={(event) => onDrop(event, node.mailbox)}
        >
          <a href={url} className="folder-main" onClick={(event) => open(event, url)}>
            <span className="folder-name"><Icon name={node.mailbox.icon || "folder"} weight={active ? "bold" : undefined} />{node.label}</span>
          </a>
          {count > 0 ? <span className="folder-count">{count.toLocaleString()}</span> : null}
          <button className="folder-toggle" type="button" onClick={() => toggleGroup(expandKey)} title={expanded ? "Collapse folder" : "Expand folder"}>
            <Icon name={expanded ? "expand_more" : "chevron_right"} />
          </button>
        </div>
        {expanded ? <div className="folder-children">{node.children.map((child) => folderNode(child, depth + 1))}</div> : null}
      </div>
    );
  }

  return (
    <aside className={`sidebar ${mobileOpen ? "open" : ""} ${dragActive ? "message-drag-active" : ""}`}>
      <div className="sidebar-mobile-head">
        <a
          href="/mail"
          className="brand sidebar-mobile-brand"
          aria-label="Rolltop - All Mail"
          onClick={(event) => open(event, "/mail")}
        >
          <LogoMark />
          <span className="brand-wordmark">rolltop</span>
        </a>
        <button className="ghost" type="button" title="Close folders" aria-label="Close folders" onClick={onClose}><Icon name="close" /></button>
      </div>
      <a href="/compose" className="button compose" onClick={(event) => {
        event.preventDefault();
        onClose();
        openCompose("");
      }}>
        <Icon name="edit" weight="bold" />
        Compose
      </a>
      <div className="sidebar-scroll">
        {namedLists.map((entry, index) => {
          const category = entry.category;
          const dropping = category ? dropCategory === category.name || touchDropCategory === category.name : false;
          return (
            <Fragment key={entry.url}>
              {entry.section ? <div className="side-section">{entry.section}</div> : null}
              <a
                href={entry.url}
                className={`folder ${category ? "message-drop-target" : ""} ${entry.active ? "active" : ""} ${dropping ? "drop-target" : ""}`}
                title={entry.title}
                data-rolltop-drop-category={category?.name}
                data-rolltop-drop-category-label={category?.label}
                onClick={(event) => open(event, entry.url)}
                onDragEnter={category ? (event) => onCategoryDragEnter(event, category) : undefined}
                onDragOver={category ? (event) => onCategoryDragOver(event, category) : undefined}
                onDragLeave={category ? (event) => onCategoryDragLeave(event, category) : undefined}
                onDrop={category ? (event) => onCategoryDrop(event, category) : undefined}
              >
                <span className="folder-name">
                  <Icon name={entry.icon} weight={entry.active ? "bold" : undefined} />
                  {entry.label}
                </span>
                {shortcutHintsVisible && index < maxSidebarShortcuts ? (
                  <span className="folder-shortcut" aria-hidden="true">{index + 1}</span>
                ) : entry.unread > 0 ? (
                  <span className="folder-count">{entry.unread.toLocaleString()}</span>
                ) : null}
              </a>
            </Fragment>
          );
        })}
        {mailCategoriesPending > 0 ? (
          <div className="sidebar-note" title="Stored mail is still being read for its category headers">
            Sorting {mailCategoriesPending.toLocaleString()} more {mailCategoriesPending === 1 ? "message" : "messages"} into categories
          </div>
        ) : null}
        <div className="side-section">Folders</div>
        {accountGroups.map((group) => {
          const collapsed = accountCollapsed(group);
          const unread = collapsed ? folderTreeUnreadCount(group.folders) : 0;
          return (
            <div className="account-folder-group" key={group.key}>
              <button
                type="button"
                className="account-toggle"
                aria-expanded={!collapsed}
                title={collapsed ? "Expand account folders" : "Collapse account folders"}
                data-rolltop-drop-account-key={group.key}
                onClick={() => setAccountCollapsed(group.key, !collapsed)}
                onDragEnter={(event) => {
                  if (collapsed && canAcceptDraggedMessages(event)) revealAccountForDrag(group.key);
                }}
              >
                <Icon name={collapsed ? "chevron_right" : "expand_more"} />
                <span className="account-toggle-label">{group.label}</span>
                {unread > 0 ? <span className="folder-count">{unread.toLocaleString()}</span> : null}
              </button>
              {collapsed ? null : group.folders.map((node) => folderNode(node))}
            </div>
          );
        })}
        <div className="side-section">Organizer</div>
        {organizerLinks.map((entry) => {
          const active = organizerRoute(currentPath, entry.route);
          const url = organizerURL(entry.route);
          return (
            <a
              key={entry.route}
              href={url}
              className={`folder ${entry.gap ? "folder-group-break" : ""} ${active ? "active" : ""}`}
              onClick={(event) => open(event, url)}
            >
              <span className="folder-name">
                <Icon name={entry.icon} weight={active ? "bold" : undefined} />
                {entry.label}
              </span>
            </a>
          );
        })}
        {pluginAppLinks.filter((entry) => entry.label).map((entry) => {
          const active = entry.path === currentPath || (entry.nested === true && currentPath.startsWith(`${entry.path}/`));
          return (
            <a
              key={entry.path}
              href={entry.path}
              className={`folder ${active ? "active" : ""}`}
              onClick={(event) => open(event, entry.path)}
            >
              <span className="folder-name">
                <Icon name={entry.icon || "folder"} weight={active ? "bold" : undefined} />
                {entry.label}
              </span>
            </a>
          );
        })}
        {advertiseAndroidApp ? (
          <>
            <div className="side-section">Android app</div>
            <a href="/android/rolltop.apk" className="folder android-app-download" download="rolltop.apk">
              <span className="folder-name"><Icon name="android" weight="fill" />Get Rolltop for Android</span>
              <Icon name="download" />
            </a>
          </>
        ) : null}
      </div>
      <SidebarSync mailboxes={mailboxes} csrf={csrf} latest={latestSyncRun} activeRuns={activeSyncRuns} unfinishedMove={unfinishedMoveRun} running={syncRunning} refreshChrome={refreshChrome} />
      {uptimeParts.length > 0 ? (
        <div className="sidebar-uptime" title={uptimeTitle}>
          {uptimeParts.join(" · ")}
        </div>
      ) : null}
      <div className="sidebar-license">
        GNU AGPLv3-or-later
      </div>
    </aside>
  );
}

/**
 * PluginAppLink is one plugin-owned page as the shell needs it: the path the
 * router claims, plus the label and icon the sidebar draws it with. A link
 * whose module has not loaded yet has no label, and an entry with no label is
 * not drawn -- a nameless row in the sidebar says less than no row at all.
 */
export type PluginAppLink = PluginAppRoute & { label?: string; icon?: string };

/**
 * organizerLinks are the sidebar's non-mail destinations, in the order they are
 * drawn. Each carries only its label and icon; which paths belong to it is the
 * router's answer, read through organizerRoute, so an entry cannot light up for
 * a path that would render the mail list instead.
 *
 * `gap` opens a blank line above an entry. The four sit under one heading
 * because a heading per single entry only repeated the entry, but they are two
 * different kinds of thing: the calendar and the address book are what this
 * reader keeps, while parcels and invoices are what the mail brought in and
 * still wants something done about. A gap says that much without spending a
 * second heading on it.
 */
const organizerLinks: { route: OrganizerRoute; label: string; icon: string; gap?: boolean }[] = [
  { route: "calendar", label: "Calendar", icon: "calendar" },
  { route: "contacts", label: "Contacts", icon: "group" },
  { route: "deliveries", label: "Parcels", icon: "package", gap: true },
  { route: "invoices", label: "Invoices", icon: "receipt" }
];

/**
 * NamedListEntry is one of the sidebar's whole-account lists. They are numbered
 * in the order they appear, so the array order is the shortcut order.
 */
type NamedListEntry = {
  url: string;
  label: string;
  icon: string;
  active: boolean;
  unread: number;
  title: string;
  /** Section heading rendered above this entry, when it starts a new group. */
  section?: string;
  /**
   * The category this entry lists, when it is one. Only these entries take a
   * drop: a folder is somewhere mail can be put, while Inbox, All Mail, Sent,
   * Drafts and Snoozed are questions about mail that dropping cannot answer.
   */
  category?: MailCategoryTarget;
};

type SidebarAccountGroup = {
  key: string;
  label: string;
  folders: FolderNode[];
};

function sidebarAccountGroups(mailboxes: Mailbox[]): SidebarAccountGroup[] {
  const grouped = new Map<string, { key: string; label: string; mailboxes: Mailbox[] }>();
  for (const mailbox of mailboxes) {
    const key = mailbox.account_id ? String(mailbox.account_id) : `email:${mailbox.account_email || "local"}`;
    const existing = grouped.get(key);
    if (existing) {
      existing.mailboxes.push(mailbox);
      continue;
    }
    grouped.set(key, { key, label: mailboxAccountLabel(mailbox), mailboxes: [mailbox] });
  }

  const groups = Array.from(grouped.values());
  const labelCounts = groups.reduce((counts, group) => {
    counts.set(group.label, (counts.get(group.label) || 0) + 1);
    return counts;
  }, new Map<string, number>());

  return groups
    .map((group) => ({
      key: group.key,
      label: (labelCounts.get(group.label) || 0) > 1 ? `${group.label} · Account ${group.key}` : group.label,
      folders: folderTree(group.mailboxes)
    }))
    .filter((group) => group.folders.length > 0);
}

function mailboxAccountLabel(mailbox: Mailbox): string {
  const label = (mailbox.account_label || mailbox.account_email || "").trim();
  if (label) return label;
  return mailbox.account_id ? `Account ${mailbox.account_id}` : "Mail account";
}

function folderExpandKey(mailbox: Mailbox): string {
  return `${mailbox.account_id}:${mailbox.name}`;
}

function SidebarSync({
  mailboxes,
  csrf,
  latest,
  activeRuns,
  unfinishedMove,
  running,
  refreshChrome
}: {
  mailboxes: Mailbox[];
  csrf: string;
  latest: SyncRun | null;
  activeRuns: SyncRun[];
  unfinishedMove: SyncRun | null;
  running: boolean;
  refreshChrome: () => Promise<Bootstrap | null>;
}) {
  const [busy, setBusy] = useState(false);
  const orderedActiveRuns = useMemo(() => stableSyncRunOrder(activeRuns), [activeRuns]);
  // Historical runs belong on the settings history page. The sidebar is live
  // activity only: falling back to the latest interrupted/no-op row made a
  // settled mailbox look as though it were perpetually syncing.
  // IMAP STATUS probes are similarly background housekeeping. They can fan out
  // across every folder while a remote import is active, but they neither copy
  // mail nor need attention, so keep them out of the chrome altogether.
  const visibleRuns = orderedActiveRuns.filter((run) => !isSyncRunChecking(run));
  const isActive = visibleRuns.length > 0 || (running && activeRuns.length === 0);
  const controlsBusy = busy || running || activeRuns.length > 0;
  const latestVisible = visibleRuns[visibleRuns.length - 1] || null;
  // A whole-filter delete is a background move, and a move that could not finish
  // has to be reported here: the messages the user asked to delete are still
  // sitting in their folders. The server picks that run, because it cannot be
  // recognised from the latest run alone — a finished move immediately queues
  // the mailbox refresh whose own run supersedes it.
  const reportMove = !isActive ? unfinishedMove : null;

  async function startSync() {
    setBusy(true);
    try {
      await api.syncAccount(csrf);
      await refreshChrome();
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className={`sidebar-sync ${isActive ? "running" : reportMove ? "attention" : "idle"}`}>
      <div className="sync-meta">
        <strong>{isActive ? `Syncing${visibleRuns.length > 1 ? ` (${visibleRuns.length})` : ""}` : "Sync"}</strong>
        <span>{isActive
          ? latestVisible ? `${latestVisible.status}${latestVisible.current_mailbox ? ` - ${latestVisible.current_mailbox}` : ""}` : "starting"
          : reportMove ? "Move incomplete" : "Up to date"}</span>
        <button className="secondary" type="button" disabled={controlsBusy} onClick={startSync}>
          <Icon name="sync" />
			{controlsBusy ? "Syncing" : "Sync now"}
        </button>
      </div>
      <div className="sync-run-list">
        {visibleRuns.map((run) => (
          <SyncRunMini key={run.id} run={run} mailbox={syncRunMailbox(run, mailboxes)} />
        ))}
      </div>
      {reportMove ? (
        <div className="sync-run-problem" role="status">
          <strong>Move did not finish</strong>
          <span>{reportMove.error}</span>
        </div>
      ) : null}
    </section>
  );
}

// A run is created before IMAP STATUS has determined whether there are any
// UIDs to fetch. Calling that short no-op phase "Syncing" made an unchanged
// INBOX look permanently busy, especially with two IMAP accounts.
function isSyncRunChecking(run: SyncRun): boolean {
	return run.status === "running" && run.messages_total === 0 && run.messages_stored === 0 &&
		run.latest_new_from !== "rolltop:maintenance" && !isMoveRun(run);
}

// Moves are the runs a user starts by deleting or dragging mail, rather than
// background mirroring, so they are labelled and surfaced on their own terms.
function isMoveRun(run: SyncRun): boolean {
	return run.latest_new_from === "rolltop:move";
}


function stableSyncRunOrder(runs: SyncRun[]) {
  return [...runs].sort((a, b) => {
    const startedA = Date.parse(a.started_at) || 0;
    const startedB = Date.parse(b.started_at) || 0;
    if (startedA !== startedB) return startedA - startedB;
    return a.id - b.id;
  });
}

function syncRunMailbox(run: SyncRun, mailboxes: Mailbox[]): Mailbox | undefined {
  const name = run.current_mailbox.trim().toLowerCase();
  if (!name) return undefined;
  return mailboxes.find((mailbox) =>
    mailbox.account_id === run.account_id && mailbox.name.trim().toLowerCase() === name
  );
}

type MirrorResumeEstimate = {
  completed: number;
  total: number;
  approximate: boolean;
};

// Mailbox counts and UID checkpoints survive a process restart, unlike the
// short-lived sync run. Prefer exact IMAP/local counts; UID range is a clearly
// labelled fallback because IMAP UIDs can have gaps.
function mirrorResumeEstimate(mailbox: Mailbox | undefined): MirrorResumeEstimate | null {
  if (!mailbox) return null;
  const local = Math.max(0, mailbox.local_message_count ?? mailbox.message_count);
  const remote = Math.max(0, mailbox.remote_message_count);
  if (remote > 0) {
    return { completed: Math.min(local, remote), total: remote, approximate: false };
  }
  const uidTotal = Math.max(0, mailbox.remote_uid_next - 1);
  if (uidTotal <= 0) return null;
  return { completed: Math.min(Math.max(0, mailbox.last_uid), uidTotal), total: uidTotal, approximate: true };
}

/** Render current-run activity alongside the durable local mirror checkpoint. */
export function SyncRunMini({ run, mailbox }: { run: SyncRun; mailbox?: Mailbox }) {
	const isChecking = isSyncRunChecking(run);
  const totalMessages = run.messages_total || 0;
  const totalFolders = run.mailboxes_total || 0;
  const runProgress = totalMessages > 0
    ? Math.min(100, Math.round((run.messages_seen / totalMessages) * 100))
      : totalFolders > 0
        ? Math.min(100, Math.round((run.mailboxes_done / totalFolders) * 100))
        : run.status === "running" ? 100 : 0;
  const isPurge = run.latest_new_from === "rolltop:maintenance" && run.latest_new_subject.trim().toLowerCase().startsWith("purging");
  const isMove = isMoveRun(run);
  const isMaintenance = run.latest_new_from === "rolltop:maintenance";
	const resume = run.status === "running" && !isChecking && !isPurge && !isMove && !isMaintenance ? mirrorResumeEstimate(mailbox) : null;
  const resumePercent = resume && resume.total > 0 ? Math.round((resume.completed * 100) / resume.total) : 0;
  const progress = resume ? resumePercent : runProgress;
	const processedLabel = run.messages_stored > 0 ? `${run.messages_stored.toLocaleString()} processed` : isChecking ? "Checking IMAP status..." : "Syncing...";
  const resumeLabel = resume
    ? resume.approximate
      ? `about ${resume.completed.toLocaleString()} of ${resume.total.toLocaleString()} already mirrored`
      : `${resume.completed.toLocaleString()} of ${resume.total.toLocaleString()} already mirrored`
    : "";
  // The bar tracks how far through the batch the run is, but the count says how
  // many messages actually arrived: a move that steps over unmovable messages
  // reaches the end of its batch without having moved all of them.
  const movedLabel = totalMessages > 0
    ? `${run.messages_stored.toLocaleString()} of ${totalMessages.toLocaleString()} moved`
    : "Moving...";
  const purgeLabel = totalMessages > 0
    ? `${run.messages_seen.toLocaleString()} of ${totalMessages.toLocaleString()} purged`
    : "Purging...";
  const detail = isPurge
    ? purgeLabel
    : isMove
      ? movedLabel
      : run.messages_skipped > 0
        ? `${processedLabel}, ${run.messages_skipped.toLocaleString()} skipped`
        : processedLabel;
  return (
    <div className="sync-run-mini">
      <div className="sync-run-title">
        <span>{run.current_mailbox || run.status}</span>
        <span>{progress}%</span>
      </div>
      <div className="sync-run-detail">{resumeLabel ? `${resumeLabel} · ${detail}` : detail}</div>
      <div className="progress" aria-label={`${run.current_mailbox || "Sync"} progress`}>
        <div style={{ width: `${progress}%` }} />
      </div>
    </div>
  );
}
