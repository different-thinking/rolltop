package plugins

import "testing"

func TestAppRoutePathAcceptsWhatAPluginMayClaim(t *testing.T) {
	for _, path := range []string{"/files", "/files-archive", "/notes/inbox", "/a1"} {
		if !ValidAppRoutePath(path) {
			t.Errorf("ValidAppRoutePath(%q) = false, want a plugin to be able to claim it", path)
		}
	}
}

func TestAppRoutePathRefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"root", "/"},
		{"empty", ""},
		{"relative", "files"},
		{"trailing slash", "/files/"},
		{"uppercase", "/Files"},
		{"query", "/files?x=1"},
		{"wildcard", "/files/*"},
		{"traversal", "/files/../admin"},
		{"double slash", "//files"},
		{"a core route", "/mail"},
		{"below a core route", "/settings/account"},
		{"the api tree", "/api/plugins/x"},
		{"the plugin asset tree", "/plugins/webdav_archive/assets"},
		{"the attachment route", "/attachments/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ValidAppRoutePath(tc.path) {
				t.Errorf("ValidAppRoutePath(%q) = true, want it refused", tc.path)
			}
		})
	}
}
