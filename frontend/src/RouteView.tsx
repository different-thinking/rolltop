// File overview: Small route switch for the single-page app. It translates the parsed location
// into feature views while passing only the shared state each view needs.

import type { AddToast, LocationState, SecurityUnlockState } from "./appTypes";
import type { AccountMailboxChoice, Bootstrap, MailCategorySummary, Mailbox, SwipePreferences, SyncRun, ThemeDefinition, User } from "./types";
import { MailView, SearchView, SnoozedView } from "./features/mail/MailViews";
import { ThreadView } from "./features/mail/ThreadView";
import { ComposePage } from "./features/compose/ComposeViews";
import { ContactsView } from "./features/contacts/ContactsView";
import { DeliveriesView } from "./features/deliveries/DeliveriesView";
import { InvoicesView } from "./features/invoices/InvoicesView";
import { CalendarView } from "./features/calendar/CalendarView";
import { SettingsView, AdminUsersView, SyncRunView } from "./features/settings/SettingsViews";
import { ActivityView } from "./features/activity/ActivityView";
import { AdminDatabaseView } from "./features/settings/admin/DatabasePanel";
import { mailRouteView } from "./lib/routes";
import type { RuntimePlugins } from "./plugins/runtime";
import { securityUnlockPlugin } from "./plugins/securityUnlock";

/**
 * RouteView is the app's manual router. Each branch maps one URL family to a
 * feature view and passes shared chrome state downward without letting features
 * import App-level bootstrap or navigation state directly.
 */
