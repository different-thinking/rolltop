// File overview: A token source for tests. It lives in the package rather than
// beside each client because both clients test the same retry contract, and two
// copies of the stub drift the moment that contract changes.

package xoauth2

import (
	"context"
	"sync"
)

// StubTokenSource hands out a scripted sequence of tokens and counts how often
// each entry point was used, which is what separates "retried with a fresh
// token" from "retried with the same one". The last token repeats once the
// script runs out.
type StubTokenSource struct {
	mu sync.Mutex
	// Tokens is the script, handed out in order.
	Tokens []string
	// Issued and Forced count the calls each entry point received.
	Issued int
	Forced int
	index  int
}

func (s *StubTokenSource) AccessToken(context.Context, int64, int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Issued++
	return s.next(), nil
}

func (s *StubTokenSource) ForceRefresh(context.Context, int64, int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Forced++
	return s.next(), nil
}

func (s *StubTokenSource) next() string {
	if len(s.Tokens) == 0 {
		return ""
	}
	if s.index >= len(s.Tokens) {
		return s.Tokens[len(s.Tokens)-1]
	}
	token := s.Tokens[s.index]
	s.index++
	return token
}
