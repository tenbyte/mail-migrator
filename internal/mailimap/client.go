package mailimap

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/tenbyte/mail-migrator/internal/domain"
	"github.com/tenbyte/mail-migrator/internal/folders"
)

type MessageMetadata struct {
	UID           uint32
	Flags         []string
	InternalDate  time.Time
	Size          int64
	SizeKnown     bool
	MessageID     string
	HeaderWarning string
}

type KeywordCount struct {
	Name     string
	Messages int64
}

type AppendResult struct{ UIDValidity, UID uint32 }

type MailboxLimits struct {
	AppendLimit         int64
	QuotaAvailableBytes int64
	QuotaUsedBytes      int64
}

type UncertainAppendError struct {
	Stage string
	Sent  int64
	Err   error
}

func (e *UncertainAppendError) Error() string {
	return fmt.Sprintf("uncertain APPEND during %s after %d bytes: %v", e.Stage, e.Sent, e.Err)
}
func (e *UncertainAppendError) Unwrap() error { return e.Err }

func IsUncertainAppend(err error) bool {
	var target *UncertainAppendError
	return errors.As(err, &target)
}

type Candidate struct {
	Found     bool
	Ambiguous bool
	UID       uint32
}

type MessageSummary struct {
	Subject string
	From    string
}

type Client interface {
	Capabilities(context.Context) ([]string, error)
	ListMailboxes(context.Context) ([]domain.Mailbox, error)
	SelectMailbox(context.Context, string, bool) (uidValidity, uidNext uint32, permanentFlags []string, err error)
	SearchUIDs(context.Context, uint32) ([]uint32, error)
	FetchMetadata(context.Context, uint32) (MessageMetadata, error)
	ListMessageMetadata(context.Context, string) ([]MessageMetadata, error)
	ListMessageKeywords(context.Context, string) ([]KeywordCount, error)
	FetchMessageID(context.Context, uint32) (string, error)
	StreamMessage(context.Context, uint32, func(io.Reader, int64) error) error
	AppendMessage(context.Context, string, MessageMetadata, io.Reader, []string, []string) (AppendResult, error)
	FindCandidate(context.Context, string, MessageMetadata) (Candidate, error)
	CreateMailbox(context.Context, string) error
	Limits(context.Context, string) (MailboxLimits, error)
	Close() error
}

type Factory interface {
	Connect(context.Context, domain.AccountConfig, time.Duration, time.Duration) (Client, error)
}

// DestinationClient intentionally lives outside Client so source-facing code
// cannot issue destructive IMAP commands.
type DestinationClient interface {
	Client
	MoveMessage(context.Context, uint32, string) error
	DeleteMessage(context.Context, uint32) error
}

type DestinationFactory interface {
	ConnectDestination(context.Context, domain.AccountConfig, time.Duration, time.Duration) (DestinationClient, error)
}

type SummaryClient interface {
	FetchSummary(context.Context, uint32) (MessageSummary, error)
}

type RealFactory struct{}

func (RealFactory) Connect(ctx context.Context, account domain.AccountConfig, connectTimeout, stallTimeout time.Duration) (Client, error) {
	return connect(ctx, account, connectTimeout, stallTimeout)
}

func (RealFactory) ConnectDestination(ctx context.Context, account domain.AccountConfig, connectTimeout, stallTimeout time.Duration) (DestinationClient, error) {
	return connect(ctx, account, connectTimeout, stallTimeout)
}

