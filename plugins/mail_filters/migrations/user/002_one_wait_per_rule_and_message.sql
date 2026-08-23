-- One rule may wait on one message once. Two waiting rows run the rule's action
-- twice, and for a rule that moves mail to Trash the second run acts on a
-- message that is already there. Any duplicates a release without this
-- constraint left behind are collapsed onto the oldest row before the index is
-- built, because that is the row the scheduler would have reached first.
DELETE FROM plugin_mail_filter_evaluations a
USING plugin_mail_filter_evaluations b
WHERE a.status = 'scheduled' AND b.status = 'scheduled'
  AND a.user_id = b.user_id AND a.rule_id = b.rule_id AND a.message_id = b.message_id
  AND a.id > b.id;

CREATE UNIQUE INDEX IF NOT EXISTS idx_plugin_mail_filter_evaluations_one_wait
  ON plugin_mail_filter_evaluations(user_id, rule_id, message_id)
  WHERE status = 'scheduled';
