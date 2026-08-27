// File overview: The retention settings panel. It holds the two halves of one
// policy: how long each category keeps mail before it is thrown away, and how long
// the Trash keeps what was thrown away before the server is told to delete it.

import { useCallback, useEffect, useState } from "react";
import type { FormEvent } from "react";
import { api } from "../../api";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import type { RelativeCutoff } from "../../lib/retentionCutoff";
import {
  cutoffInstant,
  dateInputValue,
  displayCutoff,
  relativeCutoffLabel,
  retentionUnitChoices
} from "../../lib/retentionCutoff";
import type { Toast } from "../../appTypes";
import type { CategoryRetention, MailCategorySummary, RetentionMode, RetentionSettings, RetentionUnit } from "../../types";

/**
 * categoryDraft is one category's row while it is being edited. Both spellings
 * of the cutoff are kept side by side rather than one being derived from the
 * other, so switching between them and back does not lose what was typed.
 */
type categoryDraft = {
  category: string;
  label: string;
  icon: string;
  mode: RetentionMode;
  relative: RelativeCutoff;
  day: string;
};

/** defaultRelative is what an untouched relative row starts at. */
const defaultRelative: RelativeCutoff = { count: 1, unit: "years" };

function draftsFor(categories: readonly MailCategorySummary[], settings: RetentionSettings): categoryDraft[] {
  const rules = new Map(settings.categories.map((rule) => [rule.category, rule]));
  return categories.map((category) => {
    const rule = rules.get(category.name);
    const relative: RelativeCutoff = rule && rule.mode === "relative" && rule.unit
      ? { count: rule.count, unit: rule.unit }
      : defaultRelative;
    const day = rule && rule.mode === "fixed" && rule.before
      ? dateInputValue(new Date(rule.before))
      : "";
    return {
      category: category.name,
      label: category.label || category.name,
      icon: category.icon || "label",
      mode: rule ? rule.mode : "off",
      relative,
      day
    };
  });
}

function ruleFor(draft: categoryDraft): CategoryRetention {
  if (draft.mode === "relative") {
    return {
      category: draft.category, mode: "relative",
      count: draft.relative.count, unit: draft.relative.unit, before: ""
    };
  }
  if (draft.mode === "fixed") {
    return {
      category: draft.category, mode: "fixed",
      count: 0, unit: "", before: draft.day ? cutoffInstant(draft.day) : ""
    };
  }
  return { category: draft.category, mode: "off", count: 0, unit: "", before: "" };
}

/** describeDraft says in one line what a row currently promises to do. */
function describeDraft(draft: categoryDraft): string {
  if (draft.mode === "relative") return `Deleted when ${relativeCutoffLabel(draft.relative)}`;
  if (draft.mode === "fixed") {
    return draft.day ? `Deleted when dated before ${displayCutoff(draft.day)}` : "Choose the date to delete before";
  }
  return "Kept until you delete it";
}

/**
 * RetentionSettingsPanel edits the whole policy at once, because the two halves
 * only make sense together: a category rule throws mail into the Trash, and the
 * Trash rule is what eventually takes it off the server.
 */