func connect(ctx context.Context, account domain.AccountConfig, connectTimeout, stallTimeout time.Duration) (*realClient, error) {
	if account.Host == "" || account.Port < 1 || account.Port > 65535 {
		return nil, errors.New("valid IMAP host and port are required")
	}
	if account.Encryption != domain.EncryptionTLS && account.Encryption != domain.EncryptionStartTLS {
		return nil, errors.New("only TLS and STARTTLS are supported")
	}
	if connectTimeout <= 0 {
		connectTimeout = 15 * time.Second
	}
	if stallTimeout <= 0 {
		stallTimeout = 90 * time.Second
	}
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(account.Host, strconv.Itoa(account.Port)))
	if err != nil {
		return nil, fmt.Errorf("connect to IMAP server: %w", err)
	}
	activity := &activityConn{Conn: raw, stall: stallTimeout}
	tlsConfig := &tls.Config{ServerName: account.Host, MinVersion: tls.VersionTLS12}
	options := &imapclient.Options{TLSConfig: tlsConfig}
	var c *imapclient.Client
	if account.Encryption == domain.EncryptionTLS {
		tlsConn := tls.Client(activity, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, fmt.Errorf("TLS certificate or handshake failed: %w", err)
		}
		c = imapclient.New(tlsConn, options)
	} else {
		c, err = imapclient.NewStartTLS(activity, options)
		if err != nil {
			raw.Close()
			return nil, fmt.Errorf("STARTTLS failed: %w", err)
		}
	}
	if err := c.Login(account.Username, account.Password).Wait(); err != nil {
		c.Close()
		return nil, fmt.Errorf("IMAP authentication failed: %w", err)
	}
	client := &realClient{client: c}
	caps, capErr := client.ensureCapabilities()
	if capErr != nil {
		_ = c.Close()
		return nil, fmt.Errorf("read IMAP capabilities: %w", capErr)
	}
	if needsRev2Enable(caps) {
		if _, enableErr := c.Enable(imap.CapIMAP4rev2).Wait(); enableErr != nil {
			_ = c.Close()
			return nil, fmt.Errorf("enable IMAP4rev2 compatibility mode: %w", enableErr)
		}
	}
	return client, nil
}

func needsRev2Enable(caps imap.CapSet) bool {
	return caps.Has(imap.CapIMAP4rev1) && caps.Has(imap.CapIMAP4rev2)
}

type activityConn struct {
	net.Conn
	stall time.Duration
	mu    sync.Mutex
}

func (c *activityConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.stall))
	c.mu.Unlock()
	return c.Conn.Read(p)
}
func (c *activityConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.stall))
	c.mu.Unlock()
	return c.Conn.Write(p)
}

type realClient struct {
	client       *imapclient.Client
	capabilities imap.CapSet
	selectedName string
	selectedSize uint32
}

func (c *realClient) ensureCapabilities() (imap.CapSet, error) {
	if c.capabilities != nil {
		return c.capabilities, nil
	}
	caps, err := c.client.Capability().Wait()
	if err != nil {
		return nil, err
	}
	c.capabilities = caps
	return caps, nil
}

func (c *realClient) Capabilities(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	caps, err := c.ensureCapabilities()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(caps))
	for cap := range caps {
		out = append(out, string(cap))
	}
	sort.Strings(out)
	return out, nil
}

