package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/domain"
	"github.com/tenbyte/mail-migrator/internal/mailimap"
)

type reconciliationClient struct {
	uids          []uint32
	metadata      map[uint32]mailimap.MessageMetadata
	candidates    map[string]mailimap.Candidate
	mailboxes     []domain.Mailbox
	listResponses [][]domain.Mailbox
	listCalls     int
	createCalls   []string
	createErrors  map[string]error
	raw           map[uint32][]byte
	summaries     map[uint32]mailimap.MessageSummary
	keywords      map[string][]mailimap.KeywordCount
	uidValidity   uint32
}

func (c *reconciliationClient) Capabilities(context.Context) ([]string, error) { return nil, nil }
func (c *reconciliationClient) ListMailboxes(context.Context) ([]domain.Mailbox, error) {
	if len(c.listResponses) > 0 {
		index := min(c.listCalls, len(c.listResponses)-1)
		c.listCalls++
		return c.listResponses[index], nil
	}
	c.listCalls++
	return c.mailboxes, nil
}
func (c *reconciliationClient) SelectMailbox(context.Context, string, bool) (uint32, uint32, []string, error) {
	return c.uidValidity, 0, nil, nil
}
func (c *reconciliationClient) SearchUIDs(context.Context, uint32) ([]uint32, error) {
	return c.uids, nil
}
func (c *reconciliationClient) FetchMetadata(_ context.Context, uid uint32) (mailimap.MessageMetadata, error) {
	return c.metadata[uid], nil
}
func (c *reconciliationClient) ListMessageMetadata(_ context.Context, _ string) ([]mailimap.MessageMetadata, error) {
	result := make([]mailimap.MessageMetadata, 0, len(c.metadata))
	for _, uid := range c.uids {
		if metadata, ok := c.metadata[uid]; ok {
			result = append(result, metadata)
		}
	}
	return result, nil
}
func (c *reconciliationClient) ListMessageKeywords(_ context.Context, mailbox string) ([]mailimap.KeywordCount, error) {
	return c.keywords[mailbox], nil
}
func (c *reconciliationClient) FetchMessageID(_ context.Context, uid uint32) (string, error) {
	return c.metadata[uid].MessageID, nil
}
func (c *reconciliationClient) FetchSummary(_ context.Context, uid uint32) (mailimap.MessageSummary, error) {
	return c.summaries[uid], nil
}
func (c *reconciliationClient) StreamMessage(_ context.Context, uid uint32, consume func(io.Reader, int64) error) error {
	if c.raw == nil {
		return nil
	}
	raw, ok := c.raw[uid]
	if !ok {
		return errors.New("message body not returned")
	}
	return consume(bytes.NewReader(raw), int64(len(raw)))
}
func (c *reconciliationClient) AppendMessage(context.Context, string, mailimap.MessageMetadata, io.Reader, []string, []string) (mailimap.AppendResult, error) {
	return mailimap.AppendResult{}, nil
}
func (c *reconciliationClient) FindCandidate(_ context.Context, _ string, metadata mailimap.MessageMetadata) (mailimap.Candidate, error) {
	return c.candidates[metadata.MessageID], nil
}
func (c *reconciliationClient) CreateMailbox(_ context.Context, name string) error {
	c.createCalls = append(c.createCalls, name)
	return c.createErrors[name]
}
func (c *reconciliationClient) Limits(context.Context, string) (mailimap.MailboxLimits, error) {
	return mailimap.MailboxLimits{}, nil
}
func (c *reconciliationClient) Close() error { return nil }

type staticFactory struct{ client mailimap.Client }

func (f staticFactory) Connect(context.Context, domain.AccountConfig, time.Duration, time.Duration) (mailimap.Client, error) {
	return f.client, nil
}

type mutationClient struct {
	*reconciliationClient
	moves     []uint32
	moveBoxes []string
	deletes   []uint32
	moveErr   error
	deleteErr error
}

func (c *mutationClient) MoveMessage(_ context.Context, uid uint32, mailbox string) error {
	c.moves = append(c.moves, uid)
	c.moveBoxes = append(c.moveBoxes, mailbox)
	return c.moveErr
}

