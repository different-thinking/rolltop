// File overview: Table-driven tests for the shared token-refresh retry contract
// that both the IMAP and SMTP clients depend on.

package googletoken

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// erroringSource fails at the credential exchange itself, which WithFreshToken
// must surface without ever running the attempt.
type erroringSource struct{ err error }

func (e erroringSource) AccessToken(context.Context, int64, int64) (string, error) {
	return "", e.err
}

func (e erroringSource) ForceRefresh(context.Context, int64, int64) (string, error) {
	return "", e.err
}

func TestWithFreshTokenNilSource(t *testing.T) {
	ran := false
	err := WithFreshToken(context.Background(), nil, 1, 2, func(string) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("attempt ran without a token source")
	}
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
}

func TestWithFreshTokenAccessTokenError(t *testing.T) {
	sentinel := errors.New("mint failed")
	ran := false
	err := WithFreshToken(context.Background(), erroringSource{err: sentinel}, 1, 2, func(string) error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("attempt ran despite an access-token error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the mint error", err)
	}
}

func TestWithFreshTokenRetry(t *testing.T) {
	authErr := AuthError{Err: errors.New("server rejected the credentials")}
	otherErr := errors.New("network unreachable")

	cases := []struct {
		name        string
		tokens      []string
		attemptErrs []error
		wantErr     error
		wantSeen    []string
		wantForced  int
	}{
		{
			name:        "success on first attempt does not refresh",
			tokens:      []string{"t1"},
			attemptErrs: []error{nil},
			wantErr:     nil,
			wantSeen:    []string{"t1"},
			wantForced:  0,
		},
		{
			name:        "non-auth failure is returned without a retry",
			tokens:      []string{"t1", "t2"},
			attemptErrs: []error{otherErr},
			wantErr:     otherErr,
			wantSeen:    []string{"t1"},
			wantForced:  0,
		},
		{
			name:        "auth failure retries once with a fresh token",
			tokens:      []string{"t1", "t2"},
			attemptErrs: []error{authErr, nil},
			wantErr:     nil,
			wantSeen:    []string{"t1", "t2"},
			wantForced:  1,
		},
		{
			name:        "refresh returning the same token is not retried",
			tokens:      []string{"t1", "t1"},
			attemptErrs: []error{authErr},
			wantErr:     authErr,
			wantSeen:    []string{"t1"},
			wantForced:  1,
		},
		{
			name:        "a fresh token that still fails returns the second error",
			tokens:      []string{"t1", "t2"},
			attemptErrs: []error{authErr, otherErr},
			wantErr:     otherErr,
			wantSeen:    []string{"t1", "t2"},
			wantForced:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &StubTokenSource{Tokens: tc.tokens}
			var seen []string
			call := 0
			err := WithFreshToken(context.Background(), src, 7, 9, func(token string) error {
				seen = append(seen, token)
				var e error
				if call < len(tc.attemptErrs) {
					e = tc.attemptErrs[call]
				}
				call++
				return e
			})
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(seen, tc.wantSeen) {
				t.Fatalf("attempt saw tokens %v, want %v", seen, tc.wantSeen)
			}
			if src.Forced != tc.wantForced {
				t.Fatalf("ForceRefresh calls = %d, want %d", src.Forced, tc.wantForced)
			}
		})
	}
}