func (c *realClient) ListMailboxes(ctx context.Context) ([]domain.Mailbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	caps, err := c.ensureCapabilities()
	if err != nil {
		return nil, err
	}
	statusOpts := &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, Size: caps.Has(imap.CapStatusSize)}
	var opts *imap.ListOptions
	if caps.Has(imap.CapListExtended) || caps.Has(imap.CapIMAP4rev2) {
		opts = &imap.ListOptions{ReturnChildren: true, ReturnSpecialUse: caps.Has(imap.CapSpecialUse)}
	}
	if caps.Has(imap.CapListStatus) || caps.Has(imap.CapIMAP4rev2) {
		if opts == nil {
			opts = &imap.ListOptions{}
		}
		opts.ReturnStatus = statusOpts
	}
	items, err := c.client.List("", "*", opts).Collect()
	if err != nil && opts != nil && opts.ReturnStatus != nil {
		// A few nominal LIST-STATUS implementations reject RETURN STATUS. The
		// tagged BAD leaves the connection usable, so retry with classic LIST.
		opts.ReturnStatus = nil
		items, err = c.client.List("", "*", opts).Collect()
	}
	if err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	result := make([]domain.Mailbox, 0, len(items))
	for _, item := range items {
		attrs := make([]string, len(item.Attrs))
		for i, a := range item.Attrs {
			attrs[i] = string(a)
		}
		box := domain.Mailbox{Name: item.Mailbox, Delimiter: item.Delim, Attributes: attrs, SpecialUse: folders.SpecialUse(attrs), Selectable: !hasAttr(item.Attrs, imap.MailboxAttrNoSelect) && !hasAttr(item.Attrs, imap.MailboxAttrNonExistent)}
		if box.Selectable {
			status := item.Status
			var statusErr error
			if status == nil {
				status, statusErr = c.client.Status(box.Name, statusOpts).Wait()
			}
			if statusErr != nil {
				selected, selectErr := c.client.Select(box.Name, &imap.SelectOptions{ReadOnly: true}).Wait()
				if selectErr != nil {
					return nil, fmt.Errorf("read status for %q: %w (EXAMINE fallback: %v)", box.Name, statusErr, selectErr)
				}
				box.Messages = selected.NumMessages
				box.UIDNext = uint32(selected.UIDNext)
				box.UIDValidity = selected.UIDValidity
				if box.Messages == 0 {
					box.SizeKnown = true
				}
				result = append(result, box)
				continue
			}
			if status.NumMessages != nil {
				box.Messages = *status.NumMessages
			}
			box.UIDNext = uint32(status.UIDNext)
			box.UIDValidity = status.UIDValidity
			if status.Size != nil {
				box.Size = *status.Size
				box.SizeKnown = true
			} else if box.Messages == 0 {
				box.SizeKnown = true
			}
		}
		result = append(result, box)
	}
	return result, nil
}

func (c *realClient) FetchSummary(ctx context.Context, uid uint32) (MessageSummary, error) {
	if err := ctx.Err(); err != nil {
		return MessageSummary{}, err
	}
	// Some IMAP servers return an empty literal for HEADER.FIELDS even though a
	// complete header fetch works. Source-deletion summaries are fetched only
	// for the small set of target-only messages, so prefer the interoperable
	// complete header here.
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	cmd := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}})
	defer cmd.Close()
	msg := cmd.Next()
	if msg == nil {
		return MessageSummary{}, errors.New("message summary not returned")
	}
	for item := msg.Next(); item != nil; item = msg.Next() {
		body, ok := item.(imapclient.FetchItemDataBodySection)
		if !ok || body.Literal == nil {
			continue
		}
		return readMessageSummary(body.Literal)
	}
	return MessageSummary{}, nil
}

func readMessageSummary(reader io.Reader) (MessageSummary, error) {
	parsed, err := mail.ReadMessage(reader)
	if err != nil {
		return MessageSummary{}, err
	}
	decoder := &mime.WordDecoder{}
	subject, subjectErr := decoder.DecodeHeader(parsed.Header.Get("Subject"))
	if subjectErr != nil {
		subject = parsed.Header.Get("Subject")
	}
	from, fromErr := decoder.DecodeHeader(parsed.Header.Get("From"))
	if fromErr != nil {
		from = parsed.Header.Get("From")
	}
	return MessageSummary{Subject: subject, From: from}, nil
}

