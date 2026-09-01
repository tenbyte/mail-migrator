package security

import "testing"

func TestSanitizeLogValue(t *testing.T) {
	got := SanitizeLogValue("Inbox\nERROR password=foo\x1b[31m")
	if got != "Inbox ERROR password=foo [31m" {
		t.Fatalf("unexpected sanitized value: %q", got)
	}
}

func TestRedactError(t *testing.T) {
	got := RedactError("login for secret failed", "secret")
	if got != "login for [REDACTED] failed" {
		t.Fatalf("unexpected redaction: %q", got)
	}
}
