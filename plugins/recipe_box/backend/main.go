// File overview: Runtime backend plugin that turns the recipe links a reader
// mails themselves into a cookbook.
//
// The work is one background loop per process, in three steps a reader can
// watch move in the settings page:
//
//  1. scan: walk the reader's mail by row id from a cursor, keep only what one
//     of their own addresses sent to their own addresses, and record the links
//     it carries. Starting the cursor at zero is the backfill; new mail is the
//     same walk continuing, which is why there is no second path for it.
//  2. fetch: ask each recorded link's page what it is. A page that publishes a
//     schema.org Recipe becomes a recipe; anything else is kept as a preview the
//     reader can still file as a recipe by hand.
//  3. file: move every mail that carried a recipe into the account's recipe
//     folder, creating it on first use, so it is out of the Inbox and -- for a
//     folder the box created -- out of All Mail.
//
// The stored-message hook only wakes the loop. Nothing here runs inside a sync
// turn: a page fetch can take seconds, and a slow recipe site must never cost a
// mailbox its sync window.

package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/remoteimages"
	"rolltop/backend/store"
	"rolltop/plugins/recipe_box/recipe"
)

const (
	pluginID = "recipe_box"
	apiPath  = "plugins/recipe_box"
)

type recipeBoxBackend struct {
	mu     sync.Mutex
	routes []plugins.ProtectedAPIRouteHandle
	cancel context.CancelFunc
	wake   chan struct{}
	// fetcher is replaced by tests, which serve pages from loopback addresses
	// the production dialer refuses.
	fetcher *recipe.Fetcher
	// lastFetch is when each site may next be asked for a page, guarded by mu.
	lastFetch   map[string]time.Time
	hostSpacing time.Duration
}

// RolltopPlugin is the symbol loaded by plugin.Open.
func RolltopPlugin() plugins.BackendPlugin {
	return newBackend(recipe.NewFetcher(remoteimages.SafeDialContext))
}

func newBackend(fetcher *recipe.Fetcher) *recipeBoxBackend {
	return &recipeBoxBackend{wake: make(chan struct{}, 1), fetcher: fetcher, hostSpacing: sameHostSpacing}
}

func (*recipeBoxBackend) ID() string { return pluginID }

func (p *recipeBoxBackend) Start(host plugins.BackendStartHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unregisterRoutesLocked()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	handle, err := host.RegisterProtectedAPI(p.ID(), plugins.ProtectedAPIRoute{Path: apiPath, Prefix: true, Handle: p.handleAPI})
	if err != nil {
		return err
	}
	p.routes = append(p.routes, handle)
	parent := context.Background()
	if lifecycle, ok := host.(plugins.LifecycleHost); ok && lifecycle.Lifetime() != nil {
		parent = lifecycle.Lifetime()
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	go p.loop(ctx, host)
	return nil
}

func (p *recipeBoxBackend) Stop(plugins.BackendStartHost) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.unregisterRoutesLocked()
	return nil
}

func (p *recipeBoxBackend) unregisterRoutesLocked() {
	for _, handle := range p.routes {
		handle.Unregister()
	}
	p.routes = nil
}

// ImportStoredMessage only says that there is new mail to look at. The scan
// reads it from the database on its own schedule, so a hook that ran for a
// message the reader never sent themselves costs one channel send.
func (p *recipeBoxBackend) ImportStoredMessage(context.Context, plugins.StoredMessageHost, plugins.StoredMessageContext) error {
	p.signal()
	return nil
}

func (p *recipeBoxBackend) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func userDB(ctx context.Context, host plugins.BackendHost, userID int64) (*sql.DB, error) {
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		return nil, errors.New("plugin host store is unavailable")
	}
	return st.UserDB(ctx, userID)
}

var (
	_ plugins.StoredMessageHook = (*recipeBoxBackend)(nil)
)
