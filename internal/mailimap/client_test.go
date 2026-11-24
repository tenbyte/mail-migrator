package mailimap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestReadMessageSummaryDecodesCompleteHeader(t *testing.T) {
	raw := "From: =?UTF-8?Q?M=C3=BCller?= <mueller@example.test>\r\n" +
		"Subject: =?UTF-8?Q?Gel=C3=B6schte_Nachricht?=\r\n" +
		"Message-ID: <summary@example.test>\r\n\r\n"
	summary, err := readMessageSummary(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Subject != "Gelöschte Nachricht" || summary.From != "Müller <mueller@example.test>" {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestFilterFlags(t *testing.T) {
	got := filterFlags([]string{"\\Seen", "\\Recent", "Custom", "Unsupported"}, []string{"Custom"}, nil)
	if len(got) != 2 || got[0] != "\\Seen" || got[1] != "Custom" {
		t.Fatalf("unexpected flags: %#v", got)
	}
}

func TestFilterFlagsExcludesSelectedKeywordsButKeepsSystemState(t *testing.T) {
	got := filterFlags([]string{"\\Seen", "$HasNoAttachment", "Project-X"}, []string{"\\*"}, []string{"$hasnoattachment"})
	if !reflect.DeepEqual(got, []string{"\\Seen", "Project-X"}) {
		t.Fatalf("unexpected filtered flags: %#v", got)
	}
}

func TestSourceFacingInterfaceHasNoMutationCommands(t *testing.T) {
	typeOfClient := reflect.TypeOf((*Client)(nil)).Elem()
	for _, forbidden := range []string{"Store", "Move", "Delete", "Expunge", "Copy"} {
		if _, exists := typeOfClient.MethodByName(forbidden); exists {
			t.Fatalf("forbidden source mutation method exposed: %s", forbidden)
		}
	}
}

func TestMailboxEncoderSupportsInternationalNamesWithoutCommandInjection(t *testing.T) {
	rev1Names := []string{"Entwürfe", "客户/归档", "Emoji/📨", "A&B", `A "quoted"`, "Parent/Child", "Parent.Child"}
	for _, name := range rev1Names {
		line := captureSelectCommand(t, "IMAP4rev1", name)
		if strings.Contains(line, "\r") || strings.Contains(line, "\n") {
			t.Fatalf("command for %q contains embedded line break: %q", name, line)
		}
		if strings.Contains(name, "&") && !strings.Contains(line, "&-") {
			t.Fatalf("rev1 ampersand was not modified-UTF-7 escaped: %q", line)
		}
		if strings.ContainsAny(name, "ü客户归档📨") && strings.Contains(line, name) {
			t.Fatalf("rev1 mailbox was sent as raw Unicode: %q", line)
		}
		if strings.Contains(name, `"`) && !strings.Contains(line, `\"`) {
			t.Fatalf("quote was not encoded by the IMAP library: %q", line)
		}
	}

	rev2Name := `Entwürfe/客户 & "📨"`
	line := captureSelectCommand(t, "IMAP4rev2", rev2Name)
	if !strings.Contains(line, "Entwürfe/客户") || !strings.Contains(line, `\"📨\"`) {
		t.Fatalf("rev2 mailbox was not sent as safely quoted UTF-8: %q", line)
	}
}

func TestDualRevisionServerRequiresRev2Enable(t *testing.T) {
	if !needsRev2Enable(imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}}) {
		t.Fatal("dual-revision server must enable IMAP4rev2 before mailbox commands")
	}
	if needsRev2Enable(imap.CapSet{imap.CapIMAP4rev1: {}}) || needsRev2Enable(imap.CapSet{imap.CapIMAP4rev2: {}}) {
		t.Fatal("single-revision server must not send the compatibility ENABLE")
	}
}

func TestSelectMailboxRetriesReadOnlyExamineWithSelect(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	commands := make(chan []string, 1)
	serverError := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			serverError <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		lines := make([]string, 0, 2)
		for index := 0; index < 2; index++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverError <- err
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			lines = append(lines, line)
			tag := strings.Fields(line)[0]
			if index == 0 {
				if _, err := fmt.Fprintf(serverConn, "%s BAD [READ-ONLY] EXAMINE unable to open folder index.\r\n", tag); err != nil {
					serverError <- err
					return
				}
				continue
			}
			if _, err := fmt.Fprintf(serverConn, "* FLAGS ()\r\n* 7 EXISTS\r\n* OK [UIDVALIDITY 42] valid\r\n* OK [UIDNEXT 8] next\r\n%s OK [READ-WRITE] selected\r\n", tag); err != nil {
				serverError <- err
				return
			}
		}
		commands <- lines
	}()

	protocolClient := imapclient.New(clientConn, nil)
	client := &realClient{client: protocolClient}
	defer client.Close()
	uidValidity, uidNext, _, err := client.SelectMailbox(context.Background(), "Parent/Folder ", true)
	if err != nil {
		t.Fatal(err)
	}
	if uidValidity != 42 || uidNext != 8 || client.selectedSize != 7 {
		t.Fatalf("unexpected selection result: uidValidity=%d uidNext=%d messages=%d", uidValidity, uidNext, client.selectedSize)
	}
	select {
	case lines := <-commands:
		if !strings.Contains(lines[0], "EXAMINE \"Parent/Folder \"") || !strings.Contains(lines[1], "SELECT \"Parent/Folder \"") {
			t.Fatalf("unexpected commands: %#v", lines)
		}
	case err := <-serverError:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mailbox commands")
	}
}