// ListMessageMetadata inventories one mailbox without downloading message
// bodies. Requests are chunked so large folders do not create an unbounded
// IMAP command or in-memory result inside the protocol client.
func (c *realClient) ListMessageMetadata(ctx context.Context, mailbox string) ([]MessageMetadata, error) {
	if _, _, _, err := c.SelectMailbox(ctx, mailbox, true); err != nil {
		return nil, err
	}
	uids, err := c.SearchUIDs(ctx, 0)
	if err != nil {
		return nil, err
	}
	const chunkSize = 500
	result := make([]MessageMetadata, 0, len(uids))
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}, Peek: true}
	for start := 0; start < len(uids); start += chunkSize {
		end := min(start+chunkSize, len(uids))
		setUIDs := make([]imap.UID, end-start)
		for i, uid := range uids[start:end] {
			setUIDs[i] = imap.UID(uid)
		}
		cmd := c.client.Fetch(imap.UIDSetNum(setUIDs...), &imap.FetchOptions{UID: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}})
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			meta := MessageMetadata{}
			for item := msg.Next(); item != nil; item = msg.Next() {
				switch value := item.(type) {
				case imapclient.FetchItemDataUID:
					meta.UID = uint32(value.UID)
				case imapclient.FetchItemDataInternalDate:
					meta.InternalDate = value.Time
				case imapclient.FetchItemDataRFC822Size:
					meta.Size = value.Size
					meta.SizeKnown = true
				case imapclient.FetchItemDataBodySection:
					if value.Literal != nil {
						messageID, parseErr := readMessageID(value.Literal)
						if parseErr != nil {
							_ = cmd.Close()
							return nil, parseErr
						}
						meta.MessageID = messageID
					}
				}
			}
			if meta.UID == 0 || !meta.SizeKnown {
				_ = cmd.Close()
				return nil, fmt.Errorf("mailbox %q returned incomplete metadata", mailbox)
			}
			result = append(result, meta)
		}
		if err := cmd.Close(); err != nil {
			return nil, fmt.Errorf("inventory metadata for %q: %w", mailbox, err)
		}
	}
	if len(result) != len(uids) {
		return nil, fmt.Errorf("mailbox %q returned metadata for %d of %d messages", mailbox, len(result), len(uids))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UID < result[j].UID })
	return result, nil
}

// ListMessageKeywords inventories custom IMAP keywords without downloading
// headers or message bodies. System flags beginning with a backslash are mail
// state, not user-selectable tags, and are intentionally omitted.
func (c *realClient) ListMessageKeywords(ctx context.Context, mailbox string) ([]KeywordCount, error) {
	if _, _, _, err := c.SelectMailbox(ctx, mailbox, true); err != nil {
		return nil, err
	}
	uids, err := c.SearchUIDs(ctx, 0)
	if err != nil {
		return nil, err
	}
	type aggregate struct {
		name  string
		count int64
	}
	counts := make(map[string]*aggregate)
	const chunkSize = 500
	for start := 0; start < len(uids); start += chunkSize {
		end := min(start+chunkSize, len(uids))
		setUIDs := make([]imap.UID, end-start)
		for i, uid := range uids[start:end] {
			setUIDs[i] = imap.UID(uid)
		}
		cmd := c.client.Fetch(imap.UIDSetNum(setUIDs...), &imap.FetchOptions{UID: true, Flags: true})
		messages := 0
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			messages++
			seen := make(map[string]struct{})
			for item := msg.Next(); item != nil; item = msg.Next() {
				flags, ok := item.(imapclient.FetchItemDataFlags)
				if !ok {
					continue
				}
				for _, raw := range flags.Flags {
					name := strings.TrimSpace(string(raw))
					if name == "" || strings.HasPrefix(name, "\\") {
						continue
					}
					key := strings.ToLower(name)
					if _, duplicate := seen[key]; duplicate {
						continue
					}
					seen[key] = struct{}{}
					entry := counts[key]
					if entry == nil {
						entry = &aggregate{name: name}
						counts[key] = entry
					}
					entry.count++
				}
			}
		}
		if err := cmd.Close(); err != nil {
			return nil, fmt.Errorf("inventory keywords for %q: %w", mailbox, err)
		}
		if messages != len(setUIDs) {
			return nil, fmt.Errorf("mailbox %q returned flags for %d of %d messages", mailbox, messages, len(setUIDs))
		}
	}
	result := make([]KeywordCount, 0, len(counts))
	for _, item := range counts {
		result = append(result, KeywordCount{Name: item.name, Messages: item.count})
	}
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name) })
	return result, nil
}

