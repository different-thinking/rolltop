// File overview: Wire-level TypeScript shapes returned by the Go API. These mirror JSON payloads,
// so field names intentionally stay snake_case instead of being adapted per component.

/** User mirrors the authenticated local account and display preferences returned by the API. */
export type User = {
  id: number;
  email: string;
  name: string;
  backup_email?: string;
  is_admin: boolean;
  date_locale: string;
  date_format: "mdy" | "dmy" | "ymd" | "locale" | string;
  theme: "classic" | "classic_dark" | "matrix" | string;
  search_preset: "strict" | "balanced" | "forgiving" | string;
  search_recency_bias: "none" | "light" | "normal" | "strong" | string;
  search_fuzzy: "off" | "balanced" | "forgiving" | string;
  search_sender_boost: boolean;
  search_sender_history: "none" | "light" | "normal" | "strong" | string;
  search_contact_boost: "none" | "light" | "normal" | "strong" | string;
  search_attachment_weight: "off" | "light" | "normal" | "strong" | string;
  search_compact_splitting: boolean;
};

export type SwipeAction = "trash" | "archive" | "snooze" | "mark_read" | "mark_unread";

export type SwipeSnoozePreset = "later_today" | "tomorrow" | "next_week";

/** AccountMailboxChoice is one account's pick for a folder role the app assigns. */
export type AccountMailboxChoice = {
  account_id: number;
  mailbox_id: number;
};

export type SwipeArchiveMailbox = AccountMailboxChoice;

export type SwipePreferences = {
  left_action: SwipeAction;
  left_snooze_preset: SwipeSnoozePreset;
  right_action: SwipeAction;
  right_snooze_preset: SwipeSnoozePreset;
  archive_mailboxes: AccountMailboxChoice[];
};

/**
 * RetentionMode says how a retention cutoff is expressed: "relative" keeps mail
 * for a number of days and moves with the calendar, "fixed" names one day and
 * stays there, and "off" deletes nothing.
 */
export type RetentionMode = "off" | "relative" | "fixed";

/** RetentionUnit is the calendar step a relative cutoff counts in. */
export type RetentionUnit = "days" | "months" | "years";

/** CategoryRetention is one category's answer to how long its mail is kept. */
export type CategoryRetention = {
  category: string;
  mode: RetentionMode;
  /**
   * The relative cutoff, on the calendar: six months is six months rather than
   * a rounded number of days. Both are zero and empty unless the mode is
   * relative.
   */
  count: number;
  unit: RetentionUnit | "";
  /** The fixed cutoff as an RFC 3339 timestamp, empty unless the mode is fixed. */
  before: string;
};

/**
 * RetentionSettings is the whole policy: how long the Trash keeps what was
 * thrown away, and what each category keeps before it is thrown away. Only the
 * categories with a rule are listed; a category that is absent deletes nothing.
 */
export type RetentionSettings = {
  trash_enabled: boolean;
  trash_days: number;
  categories: CategoryRetention[];
};

/** Mailbox mirrors a folder summary row including sync, visibility, and indexing counters. */
export type Mailbox = {
  id: number;
  account_id: number;
  account_email: string;
  account_label: string;
  name: string;
  message_count: number;
  unread_count: number;
  sync_mode: "auto" | "manual" | "never" | string;
  role: "inbox" | "sent" | "drafts" | "trash" | "junk" | "" | string;
  icon: string;
  show_in_sidebar: boolean;
  show_in_all_mail: boolean;
  include_in_search: boolean;
  last_uid: number;
  remote_message_count: number;
  remote_unread_count: number;
  remote_uid_next: number;
  sync_percent: number;
  local_message_count?: number;
  cached_message_count?: number;
  local_sync_percent?: number;
  search_indexed_count?: number;
  search_index_total?: number;
  search_index_percent?: number;
  search_index_purged?: boolean;
  search_index_state_known?: boolean;
};

/** Message is the API's compact message record used in lists and thread cards. */
export type Message = {
  id: number;
  account_id: number;
  mailbox_id: number;
  subject: string;
  from_addr: string;
  to_addr: string;
  cc_addr: string;
  date: string;
  date_short: string;
  is_read: boolean;
  is_starred: boolean;
  has_attachments: boolean;
  is_encrypted: boolean;
  is_signed: boolean;
  /** Empty until classification has read the message's headers. */
  category?: string;
  snippet: string;
  annotations?: MessageAnnotation[];
  /** invoice is absent for the mail that is not about a bill. */
  invoice?: MessageInvoiceSummary;
  /** shipment is absent for the mail that is not about a parcel, which is
   * nearly all of it. */
  shipment?: MessageShipmentSummary;
};

