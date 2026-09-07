import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Contact } from "../../types";

const contactsMock = vi.fn();
vi.mock("../../api", () => ({ api: { contactByEmail: (...args: unknown[]) => contactsMock(...args) } }));

import { AddressLink } from "./AddressCard";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

function savedContact(): Contact {
  return {
    id: 7,
    name_prefix: "",
    given_name: "Ann",
    additional_name: "",
    family_name: "Lee",
    name_suffix: "",
    display_name: "Ann Lee",
    nickname: "",
    organization: "Example Ltd",
    department: "",
    job_title: "Buyer",
    birthday: "",
    notes: "",
    categories: "",
    is_me: false,
    is_primary: false,
    source: "local",
    google_connection_id: 0,
    emails: [
      { label: "Work", email: "Ann@Example.test", is_primary: true },
      { label: "Home", email: "ann@home.test", is_primary: false }
    ],
    phones: [{ label: "Mobile", number: "+49 170 000", is_primary: true }],
    addresses: [],
    urls: [],
    icon_url: ""
  };
}

let container: HTMLDivElement;
let root: Root;
const actions = { openCompose: vi.fn(), navigate: vi.fn(), addToast: vi.fn() };
const rowClick = vi.fn();

async function mount(name: string, email: string, active = true) {
  await act(async () => {
    root.render(
      <div onClick={rowClick}>
        <AddressLink address={{ name, email }} actions={actions} active={active}>
          {name || email}
        </AddressLink>
      </div>
    );
  });
}

function link(): HTMLButtonElement {
  return container.querySelector("button.address-link") as HTMLButtonElement;
}

function card(): HTMLElement | null {
  return document.querySelector(".address-card");
}

function cardButton(label: string): HTMLButtonElement {
  const found = Array.from(card()?.querySelectorAll("button") || []).find((button) => button.textContent?.trim() === label);
  if (!found) throw new Error(`no button ${label}`);
  return found;
}

async function open() {
  await act(async () => {
    link().click();
  });
  await act(async () => {
    await Promise.resolve();
  });
}

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
  contactsMock.mockReset();
  actions.openCompose.mockReset();
  actions.navigate.mockReset();
  actions.addToast.mockReset();
  rowClick.mockReset();
});

afterEach(async () => {
  await act(async () => {
    root.unmount();
  });
  container.remove();
});

describe("AddressLink", () => {
  it("opens a card for the address without collapsing the row around it", async () => {
    contactsMock.mockResolvedValue({ contacts: [] });
    await mount("Ann", "ann@example.test");
    expect(card()).toBeNull();
    await open();
    expect(rowClick).not.toHaveBeenCalled();
    expect(contactsMock).toHaveBeenCalledWith("ann@example.test");
    expect(card()?.textContent).toContain("ann@example.test");
    expect(card()?.textContent).toContain("Not in your contacts");
    expect(cardButton("Add contact")).toBeTruthy();
  });

  it("shows the saved contact and opens it in the address book", async () => {
    contactsMock.mockResolvedValue({ contacts: [savedContact()] });
    await mount("A. Lee", "ann@example.test");
    await open();
    const text = card()?.textContent || "";
    expect(text).toContain("Ann Lee");
    expect(text).toContain("Buyer · Example Ltd");
    expect(text).toContain("ann@home.test");
    expect(text).toContain("+49 170 000");
    expect(text).toContain("In your contacts");
    await act(async () => {
      cardButton("Open contact").click();
    });
    expect(actions.navigate).toHaveBeenCalledWith("/contacts?contact=7");
    expect(card()).toBeNull();
  });

  it("hands a new contact and a new message the name the header carried", async () => {
    contactsMock.mockResolvedValue({ contacts: [] });
    await mount("Ann Lee", "ann@example.test");
    await open();
    await act(async () => {
      cardButton("New message").click();
    });
    expect(actions.openCompose).toHaveBeenCalledWith("to=Ann%20Lee%20%3Cann%40example.test%3E");
    await open();
    await act(async () => {
      cardButton("Add contact").click();
    });
    expect(actions.navigate).toHaveBeenCalledWith("/contacts?new=1&name=Ann+Lee&email=ann%40example.test");
  });

  it("copies the bare address and says so", async () => {
    contactsMock.mockResolvedValue({ contacts: [] });
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    await mount("Ann Lee", "ann@example.test");
    await open();
    await act(async () => {
      cardButton("Copy address").click();
    });
    expect(writeText).toHaveBeenCalledWith("ann@example.test");
    expect(actions.addToast).toHaveBeenCalledWith("Address copied.", undefined);
    expect(card()).toBeNull();
  });

  it("closes on Escape and on a click elsewhere, and survives an address book that fails", async () => {
    contactsMock.mockRejectedValue(new Error("offline"));
    await mount("", "ann@example.test");
    await open();
    expect(card()?.textContent).toContain("Not in your contacts");
    await act(async () => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(card()).toBeNull();
    await open();
    await act(async () => {
      document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    });
    expect(card()).toBeNull();
  });

  it("does not save a nameless sender under its own address", async () => {
    contactsMock.mockResolvedValue({ contacts: [] });
    await mount("bob@example.test", "bob@example.test");
    await open();
    await act(async () => {
      cardButton("New message").click();
    });
    expect(actions.openCompose).toHaveBeenCalledWith("to=bob%40example.test");
    await open();
    await act(async () => {
      cardButton("Add contact").click();
    });
    expect(actions.navigate).toHaveBeenCalledWith("/contacts?new=1&email=bob%40example.test");
  });

  it("renders plain text when the entry has no address or the link is inactive", async () => {
    await mount("undisclosed-recipients:", "");
    expect(link()).toBeNull();
    expect(container.textContent).toContain("undisclosed-recipients:");
    await mount("Ann", "ann@example.test", false);
    expect(link()).toBeNull();
    await act(async () => {
      (container.querySelector("span") as HTMLElement).click();
    });
    expect(rowClick).toHaveBeenCalled();
  });
});