func (c *realClient) SelectMailbox(ctx context.Context, name string, readOnly bool) (uint32, uint32, []string, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, nil, err
	}
	data, err := c.client.Select(name, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		return 0, 0, nil, fmt.Errorf("open mailbox %q: %w", name, err)
	}
	c.selectedName = name
	c.selectedSize = data.NumMessages
	flags := make([]string, len(data.PermanentFlags))
	for i, f := range data.PermanentFlags {
		flags[i] = string(f)
	}
	return data.UIDValidity, uint32(data.UIDNext), flags, nil
}

func (c *realClient) SearchUIDs(ctx context.Context, after uint32) ([]uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	set := imap.UIDSet{}
	start := imap.UID(after + 1)
	if start == 0 {
		start = 1
	}
	set.AddRange(start, 0)
	data, err := c.client.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("search source UIDs: %w", err)
	}
	raw := data.AllUIDs()
	out := make([]uint32, len(raw))
	for i, u := range raw {
		out[i] = uint32(u)
	}
	if after == 0 && c.selectedSize > 0 && len(out) == 0 {
		return nil, fmt.Errorf("mailbox %q reports %d messages but a complete UID SEARCH returned none", c.selectedName, c.selectedSize)
	}
	return out, nil
}

func (c *realClient) FetchMetadata(ctx context.Context, uid uint32) (MessageMetadata, error) {
	if err := ctx.Err(); err != nil {
		return MessageMetadata{}, err
	}
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}, Peek: true}
	cmd := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}})
	defer cmd.Close()
	msg := cmd.Next()
	if msg == nil {
		return MessageMetadata{}, errors.New("message metadata not returned")
	}
	meta := MessageMetadata{UID: uid}
	for item := msg.Next(); item != nil; item = msg.Next() {
		switch v := item.(type) {
		case imapclient.FetchItemDataUID:
			meta.UID = uint32(v.UID)
		case imapclient.FetchItemDataFlags:
			for _, f := range v.Flags {
				if !strings.EqualFold(string(f), "\\Recent") {
					meta.Flags = append(meta.Flags, string(f))
				}
			}
		case imapclient.FetchItemDataInternalDate:
			meta.InternalDate = v.Time
		case imapclient.FetchItemDataRFC822Size:
			meta.Size = v.Size
			meta.SizeKnown = true
		case imapclient.FetchItemDataBodySection:
			if v.Literal != nil {
				meta.MessageID, _ = readMessageID(v.Literal)
			}
		}
	}
	if err := cmd.Close(); err != nil {
		return MessageMetadata{}, fmt.Errorf("fetch metadata for UID %d: %w", uid, err)
	}
	if meta.Size < 0 {
		return MessageMetadata{}, errors.New("server returned invalid message size")
	}
	return meta, nil
}

func (c *realClient) FetchMessageID(ctx context.Context, uid uint32) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}, Peek: true}
	cmd := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}})
	defer cmd.Close()
	msg := cmd.Next()
	if msg == nil {
		return "", errors.New("Message-ID header was not returned")
	}
	for item := msg.Next(); item != nil; item = msg.Next() {
		body, ok := item.(imapclient.FetchItemDataBodySection)
		if !ok || body.Literal == nil {
			continue
		}
		messageID, err := readMessageID(body.Literal)
		if err != nil {
			return "", err
		}
		return messageID, nil
	}
	if err := cmd.Close(); err != nil {
		return "", err
	}
	return "", nil
}

func readMessageID(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(strings.ToLower(line), "message-id:") {
			return strings.TrimSpace(line[len("message-id:"):]), nil
		}
	}
	return "", scanner.Err()
}

