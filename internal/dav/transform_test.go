package dav

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestCalendarTransformDisablesSchedulingAndPreservesSeriesData(t *testing.T) {
	input := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:series-1\r\nDTSTAMP:20260830T080000Z\r\nDTSTART;TZID=Europe/Berlin:20260830T100000\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\nORGANIZER:mailto:owner@example.test\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.test\r\nSUMMARY:Serie\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:series-1\r\nDTSTAMP:20260830T080000Z\r\nRECURRENCE-ID;TZID=Europe/Berlin:20260906T100000\r\nDTSTART;TZID=Europe/Berlin:20260906T110000\r\nSUMMARY:Ausnahme\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")

	result, err := Transform(domain.ServiceCalendar, input, domain.DAVCollection{})
	if err != nil {
		t.Fatal(err)
	}
	if result.UID != "series-1" {
		t.Fatalf("unexpected UID %q", result.UID)
	}
	output := strings.ToUpper(string(result.Data))
	for _, expected := range []string{"TZID:EUROPE/BERLIN", "RRULE:FREQ=WEEKLY;COUNT=3", "RECURRENCE-ID", "SCHEDULE-AGENT=NONE"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("transformed calendar lost %q:\n%s", expected, output)
		}
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "SCHEDULE_AGENT_NONE" {
		t.Fatalf("unexpected warnings: %#v", result.Warnings)
	}
}

func TestCalendarWithoutSchedulingIsTransferredRaw(t *testing.T) {
	input := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:todo-1\r\nSUMMARY:Aufgabe\r\nEND:VTODO\r\nEND:VCALENDAR\r\n")
	result, err := Transform(domain.ServiceCalendar, input, domain.DAVCollection{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Data, input) {
		t.Fatal("unchanged resources must remain byte-identical")
	}
}

func TestVCardFourToThreeKeepsGroupPhotoAndCustomFields(t *testing.T) {
	input := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:group-1\r\nFN:Team Ü\r\nKIND:group\r\nMEMBER:urn:uuid:person-1\r\nPHOTO:data:image/png;base64,AA==\r\nX-TENBYTE-CUSTOM:erhalten\r\nEND:VCARD\r\n")
	destination := domain.DAVCollection{ContentTypes: []string{"text/vcard; version=3.0"}}
	result, err := Transform(domain.ServiceContacts, input, destination)
	if err != nil {
		t.Fatal(err)
	}
	output := string(result.Data)
	for _, expected := range []string{"VERSION:3.0", "UID:group-1", "KIND:group", "MEMBER:urn:uuid:person-1", "X-TENBYTE-CUSTOM:erhalten"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("converted vCard lost %q:\n%s", expected, output)
		}
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "VCARD_4_TO_3" {
		t.Fatalf("unexpected warnings: %#v", result.Warnings)
	}
}

func TestDAVObjectWithoutUIDFailsInsteadOfBeingCorrupted(t *testing.T) {
	_, err := Transform(domain.ServiceContacts, []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Ohne UID\r\nEND:VCARD\r\n"), domain.DAVCollection{})
	if err == nil || !strings.Contains(err.Error(), "no UID") {
		t.Fatalf("expected a clear UID error, got %v", err)
	}
}
