package main

import (
	"context"
	"database/sql"
	"time"

	"rolltop/backend/plugins"
	"rolltop/plugins/one_click_unsubscribe/history"
)

// RolltopPlugin is the symbol loaded by plugin.Open.
func RolltopPlugin() plugins.BackendPlugin {
	return plugins.NoopBackendPlugin{PluginID: plugins.OneClickUnsubscribe}
}

type oneClickUnsubscribeHook struct{}

// Compile-time proof the hook satisfies the host interface, so a missing method
// is a build error rather than a silent fail-open at the runtime type assertion.
var _ plugins.OneClickUnsubscribeHook = oneClickUnsubscribeHook{}

func (oneClickUnsubscribeHook) LatestOneClickSend(ctx context.Context, db *sql.DB, userID, messageID int64, target string, since time.Time) (plugins.OneClickUnsubscribeSend, error) {
	send, err := history.LatestSend(ctx, db, userID, messageID, target, since)
	return plugins.OneClickUnsubscribeSend{SentAt: send.SentAt}, err
}

func (oneClickUnsubscribeHook) RecordOneClickSend(ctx context.Context, db *sql.DB, userID, messageID int64, sender, target string, sentAt time.Time) error {
	return history.RecordSend(ctx, db, userID, messageID, sender, target, sentAt)
}
