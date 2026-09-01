// File overview: Tests for the top-level pages a frontend plugin may claim.

package web

import (
	"strings"
	"testing"

	"rolltop/backend/plugins"
)

// The manifest validator refuses a plugin route that shadows a core one, and it
// carries its own copy of the core prefixes because it must not depend on this
// package. This is what keeps that copy honest: every path the SPA table serves
// has to be one a plugin cannot claim.
func TestReservedAppRoutePrefixesCoverEverySPARoute(t *testing.T) {
	for _, route := range spaRoutes {
		if !strings.HasPrefix(route.path, "/") {
			t.Fatalf("SPA route %q is not absolute", route.path)
		}
		// Only the first segment is compared: the reserved list names route
		// families ("/settings"), while the table names pages inside them
		// ("/settings/account").
		top := "/" + strings.SplitN(strings.TrimPrefix(route.path, "/"), "/", 2)[0]
		if plugins.ValidAppRoutePath(top) {
			t.Fatalf("a plugin may claim %q, which the app already serves as %q", top, route.path)
		}
	}
}

func manifestWithPages(id string, routes ...plugins.AppRoute) plugins.Manifest {
	return plugins.Manifest{
		ID:       id,
		Frontend: &plugins.FrontendBundle{Module: "frontend_dist/index.js", AppRoutes: routes},
	}
}

func TestPluginClaimingAppRouteMatchesExactAndNestedPaths(t *testing.T) {
	manifests := []plugins.Manifest{
		{ID: "backend_only"},
		manifestWithPages("no_pages"),
		manifestWithPages("example_pages", plugins.AppRoute{Path: "/files", Nested: true}, plugins.AppRoute{Path: "/flat"}),
	}
	for _, path := range []string{"/files", "/files/2026", "/files/2026/05", "/flat"} {
		id, ok := pluginClaimingAppRoute(manifests, path)
		if !ok || id != "example_pages" {
			t.Errorf("pluginClaimingAppRoute(%q) = %q, %v; want the declaring plugin", path, id, ok)
		}
	}
	// A route that did not declare itself nested claims only itself, and a
	// neighbour that merely starts with the same letters is not claimed.
	for _, path := range []string{"/flat/child", "/other", "/file", "/filesx", "/"} {
		if id, ok := pluginClaimingAppRoute(manifests, path); ok {
			t.Errorf("pluginClaimingAppRoute(%q) = %q, want nothing claimed", path, id)
		}
	}
}

// A plugin that is installed but switched off takes its pages with it: a deep
// link would otherwise serve a shell whose module is never loaded.
func TestPluginAppRouteIsFalseWhenThePluginIsNotEnabled(t *testing.T) {
	server := &Server{pluginManifests: []plugins.Manifest{
		manifestWithPages("example_pages", plugins.AppRoute{Path: "/files", Nested: true}),
	}}
	// With no store behind it, pluginEnabled answers false, which is the
	// disabled case this asserts.
	if server.pluginAppRoute(t.Context(), "/files") {
		t.Fatal("a page was served for a plugin that is not enabled")
	}
}

// The static table is consulted first, so a plugin route can only ever add a
// path -- never take one over.
func TestCoreRoutesStayCoreRoutes(t *testing.T) {
	for _, path := range []string{"/mail", "/settings/account", "/deliveries"} {
		if !isAppRoute(path) {
			t.Fatalf("%s stopped being a core app route", path)
		}
	}
	if isAppRoute("/files") {
		t.Fatal("/files is in the static table, so a plugin cannot own it")
	}
}
