package mailimap

import (
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestBuildMailboxNoticeProducesSafeMultipartAlternative(t *testing.T) {
	raw, err := BuildMailboxNotice(domain.MailboxNoticeSource, "Server: mail.example\n<script>alert(1)</script>", "person@example.com", "<notice@example>", time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("Subject"); got != domain.MailboxNoticeSourceSubject {
		t.Fatalf("subject=%q", got)
	}
	if got := message.Header.Get("From"); got != "Tenbyte Mail Migrator <no-reply@localhost>" {
		t.Fatalf("from=%q", got)
	}
	if got := message.Header.Get("To"); got != "person@example.com" {
		t.Fatalf("to=%q", got)
	}
	mediaType, parameters, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/alternative" {
		t.Fatalf("content type=%q err=%v", mediaType, err)
	}
	reader := multipart.NewReader(message.Body, parameters["boundary"])
	parts := map[string]string{}
	for {
		part, partErr := reader.NextPart()
		if partErr == io.EOF {
			break
		}
		if partErr != nil {
			t.Fatal(partErr)
		}
		body, readErr := io.ReadAll(quotedprintable.NewReader(part))
		if readErr != nil {
			t.Fatal(readErr)
		}
		partType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		parts[partType] = string(body)
	}
	if !strings.Contains(parts["text/plain"], "Server: mail.example\r\n<script>alert(1)</script>") {
		t.Fatalf("plaintext custom copy missing: %q", parts["text/plain"])
	}
	html := parts["text/html"]
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(html, "Server: mail.example<br>") {
		t.Fatalf("HTML custom copy was not safely escaped: %q", html)
	}
	if strings.Contains(strings.ToLower(html), "<img") || strings.Contains(strings.ToLower(html), "http://") || strings.Contains(strings.ToLower(html), "https://") {
		t.Fatal("notice HTML must not contain external images or URLs")
	}
}

func TestBuildMailboxNoticeOmitsInvalidRecipient(t *testing.T) {
	raw, err := BuildMailboxNotice(domain.MailboxNoticeDestination, "", "not an email address", "<notice@example>", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if got := message.Header.Get("To"); got != "" {
		t.Fatalf("invalid recipient was included: %q", got)
	}
	if got := message.Header.Get("Subject"); got != domain.MailboxNoticeDestinationSubject {
		t.Fatalf("subject=%q", got)
	}
}
