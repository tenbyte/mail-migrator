package mailimap

import (
	"bufio"
	"fmt"
	"net"
	"reflect"
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
