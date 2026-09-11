package sstransport

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestNewRejectsEmpty(t *testing.T) {
	if _, err := New(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty url")
	}
}

func TestNewRejectsGarbage(t *testing.T) {
	if _, err := New(context.Background(), "not-a-url"); err == nil {
		t.Fatal("expected error for garbage url")
	}
}

// TestNewParsesFakeSSURL checks that the SDK accepts a syntactically-valid
// ss:// URL. The URL points to a black hole (2001:db8::/32 is reserved for
// docs) so no real dial ever happens.
func TestNewParsesFakeSSURL(t *testing.T) {
	// aes-256-gcm:testpassword base64'd for the userinfo portion.
	userinfo := base64.URLEncoding.EncodeToString([]byte("aes-256-gcm:testpassword"))
	url := fmt.Sprintf("ss://%s@[2001:db8::1]:8388#fake", userinfo)
	d, err := New(context.Background(), url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.Inner() == nil {
		t.Error("inner dialer is nil")
	}
}

func TestRedactSSHidesCredential(t *testing.T) {
	url := "ss://YWVzLTI1Ni1nY206dGVzdHBhc3N3b3Jk@1.2.3.4:8388#label"
	got := redactSS(url)
	if strings.Contains(got, "YWVzLTI1Ni1nY206dGVzdHBhc3N3b3Jk") {
		t.Errorf("credential leaked in redacted form: %q", got)
	}
	if !strings.Contains(got, "1.2.3.4:8388") {
		t.Errorf("host missing from redacted form: %q", got)
	}
}

func TestRedactSSHandlesNonSS(t *testing.T) {
	got := redactSS("http://example.com")
	if strings.Contains(got, "example.com") {
		t.Errorf("non-ss url should be replaced entirely: %q", got)
	}
}