/** MessageAnnotation is compact, non-sensitive metadata supplied by an enabled backend plugin. */
export type MessageAnnotation = {
  plugin_id: string;
  kind: string;
  label: string;
  level: string;
  summary: string;
  metadata?: Record<string, string>;
};

/** Conversation is a list/search row grouped around the latest visible thread message. */
export type Conversation = {
  message: Message;
  message_ids?: number[];
  message_account_ids?: number[];
  starred_message_id: number;
  participants: string;
  recipient_participants: string;
  count: number;
  is_read: boolean;
  has_attachments: boolean;
  attachment_names?: string[];
  attachment_matches?: string[];
  attachment_content_matched?: boolean;
  snippet: string;
  match_terms?: string[];
  match_query_terms?: string[];
  snoozed_until?: string;
  /** The instant the server sorted this row by: max(snooze return, message
   * date). Date sections group by it so a message returning from a snooze
   * heads the section its position in the list actually belongs to. */
  list_date?: string;
};

/**
 * ConversationListPage is what every paged conversation list has in common.
 * Only the orders they can be drawn in differ, so the helpers that filter a page
 * take this and each list names its own sort.
 */
export type ConversationListPage = {
  conversations: Conversation[];
  page: number;
  has_prev: boolean;
  has_next: boolean;
};

/** MailListResponse is one paged conversation list returned by /api/mail. */
export type MailListResponse = ConversationListPage & {
  /** sort echoes the date direction the server applied; older servers omit it. */
  sort?: "newest" | "oldest";
};

/**
 * SearchListResponse is one paged result list returned by /api/search. Its sort
 * carries the best-match ranking as well as the two date orders, because that
 * ranking is an order the results list offers and the mail list has no
 * equivalent of.
 */
export type SearchListResponse = ConversationListPage & {
  /** sort echoes the order the server applied; older servers omit it. */
  sort?: "best" | "newest" | "oldest";
};

/**
 * ScopeMoveResponse reports what a whole-filter move resolved server-side.
 * `matched` counts the messages the filter selected, `skipped` those already in
 * the folder they would move to, and `truncated` says the filter has more
 * matches than one pass moves, so another pass is needed to finish it.
 */
export type ScopeMoveResponse = {
  ok: boolean;
  queued: boolean;
  matched: number;
  skipped: number;
  queued_messages?: number;
  truncated: boolean;
  runs: { run_id: number; account_id: number; mailbox: string; messages: number }[];
  partial_error?: string;
};

export type MessageSnooze = {
  id: number;
  message_id: number;
  snoozed_until: string;
  created_at: string;
  updated_at: string;
};

/** Attachment is message attachment metadata plus optional preview/search match details. */
export type Attachment = {
  id: number;
  filename: string;
  content_type: string;
  size: number;
  download_url: string;
  matched?: boolean;
  content_matched?: boolean;
  match_terms?: string[];
  actions?: AttachmentAction[];
  pgp_public_key_candidate?: boolean;
  preview?: AttachmentPreview;
};

/** AttachmentAction is a plugin-provided operation available for an attachment. */
export type AttachmentAction = {
  plugin_id: string;
  kind: string;
  label: string;
  metadata?: Record<string, string>;
};

/** AttachmentPreview describes a plugin-provided in-browser preview option. */
export type AttachmentPreview = {
  available: boolean;
  kind: string;
  url: string;
  status: string;
  plugin_id: string;
};

/** HeaderDetail is one expanded message header row shown in thread details. */
export type HeaderDetail = {
  label: string;
  value: string;
};

/** AuthenticationResult is a receiver-reported header result, not a Rolltop verification. */
export type AuthenticationResult = {
  result: string;
  source: "authentication-results" | "received-spf";
};

export type MessageSecuritySignal = {
  kind: "sender_display_address_mismatch" | "reply_to_domain_mismatch" | "link_destination_mismatch" | "risky_link_scheme";
  display_host?: string;
  target_host?: string;
  scheme?: string;
};

export type MessageSecurityIndicators = {
  reported_authentication?: {
    spf?: AuthenticationResult;
    dkim?: AuthenticationResult;
    dmarc?: AuthenticationResult;
  };
  signals?: MessageSecuritySignal[];
};

