-- The "star" rule action was removed (the Star field dropped from Actions). A
-- rule whose only action was starring therefore deserializes to no effective
-- action: it still matches mail and still records evaluations, but does nothing
-- to the message, all while continuing to show as enabled. Disable exactly those
-- rules so the now-inert state is visible in the UI instead of a rule that looks
-- active but silently acts on nothing.
--
-- A rule that also moved or forwarded keeps that action (the ignored "star" key
-- in its JSON is harmless) and stays enabled. Rules created after the removal
-- have no "star" key at all and never match here. actions_json is always machine
-- written valid JSON with a '{}' default, so the jsonb cast is safe.
UPDATE plugin_mail_filter_rules
SET enabled = 0, updated_at = EXTRACT(EPOCH FROM now())::bigint
WHERE enabled = 1
  AND actions_json::jsonb ->> 'star' = 'true'
  AND COALESCE(actions_json::jsonb ->> 'forward_to', '') = ''
  AND COALESCE(actions_json::jsonb ->> 'move_role', '') = ''
  AND COALESCE((actions_json::jsonb ->> 'move_mailbox_id'), '0') = '0';
