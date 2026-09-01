package security

import (
	"log/slog"
	"strings"
	"unicode"
)

const Redacted = "[REDACTED]"

func SanitizeLogValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' || r == 0x1b || (unicode.IsControl(r) && r != ' ') {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func RedactError(message string, secrets ...string) string {
	result := message
	for _, secret := range secrets {
		if secret != "" {
			result = strings.ReplaceAll(result, secret, Redacted)
		}
	}
	return SanitizeLogValue(result)
}

func LogValue(key, value string) slog.Attr {
	return slog.String(key, SanitizeLogValue(value))
}