/** ThreadMessage is the render-ready payload for one message inside a conversation. */
export type ThreadMessage = {
  message: Message;
  attachments: Attachment[];
  header_details: HeaderDetail[];
  security_indicators?: MessageSecurityIndicators;
  one_click_unsubscribe: boolean;
  one_click_unsubscribe_sent_at: string;
  sender_name: string;
  sender_email: string;
  sender_initial: string;
  sender_visual?: SenderVisual;
  recipient_line: string;
  snippet: string;
  body_doc: string;
  full_body_doc: string;
  has_hidden_quoted: boolean;
  has_display_body: boolean;
  body_preview_only: boolean;
  has_remote_images: boolean;
  images_allowed: boolean;
  expanded: boolean;
  reply_subject: string;
  can_reply_all: boolean;
  /**
   * has_autocrypt_header is true when this message was parsed with an Autocrypt
   * header. The thread view only fetches the full source to probe for a peer
   * key when it is set, rather than doing so for every message on every open.
   */
  has_autocrypt_header: boolean;
  /**
   * copy_ids names every physical message row this drawn message stands for,
   * itself included. It is absent unless a mirrored label view (Gmail's All
   * Mail) held a second copy that the thread hid behind this one, and read-state
   * changes have to reach all of them.
   */
  copy_ids?: number[];
};


/** MessageOriginalSource is the raw RFC822 source fetched on demand for View Original. */
export type MessageOriginalSource = {
  filename: string;
  source: string;
};

/** SearchExplanation describes one on-demand Bleve scoring explanation for a message. */
export type SearchExplanation = {
  matched: boolean;
  query: string;
  reason?: string;
  score?: number;
  message_id?: number;
  requested_message_id?: number;
  terms?: string[];
  query_terms?: string[];
  fields?: string[];
  field_matches?: SearchFieldMatch[];
  term_contributions?: SearchTermContribution[];
  boosts?: SearchBoost[];
  raw?: ScoreExplanationNode;
};

export type SearchFieldMatch = {
  field: string;
  terms: string[];
};

export type SearchTermContribution = {
  field: string;
  section: string;
  term: string;
  query_term: string;
  score: number;
  term_frequency?: number;
  field_norm?: number;
  idf?: number;
  query_weight?: number;
  boost?: number;
  query_norm?: number;
};

export type SearchBoost = {
  kind: string;
  label: string;
  description: string;
  value?: string;
  boost?: number;
};

export type ScoreExplanationNode = {
  value?: number;
  message: string;
  children?: ScoreExplanationNode[];
};

/** SenderVisual identifies a plugin-provided sender avatar or brand image. */
export type SenderVisual = {
  plugin_id: string;
  kind: string;
  url: string;
};

export type ThemeDefinition = {
  id: string;
  name: string;
  plugin_id?: string;
  css_url?: string;
};

export type FrontendPluginDefinition = {
  id: string;
  name: string;
  version?: string;
  module_url: string;
  css_url?: string;
};

/** ContactEmail is one editable email row on a contact. */
export type ContactEmail = {
  id?: number;
  label: string;
  email: string;
  is_primary: boolean;
};

export type ContactPGPKey = {
  id?: number;
  contact_id?: number;
  email: string;
  label: string;
  fingerprint: string;
  key_id: string;
  user_ids: string;
  public_key_armored: string;
  source_kind?: string;
  source_detail?: string;
  is_preferred: boolean;
};

/** ContactPhone is one editable phone row on a contact. */
export type ContactPhone = {
  id?: number;
  label: string;
  number: string;
  is_primary: boolean;
};

/** ContactAddress is one editable postal address row on a contact. */
export type ContactAddress = {
  id?: number;
  label: string;
  street: string;
  locality: string;
  region: string;
  postal_code: string;
  country: string;
  is_primary: boolean;
};

/** ContactURL is one editable URL row on a contact. */
export type ContactURL = {
  id?: number;
  label: string;
  url: string;
  is_primary: boolean;
};

/** Contact is the API address-book shape including nested detail rows and icon URL. */
export type Contact = {
  id: number;
  name_prefix: string;
  given_name: string;
  additional_name: string;
  family_name: string;
  name_suffix: string;
  display_name: string;
  nickname: string;
  organization: string;
  department: string;
  job_title: string;
  birthday: string;
  notes: string;
  categories: string;
  is_me: boolean;
  is_primary: boolean;
  /** source is "local" or "google". A Google contact is a mirror: edits and
   * deletions travel to that account, and the sync overwrites it. */
  source: string;
  /** google_connection_id names the account that owns the contact, and on a
   * create it asks for the contact to be saved there. Zero means local. */
  google_connection_id: number;
  emails: ContactEmail[];
  phones: ContactPhone[];
  addresses: ContactAddress[];
  urls: ContactURL[];
  pgp_keys?: ContactPGPKey[];
  icon_url: string;
};

