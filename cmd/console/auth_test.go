package main

import (
	"testing"
	"time"
)

// The sign-in cookie, handed to anyone who visits /oauth/login, must never pass as a session.
func TestSignInCookieIsNoSession(t *testing.T) {
	a := &auth{key: []byte("0123456789abcdef0123456789abcdef")}
	state := a.sign("oauth", map[string]any{"state": "s", "verifier": "v", "exp": time.Now().Add(time.Hour).Unix()})
	var id identity
	if err := a.verify("session", state, &id); err == nil {
		t.Fatalf("the sign-in cookie verified as a session: %+v", id)
	}
	sess := a.sign("session", identity{Email: "a@example.org", Exp: time.Now().Add(time.Hour).Unix()})
	if err := a.verify("session", sess, &id); err != nil || id.Email != "a@example.org" {
		t.Fatalf("a real session was refused: %v %+v", err, id)
	}
}