func (c *mutationClient) DeleteMessage(_ context.Context, uid uint32) error {
	c.deletes = append(c.deletes, uid)
	return c.deleteErr
}

type destinationFactory struct{ client *mutationClient }

func (f destinationFactory) Connect(context.Context, domain.AccountConfig, time.Duration, time.Duration) (mailimap.Client, error) {
	return f.client, nil
}

func (f destinationFactory) ConnectDestination(context.Context, domain.AccountConfig, time.Duration, time.Duration) (mailimap.DestinationClient, error) {
	return f.client, nil
}

func folderRequest(mappings ...domain.FolderMapping) domain.StartRequest {
	return domain.StartRequest{
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    mappings,
		Options:     domain.DefaultTransferOptions(),
	}
}

func TestCreateFoldersNeverCreatesExistingInbox(t *testing.T) {
	client := &reconciliationClient{mailboxes: []domain.Mailbox{{Name: "INBOX", Selectable: true}}}
	service := New(nil, staticFactory{client: client}, nil)
	request := folderRequest(domain.FolderMapping{DestinationName: "INBOX", DestinationExists: true, Enabled: true})
	if err := service.createFolders(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(client.createCalls) != 0 {
		t.Fatalf("CREATE was called for existing INBOX: %v", client.createCalls)
	}
}

func TestCreateFoldersSkipsExistingParents(t *testing.T) {
	client := &reconciliationClient{mailboxes: []domain.Mailbox{{Name: "Projects", Selectable: true}}}
	service := New(nil, staticFactory{client: client}, nil)
	request := folderRequest(domain.FolderMapping{DestinationName: "Projects/2026", DestinationDelimiter: '/', Enabled: true})
	if err := service.createFolders(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(client.createCalls) != 1 || client.createCalls[0] != "Projects/2026" {
		t.Fatalf("unexpected CREATE calls: %v", client.createCalls)
	}
}

func TestCreateFoldersCreatesMissingFolderOnce(t *testing.T) {
	client := &reconciliationClient{}
	service := New(nil, staticFactory{client: client}, nil)
	mapping := domain.FolderMapping{DestinationName: "Archive", Enabled: true}
	if err := service.createFolders(context.Background(), folderRequest(mapping, mapping)); err != nil {
		t.Fatal(err)
	}
	if len(client.createCalls) != 1 || client.createCalls[0] != "Archive" {
		t.Fatalf("missing folder was not created exactly once: %v", client.createCalls)
	}
}

func TestCreateFoldersAcceptsConcurrentCreateRace(t *testing.T) {
	client := &reconciliationClient{
		listResponses: [][]domain.Mailbox{{}, {{Name: "Archive", Selectable: true}}},
		createErrors:  map[string]error{"Archive": errors.New("imap: NO [CANNOT] already created elsewhere")},
	}
	service := New(nil, staticFactory{client: client}, nil)
	if err := service.createFolders(context.Background(), folderRequest(domain.FolderMapping{DestinationName: "Archive", Enabled: true})); err != nil {
		t.Fatal(err)
	}
	if client.listCalls != 2 {
		t.Fatalf("expected a fresh inventory after CREATE error, got %d LIST calls", client.listCalls)
	}
}

func TestCreateFoldersRejectsMissingInbox(t *testing.T) {
	client := &reconciliationClient{}
	service := New(nil, staticFactory{client: client}, nil)
	err := service.createFolders(context.Background(), folderRequest(domain.FolderMapping{DestinationName: "INBOX", Enabled: true}))
	if err == nil || len(client.createCalls) != 0 {
		t.Fatalf("missing INBOX must fail without CREATE, err=%v calls=%v", err, client.createCalls)
	}
}

func TestMergeUIDsOrdersAndDeduplicates(t *testing.T) {
	got := mergeUIDs([]uint32{9, 2, 2}, []uint32{10, 9, 4})
	want := []uint32{2, 4, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestUIDsAfter(t *testing.T) {
	got := uidsAfter([]uint32{1, 4, 7, 12}, 4)
	if len(got) != 2 || got[0] != 7 || got[1] != 12 {
		t.Fatalf("unexpected UIDs: %v", got)
	}
}

func TestHashSelectedMessageQuarantinesEmptyAndSizeMismatch(t *testing.T) {
	client := &reconciliationClient{raw: map[uint32][]byte{1: {}, 2: []byte("abc")}}
	_, _, err := hashSelectedMessage(context.Background(), client, 1, 0, false)
	var mailIssue *mailIssueError
	if !errors.As(err, &mailIssue) || mailIssue.code != "TB-MAIL-SOURCE-EMPTY" || mailIssue.state != domain.MessageQuarantined {
		t.Fatalf("empty source was not quarantined: %v", err)
	}
	_, _, err = hashSelectedMessage(context.Background(), client, 2, 4, true)
	if !errors.As(err, &mailIssue) || mailIssue.code != "TB-MAIL-SOURCE-SIZE-MISMATCH" {
		t.Fatalf("size mismatch was not quarantined: %v", err)
	}
}

func TestHashSelectedMessagePreservesNonStandardRawBytes(t *testing.T) {
	raw := append([]byte("Subject: =?broken?\r\n\r\n中文"), 0, 1, 2)
	client := &reconciliationClient{raw: map[uint32][]byte{7: raw}}
	digest, size, err := hashSelectedMessage(context.Background(), client, 7, int64(len(raw)), true)
	if err != nil || size != int64(len(raw)) || digest == "" {
		t.Fatalf("non-standard raw message was not hashed byte-exactly: digest=%q size=%d err=%v", digest, size, err)
	}
}

func TestFillMailboxSizesInventoriesUnknownSizes(t *testing.T) {
	client := &reconciliationClient{
		uids: []uint32{1, 2},
		metadata: map[uint32]mailimap.MessageMetadata{
			1: {UID: 1, Size: 10, SizeKnown: true},
			2: {UID: 2, Size: 25, SizeKnown: true},
		},
	}
	mailboxes, err := fillMailboxSizes(context.Background(), client, []domain.Mailbox{{Name: "INBOX", Selectable: true, Messages: 2}})
	if err != nil || len(mailboxes) != 1 || !mailboxes[0].SizeKnown || mailboxes[0].Size != 35 {
		t.Fatalf("unexpected mailbox inventory: %#v, %v", mailboxes, err)
	}
}

func TestInspectInventoriesUnknownMailboxSizes(t *testing.T) {
	client := &reconciliationClient{
		uids:      []uint32{1, 2},
		mailboxes: []domain.Mailbox{{Name: "INBOX", Selectable: true, Messages: 2}},
		metadata: map[uint32]mailimap.MessageMetadata{
			1: {UID: 1, Size: 10, SizeKnown: true},
			2: {UID: 2, Size: 25, SizeKnown: true},
		},
	}
	service := New(nil, staticFactory{client: client}, nil)
	summary, err := service.Inspect(context.Background(), domain.AccountConfig{Host: "mail.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Bytes != 35 || len(summary.Mailboxes) != 1 || !summary.Mailboxes[0].SizeKnown || summary.Mailboxes[0].Size != 35 {
		t.Fatalf("account inspection retained a false zero-byte size: %#v", summary)
	}
}

func TestExactDuplicateIndexPreservesMultiplicityWithoutMessageID(t *testing.T) {
	raw := []byte("Subject: same\r\n\r\nbody")
	destination := &reconciliationClient{
		uids: []uint32{101, 102},
		metadata: map[uint32]mailimap.MessageMetadata{
			101: {UID: 101, Size: int64(len(raw)), SizeKnown: true},
			102: {UID: 102, Size: int64(len(raw)), SizeKnown: true},
		},
		raw: map[uint32][]byte{101: raw, 102: raw},
	}
	source := &reconciliationClient{raw: map[uint32][]byte{1: raw, 2: raw, 3: raw}}
	index, err := newDuplicateIndex(context.Background(), destination, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	meta := mailimap.MessageMetadata{Size: int64(len(raw)), SizeKnown: true}
	first, _, _, found, err := index.findExact(context.Background(), source, destination, 1, meta)
	if err != nil || !found || first != 101 {
		t.Fatalf("first match uid=%d found=%v err=%v", first, found, err)
	}
	second, _, _, found, err := index.findExact(context.Background(), source, destination, 2, meta)
	if err != nil || !found || second != 102 {
		t.Fatalf("second match uid=%d found=%v err=%v", second, found, err)
	}
	_, _, _, found, err = index.findExact(context.Background(), source, destination, 3, meta)
	if err != nil || found {
		t.Fatalf("one target copy was reused twice: found=%v err=%v", found, err)
	}
}

func TestExactDuplicateIndexComparesRawBytesAndFailsClosed(t *testing.T) {
	destinationRaw := []byte("abc")
	destination := &reconciliationClient{uids: []uint32{9}, metadata: map[uint32]mailimap.MessageMetadata{9: {UID: 9, Size: 3, SizeKnown: true}}, raw: map[uint32][]byte{9: destinationRaw}}
	source := &reconciliationClient{raw: map[uint32][]byte{1: []byte("xyz")}}
	index, err := newDuplicateIndex(context.Background(), destination, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, found, err := index.findExact(context.Background(), source, destination, 1, mailimap.MessageMetadata{Size: 3, SizeKnown: true})
	if err != nil || found {
		t.Fatalf("different raw content was deduplicated: found=%v err=%v", found, err)
	}

	brokenDestination := &reconciliationClient{uids: []uint32{10}, metadata: map[uint32]mailimap.MessageMetadata{10: {UID: 10, Size: 3, SizeKnown: true}}, raw: map[uint32][]byte{}}
	index, err = newDuplicateIndex(context.Background(), brokenDestination, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, err = index.findExact(context.Background(), source, brokenDestination, 1, mailimap.MessageMetadata{Size: 3, SizeKnown: true})
	if err == nil {
		t.Fatal("unreadable target candidate did not fail closed")
	}
}

func TestStoredMetadataMatches(t *testing.T) {
	date := time.Now().UTC().Truncate(time.Second)
	record := database.CopiedMessageRecord{MessageID: "<same>", Size: 42, InternalDate: date}
	if !storedMetadataMatches(record, mailimap.MessageMetadata{MessageID: "<same>", Size: 42, InternalDate: date.Add(time.Second)}, true) {
		t.Fatal("matching destination metadata was rejected")
	}
	if storedMetadataMatches(record, mailimap.MessageMetadata{MessageID: "<different>", Size: 42, InternalDate: date}, true) {
		t.Fatal("different Message-ID was accepted")
	}
	if storedMetadataMatches(record, mailimap.MessageMetadata{MessageID: "<same>", Size: 42, InternalDate: date.Add(2 * time.Second)}, true) {
		t.Fatal("different internal date was accepted")
	}
}

func TestReconcileFolderRequeuesDeletedDestinationMessage(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	date := time.Now().UTC().Truncate(time.Second)
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 1, Size: 42, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
		Options:     domain.DefaultTransferOptions(),
		Mode:        "reconcile",
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	folder := folders[0]
	if _, err := db.BeginMessage(ctx, migrationID, folder.ID, 7, 10, 42, "<deleted>", date); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteMessage(ctx, migrationID, folder.ID, 7, 10, 99, 42, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDestinationUIDValidity(ctx, folder.ID, 5); err != nil {
		t.Fatal(err)
	}
	folders, err = db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	folder = folders[0]
	copiedMessages, copiedBytes := &atomic.Int64{}, &atomic.Int64{}
	copiedMessages.Store(1)
	copiedBytes.Store(42)
	client := &reconciliationClient{metadata: map[uint32]mailimap.MessageMetadata{}, candidates: map[string]mailimap.Candidate{}}
	service := New(db, nil, nil)
	if err := service.reconcileFolder(ctx, migrationID, request, folder, 7, 5, []uint32{10}, client, copiedBytes, copiedMessages, &atomic.Int64{}); err != nil {
		t.Fatal(err)
	}
	if copiedMessages.Load() != 0 || copiedBytes.Load() != 0 {
		t.Fatalf("in-memory progress not rolled back: messages=%d bytes=%d", copiedMessages.Load(), copiedBytes.Load())
	}
	unfinished, err := db.UnfinishedUIDs(ctx, migrationID, folder.ID, 7)
	if err != nil || len(unfinished) != 1 || unfinished[0] != 10 {
		t.Fatalf("deleted destination copy was not requeued: %v, %v", unfinished, err)
	}
}

func TestReconcileFolderDetectsSourceDeletionAndRemembersKeep(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	date := time.Now().UTC().Truncate(time.Second)
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "Entwürfe", UIDValidity: 7, Messages: 1, Size: 42, Selectable: true}, DestinationName: "草稿📨", Enabled: true}},
		Options:     domain.DefaultTransferOptions(),
		Mode:        "reconcile",
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, err := db.Folders(ctx, migrationID)
	if err != nil {
		t.Fatal(err)
	}
	folder := folders[0]
	if _, err := db.BeginMessage(ctx, migrationID, folder.ID, 7, 10, 42, "<deleted>", date); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteMessage(ctx, migrationID, folder.ID, 7, 10, 99, 42, "hash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDestinationUIDValidity(ctx, folder.ID, 5); err != nil {
		t.Fatal(err)
	}
	folders, _ = db.Folders(ctx, migrationID)
	client := &reconciliationClient{
		uids:        []uint32{99},
		uidValidity: 5,
		metadata:    map[uint32]mailimap.MessageMetadata{99: {UID: 99, Size: 42, SizeKnown: true, MessageID: "<deleted>", InternalDate: date}},
		summaries:   map[uint32]mailimap.MessageSummary{99: {Subject: "Re: 国际化", From: "sender@example.test"}},
		candidates:  map[string]mailimap.Candidate{},
	}
	service := New(db, nil, nil)
	if err := service.reconcileFolder(ctx, migrationID, request, folders[0], 7, 5, nil, client, &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}); err != nil {
		t.Fatal(err)
	}
	items, err := db.SourceDeletions(ctx, migrationID)
	if err != nil || len(items) != 1 || items[0].DestinationUID != 99 || items[0].Subject != "Re: 国际化" {
		t.Fatalf("source deletion was not detected: %#v, %v", items, err)
	}
	if err := service.ResolveSourceDeletions(ctx, migrationID, domain.AccountConfig{}, domain.DefaultTransferOptions(), []domain.SourceDeletionAction{{ID: items[0].ID, Resolution: domain.SourceDeletionKeep}}); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileFolder(ctx, migrationID, request, folders[0], 7, 5, nil, client, &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}); err != nil {
		t.Fatal(err)
	}
	items, err = db.SourceDeletions(ctx, migrationID)
	if err != nil || len(items) != 0 {
		t.Fatalf("remembered keep decision reopened: %#v, %v", items, err)
	}
	report, err := db.Report(ctx, migrationID)
	if err != nil || report.SourceDeletionsKept != 1 {
		t.Fatalf("keep decision missing from report: %#v, %v", report, err)
	}
}

func TestResolveSourceDeletionsAppliesMixedExplicitActions(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 3, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, _ := db.Folders(ctx, migrationID)
	folder := folders[0]
	date := time.Now().UTC().Truncate(time.Second)
	metadata := make(map[uint32]mailimap.MessageMetadata)
	for index, uid := range []uint32{10, 11, 12} {
		size := int64(40 + index)
		messageID := "<mixed-" + string(rune('a'+index)) + ">"
		if _, err := db.BeginMessage(ctx, migrationID, folder.ID, 7, uid, size, messageID, date); err != nil {
			t.Fatal(err)
		}
		destinationUID := uint32(100 + index)
		if err := db.CompleteMessage(ctx, migrationID, folder.ID, 7, uid, destinationUID, size, ""); err != nil {
			t.Fatal(err)
		}
		metadata[destinationUID] = mailimap.MessageMetadata{UID: destinationUID, Size: size, SizeKnown: true, MessageID: messageID, InternalDate: date}
	}
	if err := db.SetDestinationUIDValidity(ctx, folder.ID, 5); err != nil {
		t.Fatal(err)
	}
	copied, _ := db.CopiedMessages(ctx, migrationID, folder.ID, 7)
	for _, record := range copied {
		if err := db.UpsertSourceDeletion(ctx, migrationID, record.ID, record.DestinationUID, 5, "Betreff", "Absender"); err != nil {
			t.Fatal(err)
		}
	}
	items, _ := db.SourceDeletions(ctx, migrationID)
	client := &mutationClient{reconciliationClient: &reconciliationClient{
		uidValidity: 5,
		uids:        []uint32{100, 101, 102},
		metadata:    metadata,
		mailboxes:   []domain.Mailbox{{Name: "INBOX", Selectable: true}, {Name: "Papierkorb", SpecialUse: "\\Trash", Selectable: true}},
	}}
	service := New(db, destinationFactory{client: client}, nil)
	actions := []domain.SourceDeletionAction{{ID: items[0].ID, Resolution: domain.SourceDeletionKeep}, {ID: items[1].ID, Resolution: domain.SourceDeletionTrash}, {ID: items[2].ID, Resolution: domain.SourceDeletionDelete}}
	if err := service.ResolveSourceDeletions(ctx, migrationID, request.Destination, domain.DefaultTransferOptions(), actions); err != nil {
		t.Fatal(err)
	}
	if len(client.moves) != 1 || client.moves[0] != items[1].DestinationUID || len(client.moveBoxes) != 1 || client.moveBoxes[0] != "Papierkorb" {
		t.Fatalf("unexpected targeted MOVE calls: %v %v", client.moves, client.moveBoxes)
	}
	if len(client.deletes) != 1 || client.deletes[0] != items[2].DestinationUID {
		t.Fatalf("unexpected targeted DELETE calls: %v", client.deletes)
	}
	remaining, err := db.SourceDeletions(ctx, migrationID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("resolved actions stayed pending: %#v, %v", remaining, err)
	}
	report, err := db.Report(ctx, migrationID)
	if err != nil || report.SourceDeletionsKept != 1 || report.SourceDeletionsTrashed != 1 || report.SourceDeletionsDeleted != 1 {
		t.Fatalf("mixed actions missing from report: %#v, %v", report, err)
	}
}

func TestResolveSourceDeletionUsesVerifiedHashWhenTargetOmitsMessageID(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 1, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
	}
	migrationID, err := db.CreateMigration(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	folders, _ := db.Folders(ctx, migrationID)
	raw := []byte("From: sender@example.test\r\nSubject: Test\r\nMessage-ID: <hash-identity>\r\n\r\nBody")
	digest := sha256.Sum256(raw)
	sha := hex.EncodeToString(digest[:])
	date := time.Now().UTC().Truncate(time.Second)
	_, _ = db.BeginMessage(ctx, migrationID, folders[0].ID, 7, 10, int64(len(raw)), "<hash-identity>", date)
	_ = db.CompleteVerifiedMessage(ctx, migrationID, folders[0].ID, 7, 10, 99, int64(len(raw)), sha, sha, "verified")
	_ = db.SetDestinationUIDValidity(ctx, folders[0].ID, 5)
	copied, _ := db.CopiedMessages(ctx, migrationID, folders[0].ID, 7)
	_ = db.UpsertSourceDeletion(ctx, migrationID, copied[0].ID, 99, 5, "Test", "sender@example.test")
	items, _ := db.SourceDeletions(ctx, migrationID)
	client := &mutationClient{reconciliationClient: &reconciliationClient{
		uidValidity: 5,
		uids:        []uint32{99},
		metadata:    map[uint32]mailimap.MessageMetadata{99: {UID: 99, Size: int64(len(raw)), SizeKnown: true}},
		raw:         map[uint32][]byte{99: raw},
		mailboxes:   []domain.Mailbox{{Name: "INBOX", Selectable: true}, {Name: "Papierkorb", SpecialUse: "\\Trash", Selectable: true}},
	}}
	service := New(db, destinationFactory{client: client}, nil)
	if err := service.ResolveSourceDeletions(ctx, migrationID, request.Destination, domain.DefaultTransferOptions(), []domain.SourceDeletionAction{{ID: items[0].ID, Resolution: domain.SourceDeletionTrash}}); err != nil {
		t.Fatal(err)
	}
	if len(client.moves) != 1 || client.moves[0] != 99 {
		t.Fatalf("verified target was not moved: %v", client.moves)
	}
	remaining, err := db.SourceDeletions(ctx, migrationID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("resolved deletion remained visible: %#v, %v", remaining, err)
	}
}

func TestResolveSourceDeletionFailsClosedOnUIDValidityChange(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	request := domain.StartRequest{
		Source:      domain.AccountConfig{Host: "source", Port: 993, Encryption: domain.EncryptionTLS},
		Destination: domain.AccountConfig{Host: "destination", Port: 993, Encryption: domain.EncryptionTLS},
		Mappings:    []domain.FolderMapping{{Source: domain.Mailbox{Name: "INBOX", UIDValidity: 7, Messages: 1, Selectable: true}, DestinationName: "INBOX", Enabled: true}},
	}
	migrationID, _ := db.CreateMigration(ctx, request)
	folders, _ := db.Folders(ctx, migrationID)
	date := time.Now().UTC().Truncate(time.Second)
	_, _ = db.BeginMessage(ctx, migrationID, folders[0].ID, 7, 10, 42, "<identity>", date)
	_ = db.CompleteMessage(ctx, migrationID, folders[0].ID, 7, 10, 99, 42, "")
	copied, _ := db.CopiedMessages(ctx, migrationID, folders[0].ID, 7)
	_ = db.UpsertSourceDeletion(ctx, migrationID, copied[0].ID, 99, 5, "", "")
	items, _ := db.SourceDeletions(ctx, migrationID)
	client := &mutationClient{reconciliationClient: &reconciliationClient{uidValidity: 6, metadata: map[uint32]mailimap.MessageMetadata{99: {UID: 99, Size: 42, SizeKnown: true, MessageID: "<identity>"}}}}
	service := New(db, destinationFactory{client: client}, nil)
	if err := service.ResolveSourceDeletions(ctx, migrationID, request.Destination, domain.DefaultTransferOptions(), []domain.SourceDeletionAction{{ID: items[0].ID, Resolution: domain.SourceDeletionDelete}}); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.SourceDeletions(ctx, migrationID)
	if err != nil || len(remaining) != 1 || remaining[0].LastError == "" {
		t.Fatalf("unsafe deletion did not retain a visible error: %#v, %v", remaining, err)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("delete was issued after unsafe UIDVALIDITY change: %v", client.deletes)
	}
}

func TestInventorySourceKeywordsAggregatesPerFolderCaseInsensitively(t *testing.T) {
	client := &reconciliationClient{keywords: map[string][]mailimap.KeywordCount{
		"INBOX":   {{Name: "Project-X", Messages: 3}, {Name: "$HasNoAttachment", Messages: 2}},
		"Archive": {{Name: "project-x", Messages: 4}},
	}}
	mailboxes := []domain.Mailbox{
		{Name: "INBOX", Selectable: true, Messages: 5},
		{Name: "Archive", Selectable: true, Messages: 4},
		{Name: "Virtual", Selectable: false, Messages: 10},
	}
	keywords, err := inventorySourceKeywords(context.Background(), client, mailboxes)
	if err != nil {
		t.Fatal(err)
	}
	if len(keywords) != 2 || keywords[0].Name != "$HasNoAttachment" || keywords[0].Occurrences["INBOX"] != 2 {
		t.Fatalf("unexpected technical keyword inventory: %#v", keywords)
	}
	if keywords[1].Name != "Project-X" || keywords[1].Occurrences["INBOX"] != 3 || keywords[1].Occurrences["Archive"] != 4 {
		t.Fatalf("unexpected merged keyword inventory: %#v", keywords)
	}
}