/** ContactAutocomplete is a flattened recipient suggestion for compose. */
export type ContactAutocomplete = {
  contact_id: number;
  name: string;
  email: string;
  label: string;
  icon_url: string;
};

/** ComposeIdentity is a selectable From identity returned for compose/reply forms. */
export type ComposeIdentity = {
  id: number;
  pgp_identity_id: number;
  label: string;
  email: string;
  header: string;
  signature: string;
  icon_url: string;
  is_primary: boolean;
  autocrypt_enabled: boolean;
  has_pgp_private_key?: boolean;
  pgp_public_key_armored?: string;
};

/** PluginSetting is the admin-visible enablement state for one plugin. */
export type PluginSetting = {
  id: string;
  name: string;
  description: string;
  enabled: boolean;
  enabled_by_default: boolean;
  heavy: boolean;
  experimental?: boolean;
  backend_error?: string;
};

/** SyncRun is the API progress/status shape for sync and maintenance jobs. */
export type SyncRun = {
  id: number;
  account_id: number;
  status: string;
  started_at: string;
  finished_at: string;
  updated_at: string;
  messages_seen: number;
  messages_stored: number;
  messages_skipped: number;
  new_messages: number;
  latest_new_from: string;
  latest_new_subject: string;
  latest_new_message_id: number;
  messages_total: number;
  mailboxes_done: number;
  mailboxes_total: number;
  current_mailbox: string;
  current_uid: number;
  error: string;
};

export type SyncRunLiveDetail = {
  active: boolean;
  cancellable: boolean;
  phase: string;
  detail: string;
  phase_started_at: string;
};

/** SyncFolder combines mailbox settings with current/last sync run information. */
export type SyncFolder = {
  mailbox: Mailbox;
  is_running: boolean;
  last_run: SyncRun | null;
  can_sync_now: boolean;
};

/** FolderProgress is the small, frequently refreshed subset of a settings row. */
export type FolderProgress = {
  mailbox_id: number;
  message_count: number;
  unread_count: number;
  last_uid: number;
  remote_message_count: number;
  remote_unread_count: number;
  remote_uid_next: number;
  sync_percent: number;
  local_message_count: number;
  cached_message_count: number;
  local_sync_percent: number;
  search_indexed_count?: number;
  search_index_total?: number;
  search_index_percent?: number;
  search_index_purged: boolean;
  search_index_state_known: boolean;
  is_running: boolean;
};

/** StorageIndexBreakdown describes the files a Bleve index occupies. It is
 * absent on any other backend, which keeps its index in the database and has no
 * files to break down. */
export type StorageIndexBreakdown = {
  FileCount?: number;
  ZapCount?: number;
  ZapBytes?: number;
  LargestZapPath?: string;
  LargestZapBytes?: number;
  RootBytes?: number;
  OtherBytes?: number;
};

/** StorageStats is one user's storage usage.
 *
 * `SearchBackend` names where the full-text index lives ("bleve" or
 * "postgres"). It is what lets the page report a size of zero as "nothing
 * indexed yet" rather than as a measurement of the wrong place: only the Bleve
 * backend has a directory on the volume to walk.
 *
 * `IndexMessageCount` of `FullTextSearchMessageCount` is the coverage — how
 * many of the messages in search-visible folders the index actually holds — and
 * `FoldersNeedingRebuild` is how many folders are waiting for the rebuild that
 * would close a gap between them. */
export type StorageStats = {
  MessageHeaderCount?: number;
  /** Bytes this tenant's mail rows occupy in PostgreSQL, when they could be measured. */
  DatabaseBytes?: number;
  DatabaseMeasured?: boolean;
  SearchBackend?: string;
  IndexPath?: string;
  IndexBytes?: number;
  IndexPresent?: boolean;
  IndexMessageCount?: number;
  FullTextSearchMessageCount?: number;
  FoldersNeedingRebuild?: number;
  /** Folders whose documents were purged and are waiting for a rebuild. */
  FoldersPurged?: number;
  /** Folders included in search that no sync ever fills, with what they last
   * reported holding on the server and a few of their names. This gap is
   * outside the coverage figures above and has to be: those compare the index
   * against the messages table, and mail that was never fetched is in neither.
   * The answer is a folder setting, not a rebuild. */
  UnsyncedSearchFolders?: number;
  UnsyncedSearchMessages?: number;
  UnsyncedSearchFolderNames?: string[];
  /** True when both sides of the index/mail comparison were actually read. */
  SearchCoverageMeasured?: boolean;
  FuzzyAvailable?: boolean;
  IndexBreakdown?: StorageIndexBreakdown;
  BlobPath?: string;
  BlobBytes?: number;
  MessageBodyCount?: number;
  TotalBytes?: number;
  Error?: string;
};