func TestMailboxDiscoverySurvivesUnavailableStatistics(t *testing.T) {
	t.Run("rejected status", func(t *testing.T) {
		client := scriptedClient(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapStatusSize: {}}, []imapStep{
			{" LIST ", "* LIST () \"/\" \"INBOX\"\r\n* LIST () \"/\" \"Deleted Items\"\r\n%s OK list complete\r\n"},
			{` STATUS INBOX \(([A-Z]+ )*SIZE( [A-Z]+)*\)`, "%s BAD size unavailable\r\n"},
			{` STATUS INBOX \((MESSAGES|UIDNEXT|UIDVALIDITY)( (MESSAGES|UIDNEXT|UIDVALIDITY))*\)`, "* STATUS INBOX (MESSAGES 2 UIDNEXT 3 UIDVALIDITY 1)\r\n%s OK status complete\r\n"},
			{` STATUS "Deleted Items" \(([A-Z]+ )*SIZE( [A-Z]+)*\)`, "%s NO status unavailable\r\n"},
			{` STATUS "Deleted Items" \((MESSAGES|UIDNEXT|UIDVALIDITY)( (MESSAGES|UIDNEXT|UIDVALIDITY))*\)`, "%s NO status unavailable\r\n"},
		})
		ctx := context.Background()
		mailboxes, err := client.ListMailboxes(ctx)
		if err != nil || len(mailboxes) != 2 {
			t.Fatalf("LIST returned %#v, %v", mailboxes, err)
		}
		client.ReadMailboxStats(ctx, mailboxes)
		inbox, deleted := mailboxes[0], mailboxes[1]
		if !inbox.Selectable || inbox.MessagesUnknown || inbox.Messages != 2 || inbox.SizeKnown {
			t.Fatalf("basic STATUS did not recover message count: %#v", inbox)
		}
		if deleted.Name != "Deleted Items" || !deleted.Selectable || !deleted.MessagesUnknown || deleted.SizeKnown {
			t.Fatalf("failure reading statistics changed folder eligibility: %#v", deleted)
		}
	})

	t.Run("LIST-STATUS disconnects", func(t *testing.T) {
		client := scriptedClient(t, imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapListStatus: {}, imap.CapStatusSize: {}}, []imapStep{
			{" LIST ", "* LIST () \"/\" \"INBOX\"\r\n* LIST () \"/\" \"Deleted Items\"\r\n%s OK list complete\r\n"},
			{`RETURN \(STATUS `, ""},
		})
		ctx := context.Background()
		mailboxes, err := client.ListMailboxes(ctx)
		if err != nil || len(mailboxes) != 2 {
			t.Fatalf("LIST returned %#v, %v", mailboxes, err)
		}
		client.ReadMailboxStats(ctx, mailboxes)
		for _, mailbox := range mailboxes {
			if !mailbox.Selectable || !mailbox.MessagesUnknown || mailbox.SizeKnown {
				t.Fatalf("connection loss during optional LIST-STATUS changed folder eligibility: %#v", mailbox)
			}
		}
	})
}

