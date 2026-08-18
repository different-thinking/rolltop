// File overview: Tests that the two 403s Google sends for different reasons stay apart.

package googlecalendar

import (
	"errors"
	"net/http"
	"testing"
)

func TestForbiddenSeparatesDisabledAPIFromMissingScope(t *testing.T) {
	disabledBody := []byte(`{"error":{"errors":[{"domain":"usageLimits","reason":"accessNotConfigured","message":"Access Not Configured."}],"code":403,"status":"PERMISSION_DENIED"}}`)
	disabled := statusError(http.StatusForbidden, errorReason(disabledBody))
	if !errors.Is(disabled, ErrServiceDisabled) {
		t.Fatalf("disabled API = %v, want ErrServiceDisabled", disabled)
	}
	if !errors.Is(disabled, ErrForbidden) {
		t.Fatalf("disabled API is no longer a forbidden error: %v", disabled)
	}

	// The gateway answers in the newer shape even where Calendar itself does not.
	gatewayBody := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED"}]}}`)
	if gateway := statusError(http.StatusForbidden, errorReason(gatewayBody)); !errors.Is(gateway, ErrServiceDisabled) {
		t.Fatalf("gateway-reported disabled API = %v, want ErrServiceDisabled", gateway)
	}

	scopeBody := []byte(`{"error":{"errors":[{"domain":"global","reason":"insufficientPermissions"}],"code":403,"status":"PERMISSION_DENIED"}}`)
	scope := statusError(http.StatusForbidden, errorReason(scopeBody))
	if !errors.Is(scope, ErrScopeInsufficient) {
		t.Fatalf("insufficient scope = %v, want ErrScopeInsufficient", scope)
	}
	if errors.Is(scope, ErrServiceDisabled) {
		t.Fatalf("insufficient scope was read as a disabled API: %v", scope)
	}

	plainBody := []byte(`{"error":{"errors":[{"domain":"calendar","reason":"forbiddenForServiceAccounts","message":"alice@example.test may not"}],"code":403}}`)
	plain := statusError(http.StatusForbidden, errorReason(plainBody))
	if !errors.Is(plain, ErrForbidden) || errors.Is(plain, ErrServiceDisabled) || errors.Is(plain, ErrScopeInsufficient) {
		t.Fatalf("unexplained 403 = %v, want a plain forbidden error", plain)
	}
	if got := plain.Error(); got != "google denied the request: forbiddenForServiceAccounts" {
		t.Fatalf("unexplained 403 text = %q", got)
	}
}