/**
 * MailCategorySummary is one category sidebar entry. The name is the server's
 * stored category and doubles as the view name in /mail/<name>.
 */
export type MailCategorySummary = {
  name: string;
  label: string;
  icon: string;
  total: number;
  unread: number;
};

/** ActivityWorker is one piece of background work the runner has in progress. */
export type ActivityWorker = {
  /** The runner's reservation key, which is what a cancel names. */
  key: string;
  /** The runner's own name for the work, unchanged, so a report matches the log. */
  kind: string;
  label: string;
  phase: string;
  account_id: number;
  mailbox: string;
  started_at: string;
  cancellable: boolean;
  /** Queued behind something else rather than running. */
  waiting: boolean;
};

/** ActivityService is one thing that syncs on its own schedule, outside the mailbox runner. */
export type ActivityService = {
  kind: string;
  label: string;
  account: string;
  connection_id: number;
  status: string;
  status_detail: string;
  last_sync_at: string;
  last_success_at: string;
  item_count: number;
  ever_synced: boolean;
};

/** Activity is everything running in the background for the signed-in user. */
export type Activity = {
  sync_runs: SyncRun[];
  workers: ActivityWorker[];
  services: ActivityService[];
  /** Messages still waiting to be filed into a category. */
  categories_pending: number;
  /** Search-visible folders whose index coverage nothing has verified. Not work
   * in flight — a count of what a rebuild still has to close. */
  search_index_pending?: number;
};

/** Bootstrap is the first API payload that establishes auth, chrome, CSRF, and plugin state. */
export type Bootstrap = {
  users_exist: boolean;
  csrf: string;
  user: User | null;
  mailboxes: Mailbox[];
  latest_sync_run?: SyncRun | null;
  active_sync_runs?: SyncRun[];
  /** The newest move that ended leaving messages behind, if there is one. */
  unfinished_move_run?: SyncRun | null;
  sync_running?: boolean;
  mail_generation?: number;
  swipe_preferences?: SwipePreferences;
  /**
   * The folder the Archive action files into per account, after an identity's
   * own choice has overridden the stored swipe mapping.
   */
  effective_archive_mailboxes?: AccountMailboxChoice[];
  /** The category sidebar entries with the counts each of their lists holds. */
  mail_categories?: MailCategorySummary[];
  /** How many messages still have to be read before the categories are complete. */
  mail_categories_pending?: number;
  account_needs_password?: boolean;
  account_notice?: string;
  database_unavailable?: boolean;
  enabled_plugins?: string[];
  auth_providers?: AuthProvider[];
  available_themes?: ThemeDefinition[];
  frontend_plugins?: FrontendPluginDefinition[];
  server_started_at?: string;
  server_uptime_seconds?: number;
  build_version?: string;
  build_date?: string;
  build_label?: string;
  build_commit?: string;
  public_site_url?: string;
};

export type AuthProvider = {
  id: string;
  name: string;
  login_url: string;
};

/** ChromeEvent is the SSE payload used to refresh folders and sync status. */
export type ChromeEvent = {
  mailboxes: Mailbox[];
  latest_sync_run: SyncRun | null;
  active_sync_runs: SyncRun[];
  unfinished_move_run?: SyncRun | null;
  sync_running: boolean;
  mail_generation: number;
  swipe_preferences?: SwipePreferences;
  effective_archive_mailboxes?: AccountMailboxChoice[];
  mail_categories?: MailCategorySummary[];
  mail_categories_pending?: number;
  server_started_at?: string;
  server_uptime_seconds?: number;
  build_version?: string;
  build_date?: string;
  build_label?: string;
  build_commit?: string;
  public_site_url?: string;
};

/** ComposeExistingAttachment is an already-stored attachment that compose can reuse without a new upload. */
export type ComposeExistingAttachment = Pick<Attachment, "id" | "filename" | "content_type" | "size" | "download_url">;

/** ComposeAttachmentUpload couples a File with metadata sent in multipart compose requests. */
export type ComposeAttachmentUpload = {
  field: string;
  filename: string;
  content_type: string;
  content_id: string;
  inline: boolean;
  size: number;
  file: File;
};

