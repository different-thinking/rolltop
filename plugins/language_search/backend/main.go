package main

import (
	"rolltop/backend/plugins"
	"rolltop/plugins/language_search/detector"
)

// RolltopPlugin is the symbol loaded by plugin.Open.
func RolltopPlugin() plugins.BackendPlugin {
	return plugins.NoopBackendPlugin{PluginID: plugins.LanguageSearch}
}

type languageSearchHook struct{}

// Compile-time proof the hook satisfies the host interface, so a missing method
// is a build error rather than a silent fail-open at the runtime type assertion.
var _ plugins.LanguageSearchHook = languageSearchHook{}

func (languageSearchHook) DetectLanguage(subject, body string) string {
	return detector.DetectCode(subject, body)
}

func (languageSearchHook) NormalizeLanguageCode(code string) string {
	return detector.NormalizeCode(code)
}
