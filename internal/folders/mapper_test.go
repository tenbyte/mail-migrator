package folders

import (
	"testing"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

func TestRecommendUsesSpecialUseAndDelimiter(t *testing.T) {
	src := []domain.Mailbox{
		{Name: "Sent", Delimiter: '/', SpecialUse: "\\Sent", Selectable: true},
		{Name: "Kunden/Apple", Delimiter: '/', Selectable: true},
	}
	dst := []domain.Mailbox{{Name: "Gesendet", Delimiter: '.', SpecialUse: "\\Sent", Selectable: true}}
	got := Recommend(src, dst)
	if got[0].DestinationName != "Gesendet" || !got[0].DestinationExists {
		t.Fatalf("special mapping failed: %#v", got[0])
	}
	if got[1].DestinationName != "Kunden.Apple" {
		t.Fatalf("delimiter mapping failed: %#v", got[1])
	}
}

func TestParentBeforeChild(t *testing.T) {
	src := []domain.Mailbox{{Name: "A/B/C", Delimiter: '/', Selectable: true}, {Name: "A", Delimiter: '/', Selectable: true}}
	got := Recommend(src, nil)
	if got[0].DestinationName != "A" {
		t.Fatalf("parent not first: %#v", got)
	}
}

func TestRecommendDoesNotUseNonSelectableDestination(t *testing.T) {
	src := []domain.Mailbox{{Name: "Archive", Delimiter: '/', Selectable: true}}
	dst := []domain.Mailbox{{Name: "Archive", Delimiter: '/', Selectable: false}}
	got := Recommend(src, dst)
	if got[0].DestinationExists {
		t.Fatalf("non-selectable destination was recommended: %#v", got[0])
	}
}

func TestUnsafeNames(t *testing.T) {
	for _, name := range []string{"../../Users/test", "Entwürfe", "客户/归档", "Emoji/📨", `A&B "quoted"`, "Team.Projekt"} {
		if !SafeName(name) {
			t.Fatalf("valid IMAP name was rejected: %q", name)
		}
	}
	for _, name := range []string{"bad\x00name", "bad\rname", "bad\nname", string([]byte{0xff, 0xfe})} {
		if SafeName(name) {
			t.Fatalf("unsafe IMAP name was accepted: %q", name)
		}
	}
}

func TestNormalizeNameUsesNFC(t *testing.T) {
	decomposed := "Entwu\u0308rfe"
	if normalized := NormalizeName(decomposed); normalized != "Entwürfe" {
		t.Fatalf("unexpected normalized mailbox: %q", normalized)
	}
}