/** ComposeForm is the editable compose/reply/forward payload exchanged with the API. */
export type IdentityPGPPrivateKey = {
  id?: number;
  identity_id: number;
  label: string;
  fingerprint: string;
  key_id: string;
  user_ids: string;
  public_key_armored: string;
  private_key_armored?: string;
  private_key_storage?: "server" | "browser" | string;
  revocation_certificate?: string;
  is_active_signing: boolean;
  is_active_encryption: boolean;
  is_decrypt_only: boolean;
  created_at?: string;
  updated_at?: string;
};

export type ComposeForm = {
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  body: string;
  body_html: string;
  draft_message_id: number;
  in_reply_to_id: number;
  from_identity_id: number;
  available_attachments?: ComposeExistingAttachment[];
  include_attachment_ids?: number[];
  forward_attachment_message_id?: number;
  forward_attachment?: ComposeExistingAttachment;
  pgp_encrypted?: boolean;
  pgp_signed?: boolean;
  pgp_mime?: boolean;
  pgp_signature?: string;
  attach_public_key?: boolean;
  /** File the message this replies to once the reply is away. Send and archive sets it. */
  archive_after_send?: boolean;
};

/** Account is the IMAP account settings shape used by the settings page. */
export type Account = {
  id: number;
  email: string;
  label: string;
  host: string;
  port: number;
  username: string;
  use_tls: boolean;
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_use_tls: boolean;
  smtp_same_as_imap: boolean;
  mailbox: string;
  sync_interval_minutes: number;
  /** "password" or "google_oauth". */
  auth_type: string;
  google_connection_id: number;
  /** Address of the connected Google account, when this signs in with Google. */
  google_email?: string;
  /** Calendar date (YYYY-MM-DD); empty means mirror everything the server has. */
  sync_start_at?: string;
};

export type AccountPurgeEstimate = {
  account_id: number;
  account_name: string;
  account_email: string;
  mailbox_count: number;
  message_count: number;
  blob_count: number;
  blob_bytes: number;
  search_index_count: number;
};

/** SMTPAccount is the outgoing server settings shape used by the settings page. */
export type SMTPAccount = {
  id: number;
  label: string;
  host: string;
  port: number;
  username: string;
  use_tls: boolean;
  /** "password" or "google_oauth". */
  auth_type: string;
  google_connection_id: number;
};

/** MailIdentity is the settings shape for a Me-contact-backed outgoing identity. */
export type MailIdentity = {
  id: number;
  contact_id: number;
  contact_email_id: number;
  smtp_account_id: number;
  imap_account_id: number;
  sent_mailbox_id: number;
  drafts_mailbox_id: number;
  archive_mailbox_id: number;
  email: string;
  display_name: string;
  signature: string;
  autocrypt_enabled: boolean;
  is_primary: boolean;
};

/** DatabaseStatus is the connection and size card for the one database. */
export type DatabaseStatus = {
  /** role@host/database — never the password. */
  target: string;
  reachable: boolean;
  error?: string;
  server_version?: string;
  bytes: number;
  latency_millis: number;
  in_recovery: boolean;
  connections: number;
  pool_max_conns: number;
};

/** VolumeStatus is the data volume, which still holds blobs and search
 * indexes even though the relational data no longer does. */
export type VolumeStatus = {
  data_dir: string;
  free_bytes: number;
  total_bytes: number;
  blob_bytes: number;
  index_bytes: number;
  /** Under users/ but neither a blob nor a live index — a quarantined index,
   * most often. Without it the figures fail to add up to the used space. */
  other_bytes: number;
  /** When the walk behind the three figures above ran, or 0 if none has
   * finished yet: measuring costs one stat per stored file, so it runs on a
   * timer rather than per request. */
  measured_at_unix: number;
};

/** SearchIndexTenant is one tenant's search index on the data volume.
 *
 * `present` is false for a user who has never been indexed and for one whose
 * index was quarantined after it turned out to be unreadable.
 * `folders_needing_rebuild` counts search-visible folders whose coverage
 * nothing has verified, which is exactly what a rebuild acts on. */
export type SearchIndexTenant = {
  user_id: number;
  email: string;
  name?: string;
  present: boolean;
  bytes: number;
  folders_needing_rebuild: number;
  /** Why this tenant's numbers are missing, if they are. One unreadable
   * tenant must not blank the card for the others. */
  error?: string;
};