func (c *realClient) StreamMessage(ctx context.Context, uid uint32, consume func(io.Reader, int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	section := &imap.FetchItemBodySection{Peek: true}
	cmd := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}})
	defer cmd.Close()
	msg := cmd.Next()
	if msg == nil {
		return errors.New("message body not returned")
	}
	found := false
	for item := msg.Next(); item != nil; item = msg.Next() {
		if body, ok := item.(imapclient.FetchItemDataBodySection); ok && body.Literal != nil {
			found = true
			if err := consume(body.Literal, body.Literal.Size()); err != nil {
				return err
			}
		}
	}
	if !found {
		return errors.New("server did not return a raw message literal")
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("stream raw message UID %d: %w", uid, err)
	}
	return nil
}

func (c *realClient) AppendMessage(ctx context.Context, mailbox string, meta MessageMetadata, reader io.Reader, allowed, excludedKeywords []string) (AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return AppendResult{}, err
	}
	flags := filterFlags(meta.Flags, allowed, excludedKeywords)
	cmd := c.client.Append(mailbox, meta.Size, &imap.AppendOptions{Flags: toFlags(flags), Time: meta.InternalDate})
	n, copyErr := io.CopyBuffer(cmd, io.LimitReader(reader, meta.Size), make([]byte, 64*1024))
	closeErr := cmd.Close()
	if copyErr != nil {
		return AppendResult{}, fmt.Errorf("APPEND stream failed after %d bytes: %w", n, copyErr)
	}
	if n != meta.Size {
		return AppendResult{}, fmt.Errorf("source literal size changed: expected %d, received %d", meta.Size, n)
	}
	if closeErr != nil {
		return AppendResult{}, &UncertainAppendError{Stage: "literal close", Sent: n, Err: closeErr}
	}
	data, waitErr := cmd.Wait()
	if waitErr != nil {
		return AppendResult{}, &UncertainAppendError{Stage: "server response", Sent: n, Err: waitErr}
	}
	return AppendResult{UIDValidity: data.UIDValidity, UID: uint32(data.UID)}, nil
}

func (c *realClient) FindCandidate(ctx context.Context, mailbox string, meta MessageMetadata) (Candidate, error) {
	if meta.MessageID == "" {
		return Candidate{}, nil
	}
	if _, _, _, err := c.SelectMailbox(ctx, mailbox, true); err != nil {
		return Candidate{}, err
	}
	criteria := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: meta.MessageID}}}
	data, err := c.client.UIDSearch(criteria, &imap.SearchOptions{ReturnAll: true}).Wait()
	if err != nil {
		return Candidate{}, err
	}
	var match Candidate
	for _, uid := range data.AllUIDs() {
		candidate, fetchErr := c.FetchMetadata(ctx, uint32(uid))
		if fetchErr != nil {
			return Candidate{}, fetchErr
		}
		if candidate.Size == meta.Size && withinOneSecond(candidate.InternalDate, meta.InternalDate) {
			if match.Found {
				return Candidate{Ambiguous: true}, nil
			}
			match = Candidate{Found: true, UID: uint32(uid)}
		}
	}
	return match, nil
}

func (c *realClient) CreateMailbox(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !folders.SafeName(name) {
		return errors.New("invalid mailbox name")
	}
	err := c.client.Create(name, nil).Wait()
	var imapErr *imap.Error
	alreadyExists := errors.As(err, &imapErr) && imapErr.Code == imap.ResponseCodeAlreadyExists
	if err != nil && !alreadyExists && !(strings.Contains(strings.ToLower(err.Error()), "already exists") || strings.Contains(strings.ToLower(err.Error()), "exists")) {
		return fmt.Errorf("create mailbox %q: %w", name, err)
	}
	return nil
}

