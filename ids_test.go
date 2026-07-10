package opencodeacp

import (
	"errors"
	"regexp"
	"testing"
)

func TestNewSessionIDShapeAndError(t *testing.T) {
	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("session id %q is not a v4 UUID", id)
	}

	other, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID again: %v", err)
	}
	if other == id {
		t.Fatal("consecutive session ids collided")
	}

	oldReader := sessionIDRandReader
	t.Cleanup(func() { sessionIDRandReader = oldReader })
	sessionIDRandReader = errorReader{err: errors.New("id failed")}
	if _, err := newSessionID(); err == nil {
		t.Fatal("exhausted random reader did not fail")
	}
}