/** SearchRebuildBlock is one mail server a rebuild could not start on, and the
 * reason the sync runner gave. The reason distinguishes work that ends by
 * itself - a folder sync, another rebuild - from a recovery that refuses every
 * rebuild until it clears, which is the difference between waiting a minute and
 * waiting for something else to be fixed. */
export type SearchRebuildBlock = {
  account: string;
  reason: string;
};

/** SearchIndexReport is the search index card payload. After a rebuild it names
 * the tenant, how many per-account runs started, and which accounts could not
 * start one and why. */
export type SearchIndexReport = {
  tenants: SearchIndexTenant[];
  rebuilt?: number;
  started_runs?: number;
  busy_accounts?: number;
  blocked?: SearchRebuildBlock[];
};

/** DatabaseOverview is the admin database page payload.
 *
 * `search_backend` names where the full-text index lives ("bleve" or
 * "postgres"). The volume's `index_bytes` only describes an index on the Bleve
 * backend; on the other one the index is rows in the database. */
export type DatabaseOverview = {
  database: DatabaseStatus;
  volume: VolumeStatus;
  search_backend?: string;
};


/** ServerLogLine is one captured line of the process log tail. */
export type ServerLogLine = {
  time: string;
  message: string;
  error: boolean;
};

/** SMTPLogLine is one recorded utterance of an SMTP conversation. `direction`
 * is "client" for what Rolltop sent, "server" for the reply, and "note" for
 * Rolltop's own commentary — the dial, the TLS upgrade, the message body that
 * is deliberately not transcribed. */
export type SMTPLogLine = {
  time: string;
  direction: "client" | "server" | "note";
  text: string;
};

/** SMTPLogSession is one send or connection test with the conversation it
 * produced. It never carries credentials or message content: the AUTH exchange
 * is redacted and the payload appears only as a byte count. */
export type SMTPLogSession = {
  id: number;
  account_id: number;
  kind: "send" | "test";
  host: string;
  port: number;
  username: string;
  from: string;
  started_at: string;
  ended_at: string;
  error: string;
  truncated: boolean;
  lines: SMTPLogLine[];
};

/** SMTPTestResult answers the settings page's connection test. A refused login
 * is a successful request with `ok: false`: the transcript is the payload. */
export type SMTPTestResult = {
  ok: boolean;
  error?: string;
  session?: SMTPLogSession;
};

/** MessageShipmentSummary is the parcel one message is about, as the message
 * payload carries it. count says how many the message named; the other fields
 * describe the first of them. */
export type MessageShipmentSummary = {
  id: number;
  carrier: string;
  carrier_label: string;
  tracking_number: string;
  tracking_url: string;
  expected_date: string;
  status: "announced" | "out_for_delivery" | "delivered";
  count: number;
};

/** ShipmentMessage is one mail that named a parcel, as the parcel list links
 * back to it. */
export type ShipmentMessage = {
  id: number;
  mailbox_id: number;
  subject: string;
  from: string;
  date: string;
};

/** Shipment is one parcel the mail announced, with every message that mentioned
 * it. carrier is empty for a number a message labelled without saying whose it
 * was; carrier_label is what to show either way. */
export type Shipment = {
  id: number;
  carrier: string;
  carrier_label: string;
  tracking_number: string;
  /** tracking_url is empty for a carrier whose page needs more than the number. */
  tracking_url: string;
  /** expected_date is "YYYY-MM-DD", or empty for a parcel nobody has dated. */
  expected_date: string;
  window_start: string;
  window_end: string;
  /** status is what the parcel counts as, the reader's own answer included.
   * manual_status is that answer alone: "" when the mail is the only source. */
  status: "announced" | "out_for_delivery" | "delivered";
  manual_status: "" | "delivered" | "dismissed";
  messages: ShipmentMessage[];
};

/** InvoiceStatus is what a bill counts as. "open" is money the reader still has
 * to send; "paid" covers everything with nothing left to do -- settled, taken
 * by direct debit, charged to a card, or a credit note. */
export type InvoiceStatus = "open" | "paid";

/** InvoiceSettlement is who moves the money, which is a different question from
 * whether it has moved. It is what tells an invoice the reader must act on from
 * one that settles itself. */
export type InvoiceSettlement = "" | "transfer" | "direct_debit" | "card" | "wallet";

/** MessageInvoiceSummary is the bill one message is about, as the message
 * payload carries it. */
export type MessageInvoiceSummary = {
  id: number;
  issuer: string;
  number: string;
  due_date: string;
  amount: string;
  currency: string;
  status: InvoiceStatus;
  settlement: InvoiceSettlement;
  /** dunning_level is 0 for an ordinary invoice, 1 for a payment reminder,
   * 2 for an overdue notice and 3 for a last warning. */
  dunning_level: number;
};