export function RouteView({
  csrf,
  user,
  mailboxes,
  latestSyncRun,
  activeSyncRuns,
  syncRunning,
  mailGeneration,
  swipePreferences,
  archiveMailboxes,
  mailCategories,
  enabledPlugins,
  availableThemes,
  location,
  navigate,
  replaceRoute,
  hiddenMessageIDs,
  setMessagesHidden,
  openCompose,
  refreshChrome,
  runtimePlugins,
  reloadRuntimePlugins,
  securityUnlock,
  openSecurityUnlock,
  addToast
}: {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  latestSyncRun: SyncRun | null;
  activeSyncRuns: SyncRun[];
  syncRunning: boolean;
  mailGeneration: number;
  swipePreferences: SwipePreferences;
  archiveMailboxes: AccountMailboxChoice[];
  mailCategories: MailCategorySummary[];
  enabledPlugins: string[];
  availableThemes: ThemeDefinition[];
  location: LocationState;
  navigate: (url: string) => void;
  replaceRoute: (url: string) => void;
  hiddenMessageIDs: Set<number>;
  setMessagesHidden: (messageIDs: number[], hidden: boolean) => void;
  openCompose: (query?: string) => void;
  refreshChrome: () => Promise<Bootstrap | null>;
  runtimePlugins: RuntimePlugins;
  reloadRuntimePlugins: () => Promise<void>;
  securityUnlock: SecurityUnlockState;
  openSecurityUnlock: (identityID?: number, onUnlocked?: (state: SecurityUnlockState) => void, recipientKeyIDs?: string[], fallbackEmail?: string) => void;
  addToast: AddToast;
}) {
  const securityEnabled = Boolean(securityUnlockPlugin(runtimePlugins.all));
  if (location.path === "/snoozes") {
    return <SnoozedView csrf={csrf} datePrefs={user} location={location} navigate={navigate} hiddenMessageIDs={hiddenMessageIDs} mailboxes={mailboxes} swipePreferences={swipePreferences} archiveMailboxes={archiveMailboxes} mailGeneration={mailGeneration} messageSecurityPlugins={runtimePlugins.all} addToast={addToast} />;
  }
  if (location.path === "/search" || location.path.startsWith("/search/")) {
    return <SearchView csrf={csrf} userID={user.id} location={location} navigate={navigate} replaceRoute={replaceRoute} hiddenMessageIDs={hiddenMessageIDs} datePrefs={user} mailboxes={mailboxes} swipePreferences={swipePreferences} archiveMailboxes={archiveMailboxes} activeSyncRuns={activeSyncRuns} mailGeneration={mailGeneration} messageSecurityPlugins={runtimePlugins.all} searchActionPlugins={runtimePlugins.all} addToast={addToast} />;
  }
  if (location.path.startsWith("/messages/")) {
    return (
      <ThreadView
        userID={user.id}
        csrf={csrf}
        datePrefs={user}
        location={location}
        navigate={navigate}
        mailboxes={mailboxes}
        archiveMailboxes={archiveMailboxes}
        mailCategories={mailCategories}
        setMessagesHidden={setMessagesHidden}
        enabledPlugins={enabledPlugins}
        refreshChrome={refreshChrome}
        openCompose={openCompose}
        messageSecurityPlugins={runtimePlugins.all}
        securityUnlock={securityUnlock}
        openSecurityUnlock={openSecurityUnlock}
        addToast={addToast}
      />
    );
  }
  // Everything below is a route that is not mail, so it is reached only when
  // mailRouteView agrees - including whether this reader may open the admin
  // screens, which is why those branches no longer test it a second time. The
  // shell asks that same function which views take the reading measure; deciding
  // the branch by it rather than beside it is what keeps a route from being mail
  // to one of them and not to the other.
  if (!mailRouteView(location.path, Boolean(user.is_admin))) {
    if (location.path === "/compose") {
      return <ComposePage userID={user.id} csrf={csrf} location={location} navigate={navigate} securityEnabled={securityEnabled} securityPlugins={runtimePlugins.all} securityUnlock={securityUnlock} openSecurityUnlock={openSecurityUnlock} addToast={addToast} />;
    }
    if (location.path === "/calendar" || location.path.startsWith("/calendar/")) {
      return <CalendarView csrf={csrf} location={location} navigate={navigate} addToast={addToast} />;
    }
    if (location.path === "/contacts") {
      return <ContactsView csrf={csrf} contactPlugins={runtimePlugins.all} addToast={addToast} />;
    }
    if (location.path === "/deliveries") {
      return <DeliveriesView csrf={csrf} datePrefs={user} mailGeneration={mailGeneration} navigate={navigate} addToast={addToast} />;
    }
    if (location.path === "/invoices") {
      return <InvoicesView csrf={csrf} datePrefs={user} mailGeneration={mailGeneration} navigate={navigate} addToast={addToast} />;
    }
    if (location.path === "/settings/account" || location.path.startsWith("/settings/account/")) {
      return <SettingsView key={user.id} csrf={csrf} user={user} mailboxes={mailboxes} mailCategories={mailCategories} swipePreferences={swipePreferences} latestSyncRun={latestSyncRun} activeSyncRuns={activeSyncRuns} syncRunning={syncRunning} availableThemes={availableThemes} location={location} navigate={navigate} replaceRoute={replaceRoute} refreshChrome={refreshChrome} runtimePlugins={runtimePlugins} reloadRuntimePlugins={reloadRuntimePlugins} addToast={addToast} />;
    }
    if (location.path === "/admin/users") {
      return <AdminUsersView csrf={csrf} refreshChrome={refreshChrome} addToast={addToast} />;
    }
    if (location.path === "/admin/database") {
      return <AdminDatabaseView csrf={csrf} datePrefs={user} />;
    }
    if (location.path === "/activity") {
      return <ActivityView csrf={csrf} datePrefs={user} activeSyncRuns={activeSyncRuns} mailGeneration={mailGeneration} navigate={navigate} addToast={addToast} />;
    }
    if (location.path.startsWith("/sync-runs/")) {
      return <SyncRunView csrf={csrf} location={location} navigate={navigate} datePrefs={user} />;
    }
  }
  return (
    <MailView
      userID={user.id}
      csrf={csrf}
      datePrefs={user}
      location={location}
      navigate={navigate}
      replaceRoute={replaceRoute}
      hiddenMessageIDs={hiddenMessageIDs}
      mailboxes={mailboxes}
      latestSyncRun={latestSyncRun}
      activeSyncRuns={activeSyncRuns}
      mailGeneration={mailGeneration}
      swipePreferences={swipePreferences}
      archiveMailboxes={archiveMailboxes}
      mailCategories={mailCategories}
      refreshChrome={refreshChrome}
      addToast={addToast}
      messageSecurityPlugins={runtimePlugins.all}
    />
  );
}
