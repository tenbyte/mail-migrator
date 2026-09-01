package folders

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tenbyte/mail-migrator/internal/domain"
	"golang.org/x/text/unicode/norm"
)

var specialAttributes = []string{"\\Sent", "\\Drafts", "\\Trash", "\\Junk", "\\Archive", "\\All"}

func SpecialUse(attributes []string) string {
	for _, wanted := range specialAttributes {
		for _, attr := range attributes {
			if strings.EqualFold(attr, wanted) {
				return wanted
			}
		}
	}
	return ""
}

func Recommend(source, destination []domain.Mailbox) []domain.FolderMapping {
	dstDelimiter := destinationDelimiter(destination)
	bySpecial := make(map[string]domain.Mailbox)
	byName := make(map[string]domain.Mailbox)
	for _, mailbox := range destination {
		if !mailbox.Selectable {
			continue
		}
		byName[strings.ToLower(mailbox.Name)] = mailbox
		if mailbox.SpecialUse != "" {
			bySpecial[strings.ToLower(mailbox.SpecialUse)] = mailbox
		}
	}

	result := make([]domain.FolderMapping, 0, len(source))
	for _, src := range source {
		name := TranslateDelimiter(src.Name, src.Delimiter, dstDelimiter)
		exists := false
		if src.SpecialUse != "" {
			if dst, ok := bySpecial[strings.ToLower(src.SpecialUse)]; ok {
				name, exists = dst.Name, true
			}
		}
		if !exists {
			if dst, ok := byName[strings.ToLower(name)]; ok {
				name, exists = dst.Name, true
			}
		}
		result = append(result, domain.FolderMapping{Source: src, DestinationName: name, DestinationDelimiter: dstDelimiter, DestinationExists: exists, Enabled: src.Selectable})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return Depth(result[i].DestinationName, dstDelimiter) < Depth(result[j].DestinationName, dstDelimiter)
	})
	return result
}

func TranslateDelimiter(name string, source, destination rune) string {
	if source == 0 || destination == 0 || source == destination {
		return name
	}
	return strings.ReplaceAll(name, string(source), string(destination))
}

func Depth(name string, delimiter rune) int {
	if delimiter == 0 {
		return 1
	}
	return strings.Count(name, string(delimiter)) + 1
}

func destinationDelimiter(mailboxes []domain.Mailbox) rune {
	for _, mailbox := range mailboxes {
		if mailbox.Delimiter != 0 {
			return mailbox.Delimiter
		}
	}
	return '/'
}

func SafeName(name string) bool {
	return name != "" && utf8.ValidString(name) && !strings.ContainsAny(name, "\x00\r\n")
}

func NormalizeName(name string) string { return norm.NFC.String(name) }