/** InvoiceMessage is one mail that named a bill, as the invoice list links back
 * to it. */
export type InvoiceMessage = {
  id: number;
  mailbox_id: number;
  subject: string;
  from: string;
  date: string;
};

/** Invoice is one bill the mail says is owed, with every message that mentioned
 * it. */
export type Invoice = {
  id: number;
  /** issuer is the sender's domain, which is what an invoice number is unique
   * within -- unlike a tracking number, which is unique in the world. */
  issuer: string;
  /** number is empty for a bill whose document never printed one. */
  number: string;
  /** due_date is the day it counts as due, the reader's own entry included, or
   * empty for a bill nobody dated. manual_due_date is that entry alone. */
  due_date: string;
  manual_due_date: string;
  /** amount is normalized to "1234.56", or empty when no total was readable. */
  amount: string;
  currency: string;
  /** status is what the bill counts as, the reader's own answer included.
   * manual_status is that answer alone: "" when the mail is the only source. */
  status: InvoiceStatus;
  manual_status: "" | "paid" | "dismissed";
  settlement: InvoiceSettlement;
  dunning_level: number;
  messages: InvoiceMessage[];
};

/** CalendarSummary is one subscribed Google calendar. */
export type CalendarSummary = {
  id: number;
  /** connection_id and connection_email name the Google account it came from,
   * which is what tells two identically named calendars apart. */
  connection_id: number;
  connection_email: string;
  name: string;
  description: string;
  time_zone: string;
  /** color is Google's own background colour for the calendar, e.g. "#9fe1e7". */
  color: string;
  access_role: string;
  can_write: boolean;
  is_primary: boolean;
  selected: boolean;
  /** synced_from is the oldest point the mirror covers. An empty week before it
   * means "not synced", not "nothing scheduled". */
  synced_from: string;
  last_sync_at: string;
  status: string;
  status_detail: string;
};

/** CalendarAttendee is one invitee of an event. */
export type CalendarAttendee = {
  email: string;
  name: string;
  response: string;
  optional: boolean;
  organizer: boolean;
  /** self marks the connected account, whose answer is the only one this app
   * may change. */
  self: boolean;
  resource: boolean;
};

/** CalendarEvent is one occurrence. Recurring series arrive already expanded,
 * so there is no recurrence rule to evaluate here. */
export type CalendarEvent = {
  id: number;
  calendar_id: number;
  summary: string;
  description: string;
  location: string;
  status: string;
  /** start_at and end_at are RFC 3339. For an all-day event they are the UTC
   * midnights of Google's plain dates and must be read with the UTC getters. */
  start_at: string;
  end_at: string;
  all_day: boolean;
  time_zone: string;
  recurring_event_id: string;
  organizer_email: string;
  organizer_name: string;
  attendees: CalendarAttendee[];
  my_response: string;
  html_link: string;
};

/** CalendarEventInput is what the event dialog submits. */
export type CalendarEventInput = {
  calendar_id: number;
  summary: string;
  description: string;
  location: string;
  start_at: string;
  end_at: string;
  all_day: boolean;
  time_zone: string;
  attendees: { email: string; name: string; optional: boolean }[];
};

/** DuplicateAccountSummary is one account's share of the hidden duplicate copies. */
export type DuplicateAccountSummary = {
  account_id: number;
  email: string;
  label: string;
  hidden: number;
};

/**
 * DuplicateCopyReport lists the copies an aggregating account fetched of mail
 * another account was addressed in. They are hidden from every list; the report
 * is what makes them visible as a number.
 */
export type DuplicateCopyReport = {
  ok: boolean;
  hidden: number;
  accounts: DuplicateAccountSummary[];
};

/**
 * DuplicateScanResult is one detection pass. Beyond what it changed it reports
 * what it decided not to change: `outcomes` counts the cross-account groups by
 * the reason they were left visible, and `within_account_messages` counts the
 * messages a single account holds twice, which detection never judges at all.
 * The latter is present only on the pass that finished the scan, because it
 * answers for the whole mailbox rather than for one page of it.
 * A scan that hides nothing is the normal steady state, and these are what say
 * so rather than leaving it looking like detection failed.
 */
export type DuplicateScanResult = DuplicateCopyReport & {
  groups: number;
  newly_hidden: number;
  revealed: number;
  truncated: boolean;
  next: string;
  outcomes?: Record<string, number>;
  within_account_messages?: number;
};
