// File overview: Central Phosphor icon adapter. App code keeps semantic icon names while this file
// maps them to concrete icon components, aliases, and default weights.

import {
  AndroidLogo,
  Archive,
  ArrowBendUpLeft,
  ArrowBendUpRight,
  ArrowLeft,
  ArrowRight,
  ArrowsClockwise,
  AirplaneTilt,
  Bank,
  Bell,
  BookmarkSimple,
  Briefcase,
  Buildings,
  CalendarBlank,
  Camera,
  CaretDown,
  Check,
  CaretLeft,
  CaretRight,
  ChartBar,
  ChatsCircle,
  Clock,
  CreditCard,
  DotsThreeVertical,
  DownloadSimple,
  EnvelopeOpen,
  EnvelopeSimple,
  FileText,
  Flame,
  Folder,
  Funnel,
  GearSix,
  GraduationCap,
  Heart,
  House,
  Image,
  Key,
  List,
  ListBullets,
  ListNumbers,
  LinkSimple,
  Lock,
  LockOpen,
  MagnifyingGlass,
  Mailbox as MailboxIcon,
  Minus,
  Newspaper,
  NotePencil,
  Paperclip,
  PaperPlaneTilt,
  PencilSimple,
  Plus,
  Quotes,
  Ranking,
  Receipt,
  SealWarning,
  WarningCircle,
  SelectionAll,
  Shield,
  ShieldWarning,
  ShoppingBag,
  SidebarSimple,
  SignOut,
  Signature,
  SortAscending,
  SortDescending,
  Star,
  Tag,
  TextAa,
  Trash,
  Tray,
  User,
  Users,
  X
} from "@phosphor-icons/react";
import type { Icon as PhosphorIcon, IconWeight } from "@phosphor-icons/react";

// Keep this map semantic. Folder configuration and older UI call sites still use
// Material-ish names, while this adapter decides which Phosphor glyph to render.
const iconMap: Record<string, PhosphorIcon> = {
  add: Plus,
  android: AndroidLogo,
  archive: Archive,
  bank: Bank,
  bookmark: BookmarkSimple,
  briefcase: Briefcase,
  building: Buildings,
  calendar: CalendarBlank,
  arrow_back: ArrowLeft,
  arrow_forward: ArrowRight,
  attach_file: Paperclip,
  camera: Camera,
  chart: ChartBar,
  chevron_left: CaretLeft,
  chevron_right: CaretRight,
  check: Check,
  clock: Clock,
  close: X,
  credit_card: CreditCard,
  delete: Trash,
  draft: NotePencil,
  download: DownloadSimple,
  edit: PencilSimple,
  error: WarningCircle,
  expand_more: CaretDown,
  file_text: FileText,
  filter: Funnel,
  flame: Flame,
  folder: Folder,
  forum: ChatsCircle,
  format_color_text: TextAa,
  format_list_bulleted: ListBullets,
  format_list_numbered: ListNumbers,
  format_quote: Quotes,
  forward: ArrowBendUpRight,
  group: Users,
  heart: Heart,
  home: House,
  image: Image,
  inbox: Tray,
  key: Key,
  label: Tag,
  link: LinkSimple,
  lock: Lock,
  lock_open: LockOpen,
  logout: SignOut,
  menu: List,
  sidebar: SidebarSimple,
  mail: EnvelopeSimple,
  mail_open: EnvelopeOpen,
  mailbox: MailboxIcon,
  rolltop: MailboxIcon,
  minimize: Minus,
  more_vert: DotsThreeVertical,
  newspaper: Newspaper,
  person: User,
  notifications: Bell,
  ranking: Ranking,
  receipt: Receipt,
  report: SealWarning,
  reply: ArrowBendUpLeft,
  reply_all: Users,
  search: MagnifyingGlass,
  select_all: SelectionAll,
  send: PaperPlaneTilt,
  settings: GearSix,
  shield: Shield,
  shield_warning: ShieldWarning,
  school: GraduationCap,
  shopping_bag: ShoppingBag,
  signature: Signature,
  sort_ascending: SortAscending,
  sort_descending: SortDescending,
  star: Star,
  sync: ArrowsClockwise,
  travel: AirplaneTilt
};

const iconAliases: Record<string, string> = {
  drafts: "draft",
  rule: "filter",
  sent: "send",
  spam: "report",
  trash: "delete"
};

const iconWeights: Partial<Record<string, IconWeight>> = {
  rolltop: "duotone",
  report: "duotone",
  sync: "duotone"
};


export function LogoMark({ className = "brand-logo" }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 66.066406 69.11528" aria-hidden="true" focusable="false">
      <g transform="translate(509.99438 175.90039)">
        <path fill="#f7faf8" d="m-446.67797-106.79728-.008-32.18705v-.002l-.006-3.89258c0-16.75746-13.51402-30.27148-30.27148-30.27148h.00056c-16.75746 0-30.27149 13.51402-30.27149 30.27148l-.006 3.89258v.002l-.004 32.18705" />
        <path fill="none" stroke="#c46b44" strokeWidth="5.5" strokeLinecap="round" d="m-446.67797-109.53511-.008-29.44922v-.002l-.006-3.89258c0-16.75746-13.51402-30.27148-30.27148-30.27148h.00056c-16.75746 0-30.27149 13.51402-30.27149 30.27148l-.006 3.89258v.002l-.004 29.44922" />
        <path fill="#151f2e" d="m-454.95974-139.81893-15.33653 11.85302c-3.75097 2.89903-9.93295 2.89799-13.68392-.001l-14.98513-11.58172-.00052.56844-.004 32.19493h44.0154l-.004-32.19286z" />
        <path fill="#c46b44" d="m-476.96253-164.87683c-11.12016.00024-20.09174 7.892-21.73045 18.48931l19.27324 14.89573c1.30333 1.00731 3.25815 1.00731 4.56148 0l19.58588-15.13706c-1.73637-10.47584-10.65408-18.24774-21.68963-18.24798z" />
      </g>
    </svg>
  );
}

/** Resolve a semantic Rolltop icon name to a Phosphor component and weight. */
export function Icon({ name, weight }: { name: string; weight?: IconWeight }) {
  const normalized = name.trim().toLowerCase().replaceAll("-", "_");
  const key = iconAliases[normalized] || normalized;
  const Component = iconMap[key] || Folder;
  return <Component className="icon" aria-hidden="true" focusable="false" weight={weight || iconWeights[key] || "regular"} />;
}