func (c *realClient) MoveMessage(ctx context.Context, uid uint32, mailbox string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !folders.SafeName(mailbox) {
		return errors.New("invalid mailbox name")
	}
	caps, err := c.ensureCapabilities()
	if err != nil {
		return err
	}
	if !caps.Has(imap.CapMove) && !caps.Has(imap.CapIMAP4rev2) && !caps.Has(imap.CapUIDPlus) {
		return errors.New("the destination server does not support a safe targeted MOVE")
	}
	if _, err := c.client.Move(imap.UIDSetNum(imap.UID(uid)), mailbox).Wait(); err != nil {
		return fmt.Errorf("move destination UID %d: %w", uid, err)
	}
	return nil
}

func (c *realClient) DeleteMessage(ctx context.Context, uid uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	caps, err := c.ensureCapabilities()
	if err != nil {
		return err
	}
	if !caps.Has(imap.CapUIDPlus) && !caps.Has(imap.CapIMAP4rev2) {
		return errors.New("the destination server does not support safe UID EXPUNGE")
	}
	set := imap.UIDSetNum(imap.UID(uid))
	store := c.client.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil)
	if err := store.Close(); err != nil {
		return fmt.Errorf("mark destination UID %d deleted: %w", uid, err)
	}
	if err := c.client.UIDExpunge(set).Close(); err != nil {
		// Best effort rollback keeps a failed deletion visible to the user.
		_ = c.client.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
		return fmt.Errorf("expunge destination UID %d: %w", uid, err)
	}
	return nil
}

func (c *realClient) Limits(ctx context.Context, mailbox string) (MailboxLimits, error) {
	if err := ctx.Err(); err != nil {
		return MailboxLimits{}, err
	}
	caps, err := c.ensureCapabilities()
	if err != nil {
		return MailboxLimits{}, err
	}
	var limits MailboxLimits
	for capability := range caps {
		upper := strings.ToUpper(string(capability))
		if strings.HasPrefix(upper, "APPENDLIMIT=") {
			_, _ = fmt.Sscanf(strings.TrimPrefix(upper, "APPENDLIMIT="), "%d", &limits.AppendLimit)
		}
	}
	if !caps.Has(imap.CapQuota) || mailbox == "" {
		return limits, nil
	}
	quotas, err := c.client.GetQuotaRoot(mailbox).Wait()
	if err != nil {
		return limits, fmt.Errorf("read destination quota: %w", err)
	}
	for _, quota := range quotas {
		storage, ok := quota.Resources[imap.QuotaResourceStorage]
		if !ok {
			continue
		}
		limits.QuotaUsedBytes = storage.Usage * 1024
		limitBytes := storage.Limit * 1024
		if limitBytes > limits.QuotaUsedBytes {
			limits.QuotaAvailableBytes = limitBytes - limits.QuotaUsedBytes
		}
		break
	}
	return limits, nil
}
func (c *realClient) Close() error { return c.client.Close() }

func hasAttr(attrs []imap.MailboxAttr, wanted imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if strings.EqualFold(string(attr), string(wanted)) {
			return true
		}
	}
	return false
}
func toFlags(flags []string) []imap.Flag {
	out := make([]imap.Flag, len(flags))
	for i, f := range flags {
		out[i] = imap.Flag(f)
	}
	return out
}
func filterFlags(flags, allowed, excludedKeywords []string) []string {
	wildcard := false
	allowedSet := map[string]bool{}
	excludedSet := make(map[string]bool, len(excludedKeywords))
	for _, keyword := range excludedKeywords {
		excludedSet[strings.ToLower(strings.TrimSpace(keyword))] = true
	}
	for _, f := range allowed {
		if f == "\\*" {
			wildcard = true
		}
		allowedSet[strings.ToLower(f)] = true
	}
	var out []string
	for _, f := range flags {
		if strings.EqualFold(f, "\\Recent") {
			continue
		}
		if !strings.HasPrefix(f, "\\") && excludedSet[strings.ToLower(f)] {
			continue
		}
		if strings.HasPrefix(f, "\\") || wildcard || allowedSet[strings.ToLower(f)] {
			out = append(out, f)
		}
	}
	return out
}
func withinOneSecond(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= time.Second
}