export function RetentionSettingsPanel({
  csrf,
  categories,
  addToast
}: {
  csrf: string;
  categories: readonly MailCategorySummary[];
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [trashEnabled, setTrashEnabled] = useState(true);
  const [trashDays, setTrashDays] = useState(30);
  const [drafts, setDrafts] = useState<categoryDraft[]>([]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.retention();
      setTrashEnabled(data.retention.trash_enabled);
      setTrashDays(data.retention.trash_days || 30);
      setDrafts(draftsFor(categories, data.retention));
      setLoadError("");
    } catch (err) {
      setLoadError(messageFromError(err));
    } finally {
      setLoading(false);
    }
  }, [categories]);

  useEffect(() => {
    void load();
  }, [load]);

  function updateDraft(category: string, patch: Partial<categoryDraft>) {
    setDrafts((current) => current.map((draft) => draft.category === category ? { ...draft, ...patch } : draft));
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    if (saving) return;
    const missingDate = drafts.find((draft) => draft.mode === "fixed" && !draft.day);
    if (missingDate) {
      addToast(`Choose the date to delete ${missingDate.label} mail before.`, "error");
      return;
    }
    const missingCount = drafts.find((draft) => draft.mode === "relative" && draft.relative.count < 1);
    if (missingCount) {
      addToast(`Say how long ${missingCount.label} mail is kept for.`, "error");
      return;
    }
    if (trashEnabled && trashDays < 1) {
      addToast("Say how many days the Trash keeps mail for, or switch the automatic emptying off.", "error");
      return;
    }
    setSaving(true);
    try {
      const saved = await api.saveRetention(csrf, {
        trash_enabled: trashEnabled,
        trash_days: trashDays,
        categories: drafts.map(ruleFor)
      });
      setTrashEnabled(saved.retention.trash_enabled);
      setTrashDays(saved.retention.trash_days || 30);
      setDrafts(draftsFor(categories, saved.retention));
      addToast("Retention saved. The next pass runs shortly.");
    } catch (err) {
      addToast(`Retention could not be saved: ${messageFromError(err)}`, "error");
    } finally {
      setSaving(false);
    }
  }

  if (loading) {
    return <div className="panel retention-settings" role="status" aria-label="Loading retention settings">Loading retention.</div>;
  }
  if (loadError) {
    return (
      <div className="panel retention-settings">
        <p className="swipe-validation">{loadError}</p>
        <div className="actions"><button type="button" onClick={() => void load()}>Try again</button></div>
      </div>
    );
  }

  return (
    <form className="panel retention-settings" onSubmit={save}>
      <section className="retention-trash">
        <h3>Trash</h3>
        <p className="muted">
          Deleting mail moves it to the Trash. Emptying the Trash is the one thing Rolltop does that removes mail from
          your mail server, so it is the only step here that cannot be undone.
        </p>
        <label className="retention-toggle">
          <input
            type="checkbox"
            checked={trashEnabled}
            onChange={(event) => setTrashEnabled(event.target.checked)}
          />
          <span>Empty the Trash automatically, on every account</span>
        </label>
        <label className="retention-days">
          <span>Delete Trash mail once it has been there for</span>
          <input
            type="number"
            min={1}
            max={3650}
            value={trashDays}
            disabled={!trashEnabled}
            onChange={(event) => setTrashDays(Number(event.target.value) || 0)}
          />
          <span>days</span>
        </label>
        <small className="muted">
          The clock starts when a message arrives in the Trash, not when it was sent, so mail thrown away today keeps its
          full {trashDays > 0 ? trashDays : 30} days. Mail Rolltop has never mirrored is left alone; emptying a Trash
          folder by hand still takes all of it.
        </small>
      </section>
      <section className="retention-categories">
        <h3>Categories</h3>
        <p className="muted">
          Mail older than a category's cutoff is moved to the Trash, across every folder it is filed in. It is deleted
          for good only once the Trash rule above reaches it.
        </p>
        {categories.length === 0 ? (
          <small className="swipe-validation">No categories are available yet. They appear once mail has been sorted.</small>
        ) : (
          <div className="retention-category-list">
            {drafts.map((draft) => (
              <div className="retention-category-row" key={draft.category}>
                <div className="retention-category-label">
                  <Icon name={draft.icon} />
                  <strong>{draft.label}</strong>
                  <small className="muted">{describeDraft(draft)}</small>
                </div>
                <div className="retention-category-controls">
                  <label>
                    <span>Keep</span>
                    <select
                      value={draft.mode}
                      onChange={(event) => updateDraft(draft.category, { mode: event.target.value as RetentionMode })}
                    >
                      <option value="off">Everything</option>
                      <option value="relative">Only recent mail</option>
                      <option value="fixed">Only mail after a date</option>
                    </select>
                  </label>
                  {draft.mode === "relative" ? (
                    <label>
                      <span>Older than</span>
                      <input
                        type="number"
                        min={1}
                        max={3650}
                        aria-label={`How much older ${draft.label} mail may be`}
                        value={draft.relative.count}
                        onChange={(event) => updateDraft(draft.category, {
                          relative: { ...draft.relative, count: Number(event.target.value) || 0 }
                        })}
                      />
                      <select
                        aria-label={`Counted in, for ${draft.label}`}
                        value={draft.relative.unit}
                        onChange={(event) => updateDraft(draft.category, {
                          relative: { ...draft.relative, unit: event.target.value as RetentionUnit }
                        })}
                      >
                        {retentionUnitChoices.map((unit) => (
                          <option key={unit.value} value={unit.value}>{unit.label}</option>
                        ))}
                      </select>
                    </label>
                  ) : null}
                  {draft.mode === "fixed" ? (
                    <label>
                      <span>Before</span>
                      <input
                        type="date"
                        aria-label={`The date to delete ${draft.label} mail before`}
                        value={draft.day}
                        max={dateInputValue(new Date())}
                        onChange={(event) => updateDraft(draft.category, { day: event.target.value })}
                      />
                    </label>
                  ) : null}
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
      <div className="actions">
        <button disabled={saving}>{saving ? "Saving..." : "Save retention"}</button>
      </div>
    </form>
  );
}
