package dav

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-vcard"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

type TransformWarning struct {
	Code    string
	Message string
}

type TransformResult struct {
	Data        []byte
	UID         string
	ContentType string
	Warnings    []TransformWarning
}

func Transform(kind domain.ServiceKind, data []byte, destination domain.DAVCollection) (TransformResult, error) {
	switch kind {
	case domain.ServiceCalendar:
		return transformCalendar(data)
	case domain.ServiceContacts:
		return transformContact(data, destination)
	default:
		return TransformResult{}, fmt.Errorf("unsupported DAV object kind %q", kind)
	}
}

func transformCalendar(data []byte) (TransformResult, error) {
	calendar, err := ical.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return TransformResult{}, fmt.Errorf("invalid iCalendar resource: %w", err)
	}
	result := TransformResult{Data: data, ContentType: "text/calendar; charset=utf-8"}
	changed := false
	var visit func(*ical.Component)
	visit = func(component *ical.Component) {
		if result.UID == "" {
			result.UID, _ = component.Props.Text(ical.PropUID)
		}
		for _, name := range []string{ical.PropOrganizer, ical.PropAttendee} {
			for _, property := range component.Props[name] {
				if !strings.EqualFold(property.Params.Get("SCHEDULE-AGENT"), "NONE") {
					property.Params.Set("SCHEDULE-AGENT", "NONE")
					changed = true
				}
			}
		}
		for _, child := range component.Children {
			visit(child)
		}
	}
	visit(calendar.Component)
	if changed {
		var encoded bytes.Buffer
		if err := ical.NewEncoder(&encoded).Encode(calendar); err != nil {
			return TransformResult{}, fmt.Errorf("encode scheduling-safe calendar resource: %w", err)
		}
		result.Data = encoded.Bytes()
		result.Warnings = append(result.Warnings, TransformWarning{Code: "SCHEDULE_AGENT_NONE", Message: "Calendar notifications were disabled with SCHEDULE-AGENT=NONE."})
	}
	if result.UID == "" {
		return TransformResult{}, fmt.Errorf("calendar resource has no UID")
	}
	return result, nil
}

func transformContact(data []byte, destination domain.DAVCollection) (TransformResult, error) {
	card, err := vcard.NewDecoder(bytes.NewReader(data)).Decode()
	if err != nil {
		return TransformResult{}, fmt.Errorf("invalid vCard resource: %w", err)
	}
	result := TransformResult{Data: data, UID: card.Value(vcard.FieldUID), ContentType: "text/vcard; charset=utf-8"}
	if result.UID == "" {
		return TransformResult{}, fmt.Errorf("vCard resource has no UID")
	}
	version := card.Value(vcard.FieldVersion)
	if version == "" {
		version = "3.0"
	}
	if version == "4.0" && destinationOnlySupportsV3(destination.ContentTypes) {
		card.SetValue(vcard.FieldVersion, "3.0")
		var encoded bytes.Buffer
		if err := vcard.NewEncoder(&encoded).Encode(card); err != nil {
			return TransformResult{}, fmt.Errorf("convert vCard 4.0 to 3.0: %w", err)
		}
		result.Data = encoded.Bytes()
		result.Warnings = append(result.Warnings, TransformWarning{Code: "VCARD_4_TO_3", Message: "The vCard was converted from version 4.0 to 3.0 for the destination."})
	}
	return result, nil
}

func destinationOnlySupportsV3(contentTypes []string) bool {
	if len(contentTypes) == 0 {
		return false
	}
	hasV3, hasV4 := false, false
	for _, contentType := range contentTypes {
		lower := strings.ToLower(contentType)
		hasV3 = hasV3 || strings.Contains(lower, "version=3.0")
		hasV4 = hasV4 || strings.Contains(lower, "version=4.0")
	}
	return hasV3 && !hasV4
}