func TestFetchMetadataRetriesWithoutRejectedSize(t *testing.T) {
	client := scriptedClient(t, imap.CapSet{imap.CapIMAP4rev1: {}}, []imapStep{
		{`RFC822\.SIZE`, "%s NO message size unavailable\r\n"},
		{`\(UID (FLAGS INTERNALDATE|INTERNALDATE FLAGS) BODY\.PEEK\[HEADER\.FIELDS \("Message-ID"\)\]\)`, "* 1 FETCH (UID 5 FLAGS (\\Seen) INTERNALDATE \"01-Jan-2000 00:00:00 +0000\")\r\n%s OK fetch complete\r\n"},
	})
	metadata, err := client.FetchMetadata(context.Background(), 5)
	if err != nil || metadata.UID != 5 || metadata.SizeKnown || metadata.InternalDate.IsZero() || !reflect.DeepEqual(metadata.Flags, []string{"\\Seen"}) {
		t.Fatalf("FETCH without a size returned %#v, %v", metadata, err)
	}
}

type imapStep struct {
	command string
	reply   string
}

func scriptedClient(t *testing.T, caps imap.CapSet, steps []imapStep) *realClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	_ = serverConn.SetDeadline(time.Now().Add(5 * time.Second))
	serverError := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		serverError <- func() error {
			if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
				return err
			}
			reader := bufio.NewReader(serverConn)
			for _, step := range steps {
				line, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				if matched, err := regexp.MatchString(step.command, line); err != nil || !matched {
					return fmt.Errorf("expected %q in command %q", step.command, line)
				}
				if step.reply == "" {
					return nil
				}
				if _, err := fmt.Fprintf(serverConn, step.reply, strings.Fields(line)[0]); err != nil {
					return err
				}
			}
			return nil
		}()
	}()
	client := &realClient{client: imapclient.New(clientConn, nil), capabilities: caps}
	t.Cleanup(func() {
		_ = client.Close()
		if err := <-serverError; err != nil {
			t.Error(err)
		}
	})
	return client
}

func TestListMessageMetadataFetchesOnlyUIDAndSize(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	fetchCommand := make(chan string, 1)
	serverError := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := fmt.Fprint(serverConn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			serverError <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		for index := 0; index < 3; index++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverError <- err
				return
			}
			tag := strings.Fields(line)[0]
			switch index {
			case 0:
				_, err = fmt.Fprintf(serverConn, "* FLAGS ()\r\n* 2 EXISTS\r\n* OK [UIDVALIDITY 42] valid\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case 1:
				_, err = fmt.Fprintf(serverConn, "* SEARCH 1 2\r\n%s OK search complete\r\n", tag)
			case 2:
				fetchCommand <- strings.TrimSpace(line)
				_, err = fmt.Fprintf(serverConn, "* 1 FETCH (UID 1 RFC822.SIZE 10)\r\n* 2 FETCH (UID 2 RFC822.SIZE 25)\r\n%s OK fetch complete\r\n", tag)
			}
			if err != nil {
				serverError <- err
				return
			}
		}
	}()

	protocolClient := imapclient.New(clientConn, nil)
	client := &realClient{client: protocolClient}
	defer client.Close()
	metadata, err := client.ListMessageMetadata(context.Background(), "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 2 || metadata[0].Size != 10 || metadata[1].Size != 25 {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	select {
	case line := <-fetchCommand:
		upper := strings.ToUpper(line)
		if !strings.Contains(upper, "RFC822.SIZE") || strings.Contains(upper, "FLAGS") || strings.Contains(upper, "BODY") || strings.Contains(upper, "HEADER") {
			t.Fatalf("preflight fetch requests unnecessary data: %s", line)
		}
	case err := <-serverError:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for FETCH command")
	}
}

func captureSelectCommand(t *testing.T, capability, mailbox string) string {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	serverResult := make(chan string, 1)
	serverError := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := fmt.Fprintf(serverConn, "* OK [CAPABILITY %s] ready\r\n", capability); err != nil {
			serverError <- err
			return
		}
		reader := bufio.NewReader(serverConn)
		capabilityLine, err := reader.ReadString('\n')
		if err != nil {
			serverError <- err
			return
		}
		capabilityTag := strings.Fields(capabilityLine)[0]
		if _, err := fmt.Fprintf(serverConn, "* CAPABILITY %s\r\n%s OK capabilities\r\n", capability, capabilityTag); err != nil {
			serverError <- err
			return
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			serverError <- err
			return
		}
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		serverResult <- trimmed
		tag := strings.Fields(trimmed)[0]
		_, err = fmt.Fprintf(serverConn, "* FLAGS ()\r\n* 0 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n%s OK [READ-ONLY] selected\r\n", tag)
		if err != nil {
			serverError <- err
		}
	}()
	client := imapclient.New(clientConn, nil)
	defer client.Close()
	if _, err := client.Capability().Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-serverResult:
		return line
	case err := <-serverError:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SELECT command")
	}
	return ""
}
