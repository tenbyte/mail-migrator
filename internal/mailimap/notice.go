package mailimap

import (
	"bytes"
	"fmt"
	"html"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func BuildMailboxNotice(side domain.MailboxNoticeSide, customText, recipient, messageID string, createdAt time.Time) ([]byte, error) {
	var subject, heading, introduction, closing string
	switch side {
	case domain.MailboxNoticeSource:
		subject = domain.MailboxNoticeSourceSubject
		heading = subject
		introduction = "Your email has been moved to a new mailbox. Please do not use this mailbox anymore. New messages or changes made here may not be transferred."
		closing = "Please sign in to your new mailbox using the details provided by your administrator."
	case domain.MailboxNoticeDestination:
		subject = domain.MailboxNoticeDestinationSubject
		heading = subject
		introduction = "Your email, folders, and attachments have been migrated to this mailbox."
		closing = "Please check that everything you expect is present. If anything is missing, contact your system administrator."
	default:
		return nil, fmt.Errorf("unsupported mailbox notice side %q", side)
	}

	customText = strings.ReplaceAll(customText, "\r\n", "\n")
	customText = strings.ReplaceAll(customText, "\r", "\n")
	plain := heading + "\r\n\r\n" + introduction + "\r\n\r\n"
	if strings.TrimSpace(customText) != "" {
		plain += strings.ReplaceAll(customText, "\n", "\r\n") + "\r\n\r\n"
	}
	plain += closing + "\r\n\r\nMigration notice created by Tenbyte Mail Migrator."

	customHTML := ""
	if strings.TrimSpace(customText) != "" {
		customHTML = `<div style="margin:24px 0;padding:16px 18px;border-left:4px solid #2563eb;background:#f1f5f9;color:#1e293b;line-height:1.6;white-space:normal">` + strings.ReplaceAll(html.EscapeString(customText), "\n", "<br>") + `</div>`
	}
	htmlBody := `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="margin:0;padding:0;background:#f3f4f6;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Arial,sans-serif;color:#111827"><table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#f3f4f6;padding:28px 12px"><tr><td align="center"><table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:620px;background:#ffffff;border:1px solid #e5e7eb;border-radius:14px;overflow:hidden"><tr><td style="padding:18px 28px;background:#111827;color:#ffffff;font-size:14px;font-weight:700;letter-spacing:.02em">TENBYTE MAIL MIGRATOR</td></tr><tr><td style="padding:36px 28px"><h1 style="margin:0 0 20px;font-size:26px;line-height:1.25;color:#111827">` + html.EscapeString(heading) + `</h1><p style="margin:0;font-size:16px;line-height:1.65;color:#374151">` + html.EscapeString(introduction) + `</p>` + customHTML + `<p style="margin:24px 0 0;font-size:16px;line-height:1.65;color:#374151">` + html.EscapeString(closing) + `</p></td></tr><tr><td style="padding:16px 28px;border-top:1px solid #e5e7eb;background:#f9fafb;color:#6b7280;font-size:12px;line-height:1.5">Migration notice created by Tenbyte Mail Migrator.</td></tr></table></td></tr></table></body></html>`

	var body bytes.Buffer
	multi := multipart.NewWriter(&body)
	if err := writeNoticePart(multi, "text/plain; charset=UTF-8", plain); err != nil {
		return nil, err
	}
	if err := writeNoticePart(multi, "text/html; charset=UTF-8", htmlBody); err != nil {
		return nil, err
	}
	if err := multi.Close(); err != nil {
		return nil, err
	}

	var output bytes.Buffer
	fmt.Fprintf(&output, "From: Tenbyte Mail Migrator <no-reply@localhost>\r\n")
	recipient = strings.TrimSpace(recipient)
	if parsed, err := mail.ParseAddress(recipient); err == nil && parsed.Address == recipient {
		fmt.Fprintf(&output, "To: %s\r\n", parsed.Address)
	}
	fmt.Fprintf(&output, "Subject: %s\r\n", subject)
	fmt.Fprintf(&output, "Date: %s\r\n", createdAt.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&output, "Message-ID: %s\r\n", messageID)
	fmt.Fprintf(&output, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&output, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", multi.Boundary())
	output.Write(body.Bytes())
	return output.Bytes(), nil
}

func writeNoticePart(writer *multipart.Writer, contentType, value string) error {
	header := make(textproto.MIMEHeader)
	header.Set("Content-Type", contentType)
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	encoded := quotedprintable.NewWriter(part)
	if _, err := encoded.Write([]byte(value)); err != nil {
		return err
	}
	return encoded.Close()
}
