package dav

import (
	"strings"
	"testing"
)

func TestDecodeSyncMultiStatusIncludesChangedAndDeletedResources(t *testing.T) {
	input := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
<D:response><D:href>/cal/one.ics</D:href><D:propstat><D:prop><D:getetag>"abc"</D:getetag><D:getcontentlength>42</D:getcontentlength></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/cal/deleted.ics</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>
<D:sync-token>https://example.test/token/2</D:sync-token></D:multistatus>`
	result, err := decodeMultiStatus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if result.SyncToken != "https://example.test/token/2" || len(result.Responses) != 2 {
		t.Fatalf("unexpected multistatus: %#v", result)
	}
	if normalizeETag(result.Responses[0].Prop.ETag) != "abc" || result.Responses[0].Prop.ContentLength != 42 {
		t.Fatalf("properties not decoded: %#v", result.Responses[0])
	}
	if !strings.Contains(result.Responses[1].Status, "404") {
		t.Fatalf("deletion status not decoded: %#v", result.Responses[1])
	}
}

func TestAuthParameterParserHandlesQuotedCommas(t *testing.T) {
	params := parseAuthParams(`realm="Calendar, Contacts", nonce="a\\\"b", qop="auth,auth-int", algorithm=SHA-256`)
	if params["realm"] != "Calendar, Contacts" || params["nonce"] != `a\"b` || params["algorithm"] != "SHA-256" {
		t.Fatalf("unexpected parameters: %#v", params)
	}
}

func TestDAVPathAndETagNormalization(t *testing.T) {
	if !sameDAVPath("/dav/calendar/", "/dav/calendar") {
		t.Fatal("equivalent collection paths must match")
	}
	if got := quoteETag(`W/"abc"`); got != `W/"abc"` {
		t.Fatalf("unexpected ETag quoting: %s", got)
	}
}
