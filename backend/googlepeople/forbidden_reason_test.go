// File overview: Tests that the two 403s Google sends for different reasons stay apart.

package googlepeople

import (
	"errors"
	"net/http"
	"testing"
)

func TestForbiddenSeparatesDisabledAPIFromMissingScope(t *testing.T) {
	disabled := statusError(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"People API has not been used in project 1 before or it is disabled.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED","domain":"googleapis.com"}]}}`))
	if !errors.Is(disabled, ErrServiceDisabled) {
		t.Fatalf("disabled API = %v, want ErrServiceDisabled", disabled)
	}
	if !errors.Is(disabled, ErrForbidden) {
		t.Fatalf("disabled API is no longer a forbidden error: %v", disabled)
	}

	scope := statusError(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"Request had insufficient authentication scopes.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT","domain":"googleapis.com"}]}}`))
	if !errors.Is(scope, ErrScopeInsufficient) {
		t.Fatalf("insufficient scope = %v, want ErrScopeInsufficient", scope)
	}
	if errors.Is(scope, ErrServiceDisabled) {
		t.Fatalf("insufficient scope was read as a disabled API: %v", scope)
	}

	// A 403 Google does not explain stays the general case, and keeps saying so
	// without quoting the response body it came from.
	plain := statusError(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"alice@example.test is not allowed","status":"PERMISSION_DENIED"}}`))
	if !errors.Is(plain, ErrForbidden) || errors.Is(plain, ErrServiceDisabled) || errors.Is(plain, ErrScopeInsufficient) {
		t.Fatalf("unexplained 403 = %v, want a plain forbidden error", plain)
	}
	if got := plain.Error(); got != "google denied the request: PERMISSION_DENIED" {
		t.Fatalf("unexplained 403 text = %q", got)
	}
}
