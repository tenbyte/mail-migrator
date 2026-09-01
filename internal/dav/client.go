package dav

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
	"github.com/emersion/go-webdav/carddav"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

type realClient struct {
	kind      domain.ServiceKind
	endpoint  *url.URL
	http      *http.Client
	webdav    *webdav.Client
	caldav    *caldav.Client
	carddav   *carddav.Client
	principal string
	homeSet   string
}

func (RealFactory) Connect(ctx context.Context, kind domain.ServiceKind, endpoint domain.DAVEndpoint, connectTimeout, stallTimeout time.Duration) (Client, error) {
	if kind != domain.ServiceCalendar && kind != domain.ServiceContacts {
		return nil, fmt.Errorf("unsupported DAV service %q", kind)
	}
	endpointURL, err := discoverEndpoint(ctx, kind, endpoint)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(endpointURL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("valid DAV URL is required")
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("DAV connections require HTTPS")
	}
	if endpoint.Username == "" || endpoint.Password == "" {
		return nil, errors.New("DAV username and password are required")
	}
	if connectTimeout <= 0 {
		connectTimeout = 15 * time.Second
	}
	if stallTimeout <= 0 {
		stallTimeout = 90 * time.Second
	}
	dialer := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: parsed.Hostname()},
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: stallTimeout,
		ExpectContinueTimeout: 5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	authMethod := endpoint.AuthMethod
	if authMethod == "" {
		authMethod = domain.DAVAuthAuto
	}
	auth := &authTransport{base: transport, method: authMethod, username: endpoint.Username, password: endpoint.Password, originHost: parsed.Host}
	httpClient := &http.Client{Transport: auth, CheckRedirect: safeRedirect(parsed)}
	wc, err := webdav.NewClient(httpClient, parsed.String())
	if err != nil {
		return nil, err
	}
	client := &realClient{kind: kind, endpoint: parsed, http: httpClient, webdav: wc}
	client.principal, err = wc.FindCurrentUserPrincipal(ctx)
	if err != nil {
		return nil, fmt.Errorf("determine DAV principal: %w", err)
	}
	if kind == domain.ServiceCalendar {
		client.caldav, err = caldav.NewClient(httpClient, parsed.String())
		if err == nil {
			client.homeSet, err = client.caldav.FindCalendarHomeSet(ctx, client.principal)
		}
	} else {
		client.carddav, err = carddav.NewClient(httpClient, parsed.String())
		if err == nil {
			err = client.carddav.HasSupport(ctx)
		}
		if err == nil {
			client.homeSet, err = client.carddav.FindAddressBookHomeSet(ctx, client.principal)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("determine DAV home set: %w", err)
	}
	return client, nil
}

func discoverEndpoint(ctx context.Context, kind domain.ServiceKind, endpoint domain.DAVEndpoint) (string, error) {
	if strings.TrimSpace(endpoint.URL) != "" {
		return strings.TrimSpace(endpoint.URL), nil
	}
	domainName := endpoint.Username
	if index := strings.LastIndex(domainName, "@"); index >= 0 {
		domainName = domainName[index+1:]
	} else {
		return "", errors.New("enter a complete email address for automatic DAV discovery or configure the server address manually")
	}
	if domainName == "" || strings.ContainsAny(domainName, "/: ") {
		return "", errors.New("DAV URL is missing and could not be derived from the username")
	}
	var discovered string
	var err error
	if kind == domain.ServiceCalendar {
		discovered, err = caldav.DiscoverContextURL(ctx, domainName)
	} else {
		discovered, err = carddav.DiscoverContextURL(ctx, domainName)
	}
	if err != nil || discovered == "" {
		wellKnown := "caldav"
		if kind == domain.ServiceContacts {
			wellKnown = "carddav"
		}
		return "https://" + domainName + "/.well-known/" + wellKnown, nil
	}
	return discovered, nil
}

func (c *realClient) Endpoint() string { return c.endpoint.String() }

func (c *realClient) Summary(ctx context.Context) (domain.DAVAccountSummary, error) {
	summary := domain.DAVAccountSummary{Connected: true, Endpoint: c.endpoint.String(), Principal: c.principal, HomeSet: c.homeSet, Kind: c.kind, Verified: verifiedProvider(c.endpoint.Hostname())}
	capabilities, err := c.options(ctx)
	if err != nil {
		return summary, err
	}
	summary.Capabilities = capabilities
	if c.kind == domain.ServiceCalendar {
		collections, err := c.caldav.FindCalendars(ctx, c.homeSet)
		if err != nil {
			return summary, err
		}
		for _, collection := range collections {
			summary.Collections = append(summary.Collections, domain.DAVCollection{Path: collection.Path, Name: fallbackName(collection.Name, collection.Path), Description: collection.Description, Kind: c.kind, Components: nonNil(collection.SupportedComponentSet), ContentTypes: []string{"text/calendar; version=2.0"}, MaxResourceSize: collection.MaxResourceSize})
		}
	} else {
		collections, err := c.carddav.FindAddressBooks(ctx, c.homeSet)
		if err != nil {
			return summary, err
		}
		for _, collection := range collections {
			var contentTypes []string
			for _, dataType := range collection.SupportedAddressData {
				value := dataType.ContentType
				if dataType.Version != "" {
					value += "; version=" + dataType.Version
				}
				contentTypes = append(contentTypes, value)
			}
			summary.Collections = append(summary.Collections, domain.DAVCollection{Path: collection.Path, Name: fallbackName(collection.Name, collection.Path), Description: collection.Description, Kind: c.kind, ContentTypes: nonNil(contentTypes), MaxResourceSize: collection.MaxResourceSize})
		}
	}
	for index := range summary.Collections {
		files, err := c.webdav.ReadDir(ctx, summary.Collections[index].Path, false)
		if err != nil {
			return summary, fmt.Errorf("inventory DAV collection %q: %w", summary.Collections[index].Name, err)
		}
		for _, file := range files {
			if file.IsDir || sameDAVPath(file.Path, summary.Collections[index].Path) {
				continue
			}
			summary.Collections[index].Objects++
			summary.Collections[index].Bytes += file.Size
		}
		properties, propErr := c.collectionProperties(ctx, summary.Collections[index].Path)
		if propErr == nil {
			summary.Collections[index].SyncToken = properties.SyncToken
			summary.Collections[index].QuotaAvailableBytes = properties.QuotaAvailable
			summary.Collections[index].QuotaUsedBytes = properties.QuotaUsed
		}
		summary.Objects += summary.Collections[index].Objects
		summary.Bytes += summary.Collections[index].Bytes
	}
	summary.CollectionCount = len(summary.Collections)
	if !summary.Verified {
		summary.Warnings = append(summary.Warnings, "This DAV server is not yet part of the supported provider test matrix.")
	}
	return summary, nil
}

func (c *realClient) options(ctx context.Context) ([]string, error) {
	request, err := c.request(ctx, "OPTIONS", c.endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.do(request)
	if err != nil {
		return nil, err
	}
	_ = response.Body.Close()
	values := strings.Split(response.Header.Get("DAV"), ",")
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return nonNil(result), nil
}

func (c *realClient) Inventory(ctx context.Context, collectionPath, syncToken string, limit int) (Inventory, error) {
	if syncToken != "" {
		return c.syncInventory(ctx, collectionPath, syncToken, limit)
	}
	files, err := c.webdav.ReadDir(ctx, collectionPath, false)
	if err != nil {
		return Inventory{}, err
	}
	result := Inventory{Resources: make([]ResourceInfo, 0, len(files))}
	for _, file := range files {
		if file.IsDir || sameDAVPath(file.Path, collectionPath) {
			continue
		}
		result.Resources = append(result.Resources, ResourceInfo{Href: file.Path, ETag: normalizeETag(file.ETag), Size: file.Size})
	}
	if properties, err := c.collectionProperties(ctx, collectionPath); err == nil {
		result.SyncToken = properties.SyncToken
	}
	return result, nil
}

func (c *realClient) syncInventory(ctx context.Context, collectionPath, syncToken string, limit int) (Inventory, error) {
	var token bytes.Buffer
	_ = xml.EscapeText(&token, []byte(syncToken))
	limitXML := ""
	if limit > 0 {
		limitXML = fmt.Sprintf("<D:limit><D:nresults>%d</D:nresults></D:limit>", limit)
	}
	body := `<D:sync-collection xmlns:D="DAV:"><D:sync-token>` + token.String() + `</D:sync-token><D:sync-level>1</D:sync-level>` + limitXML + `<D:prop><D:getetag/><D:getcontentlength/></D:prop></D:sync-collection>`
	request, err := c.request(ctx, "REPORT", collectionPath, strings.NewReader(body))
	if err != nil {
		return Inventory{}, err
	}
	request.Header.Set("Depth", "1")
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	response, err := c.do(request)
	if err != nil {
		return Inventory{}, err
	}
	defer response.Body.Close()
	parsed, err := decodeMultiStatus(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return Inventory{}, err
	}
	result := Inventory{SyncToken: parsed.SyncToken, Delta: true}
	for _, item := range parsed.Responses {
		if sameDAVPath(item.Href, collectionPath) {
			continue
		}
		if strings.Contains(item.Status, " 404 ") {
			result.Deleted = append(result.Deleted, item.Href)
			continue
		}
		result.Resources = append(result.Resources, ResourceInfo{Href: item.Href, ETag: normalizeETag(item.Prop.ETag), Size: item.Prop.ContentLength})
	}
	return result, nil
}

func (c *realClient) Get(ctx context.Context, href string, maxSize int64) (Resource, error) {
	request, err := c.request(ctx, http.MethodGet, href, nil)
	if err != nil {
		return Resource{}, err
	}
	response, err := c.do(request)
	if err != nil {
		return Resource{}, err
	}
	if maxSize <= 0 {
		maxSize = DefaultMaxObjectSize
	}
	if response.ContentLength > maxSize {
		_ = response.Body.Close()
		return Resource{}, fmt.Errorf("DAV resource is %d bytes, above the safe limit of %d bytes", response.ContentLength, maxSize)
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return Resource{Href: response.Request.URL.Path, ETag: normalizeETag(response.Header.Get("ETag")), ContentType: contentType, Size: response.ContentLength, Body: &limitedReadCloser{Reader: io.LimitReader(response.Body, maxSize+1), closer: response.Body, limit: maxSize}}, nil
}

func (c *realClient) Put(ctx context.Context, href, contentType string, body io.Reader, size int64, options PutOptions) (ResourceInfo, error) {
	request, err := c.request(ctx, http.MethodPut, href, body)
	if err != nil {
		return ResourceInfo{}, err
	}
	request.Header.Set("Content-Type", contentType)
	if size >= 0 {
		request.ContentLength = size
	}
	if options.IfNoneMatch {
		request.Header.Set("If-None-Match", "*")
	}
	if options.IfMatch != "" {
		request.Header.Set("If-Match", quoteETag(options.IfMatch))
	}
	response, err := c.do(request)
	if err != nil {
		return ResourceInfo{}, err
	}
	_ = response.Body.Close()
	location := response.Header.Get("Location")
	if location == "" {
		location = href
	}
	if parsed, err := url.Parse(location); err == nil && parsed.Path != "" {
		location = parsed.Path
	}
	return ResourceInfo{Href: location, ETag: normalizeETag(response.Header.Get("ETag")), Size: size}, nil
}

func (c *realClient) CreateCollection(ctx context.Context, source domain.DAVCollection, destinationPath, destinationName string) error {
	if destinationPath == "" {
		destinationPath = path.Join(c.homeSet, slug(destinationName)) + "/"
	}
	method := "MKCOL"
	root := "D:mkcol"
	namespaces := `xmlns:D="DAV:"`
	if c.kind == domain.ServiceCalendar {
		method = "MKCALENDAR"
		root = "C:mkcalendar"
		namespaces += ` xmlns:C="urn:ietf:params:xml:ns:caldav"`
	} else {
		namespaces += ` xmlns:CR="urn:ietf:params:xml:ns:carddav"`
	}
	body := "<" + root + " " + namespaces + `><D:set><D:prop><D:resourcetype><D:collection/>`
	if c.kind == domain.ServiceCalendar {
		body += `<C:calendar/>`
	} else {
		body += `<CR:addressbook/>`
	}
	body += `</D:resourcetype><D:displayname>`
	var name bytes.Buffer
	_ = xml.EscapeText(&name, []byte(destinationName))
	body += name.String() + `</D:displayname></D:prop></D:set></` + root + `>`
	request, err := c.request(ctx, method, destinationPath, strings.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	response, err := c.do(request)
	if err != nil {
		httpErr, ok := err.(*HTTPError)
		if ok && httpErr.StatusCode == http.StatusMethodNotAllowed {
			return nil
		}
		return err
	}
	return response.Body.Close()
}

func (c *realClient) Delete(ctx context.Context, href, etag string) error {
	request, err := c.request(ctx, http.MethodDelete, href, nil)
	if err != nil {
		return err
	}
	if etag != "" {
		request.Header.Set("If-Match", quoteETag(etag))
	}
	response, err := c.do(request)
	if err != nil {
		return err
	}
	return response.Body.Close()
}

func (c *realClient) Probe(ctx context.Context, collectionPath string) error {
	suffix := ".vcf"
	contentType := "text/vcard; charset=utf-8"
	body := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:tenbyte-write-probe\r\nFN:Tenbyte Write Probe\r\nEND:VCARD\r\n"
	if c.kind == domain.ServiceCalendar {
		suffix = ".ics"
		contentType = "text/calendar; charset=utf-8"
		body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Tenbyte//Write Probe//DE\r\nBEGIN:VEVENT\r\nUID:tenbyte-write-probe\r\nDTSTAMP:20000101T000000Z\r\nDTSTART:20000101T000000Z\r\nDTEND:20000101T000100Z\r\nSUMMARY:Write Probe\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	}
	href := strings.TrimRight(collectionPath, "/") + "/.tenbyte-write-probe" + suffix
	created, err := c.Put(ctx, href, contentType, strings.NewReader(body), int64(len(body)), PutOptions{IfNoneMatch: true})
	if err != nil {
		return fmt.Errorf("DAV write probe failed: %w", err)
	}
	if err := c.Delete(ctx, created.Href, created.ETag); err != nil {
		return fmt.Errorf("DAV write probe cleanup failed for %s: %w", created.Href, err)
	}
	return nil
}

type davProperties struct {
	SyncToken      string
	QuotaAvailable int64
	QuotaUsed      int64
}

func (c *realClient) collectionProperties(ctx context.Context, href string) (davProperties, error) {
	body := `<D:propfind xmlns:D="DAV:"><D:prop><D:sync-token/><D:quota-available-bytes/><D:quota-used-bytes/></D:prop></D:propfind>`
	request, err := c.request(ctx, "PROPFIND", href, strings.NewReader(body))
	if err != nil {
		return davProperties{}, err
	}
	request.Header.Set("Depth", "0")
	request.Header.Set("Content-Type", "application/xml; charset=utf-8")
	response, err := c.do(request)
	if err != nil {
		return davProperties{}, err
	}
	defer response.Body.Close()
	parsed, err := decodeMultiStatus(io.LimitReader(response.Body, 4<<20))
	if err != nil || len(parsed.Responses) == 0 {
		return davProperties{}, err
	}
	return davProperties{SyncToken: firstNonEmpty(parsed.SyncToken, parsed.Responses[0].Prop.SyncToken), QuotaAvailable: parsed.Responses[0].Prop.QuotaAvailable, QuotaUsed: parsed.Responses[0].Prop.QuotaUsed}, nil
}

func (c *realClient) request(ctx context.Context, method, href string, body io.Reader) (*http.Request, error) {
	target, err := c.resolve(href)
	if err != nil {
		return nil, err
	}
	return http.NewRequestWithContext(ctx, method, target.String(), body)
}

func (c *realClient) resolve(href string) (*url.URL, error) {
	parsed, err := url.Parse(href)
	if err != nil {
		return nil, err
	}
	target := c.endpoint.ResolveReference(parsed)
	if target.Scheme != "https" || !strings.EqualFold(target.Host, c.endpoint.Host) {
		return nil, errors.New("DAV resource URL left the authenticated HTTPS endpoint")
	}
	return target, nil
}

func (c *realClient) do(request *http.Request) (*http.Response, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, nil
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	retryAfter := time.Duration(0)
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
		retryAfter = time.Duration(seconds) * time.Second
	}
	return nil, &HTTPError{Method: request.Method, URL: request.URL.Redacted(), StatusCode: response.StatusCode, Status: response.Status, RetryAfter: retryAfter, Body: strings.TrimSpace(string(body))}
}

type multiStatus struct {
	SyncToken string          `xml:"sync-token"`
	Responses []multiResponse `xml:"response"`
}

type multiResponse struct {
	Href     string     `xml:"href"`
	Status   string     `xml:"status"`
	PropStat []propStat `xml:"propstat"`
	Prop     davProp
}

type propStat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	ETag           string `xml:"getetag"`
	ContentLength  int64  `xml:"getcontentlength"`
	SyncToken      string `xml:"sync-token"`
	QuotaAvailable int64  `xml:"quota-available-bytes"`
	QuotaUsed      int64  `xml:"quota-used-bytes"`
}

func decodeMultiStatus(reader io.Reader) (multiStatus, error) {
	var result multiStatus
	if err := xml.NewDecoder(reader).Decode(&result); err != nil {
		return result, err
	}
	for index := range result.Responses {
		for _, propStat := range result.Responses[index].PropStat {
			if propStat.Status == "" || strings.Contains(propStat.Status, " 200 ") {
				result.Responses[index].Prop = propStat.Prop
				break
			}
		}
	}
	return result, nil
}

type limitedReadCloser struct {
	io.Reader
	closer io.Closer
	limit  int64
	read   int64
}

func (r *limitedReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.read += int64(n)
	if r.read > r.limit {
		return n, errors.New("DAV resource exceeded the safe size limit while reading")
	}
	return n, err
}

func (r *limitedReadCloser) Close() error { return r.closer.Close() }

func fallbackName(name, collectionPath string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return path.Base(strings.TrimRight(collectionPath, "/"))
}

func normalizeETag(etag string) string {
	etag = strings.TrimSpace(etag)
	weak := strings.HasPrefix(strings.ToUpper(etag), "W/")
	if weak {
		etag = strings.TrimSpace(etag[2:])
	}
	etag = strings.Trim(etag, `"`)
	if weak {
		return "W/" + etag
	}
	return etag
}
func quoteETag(etag string) string {
	etag = normalizeETag(etag)
	if strings.HasPrefix(strings.ToUpper(etag), "W/") {
		return `W/"` + etag[2:] + `"`
	}
	return `"` + etag + `"`
}
func sameDAVPath(left, right string) bool {
	return strings.TrimRight(left, "/") == strings.TrimRight(right, "/")
}
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var output strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			output.WriteRune(char)
		} else if output.Len() > 0 && !strings.HasSuffix(output.String(), "-") {
			output.WriteByte('-')
		}
	}
	if output.Len() == 0 {
		return "tenbyte-migration"
	}
	return strings.Trim(output.String(), "-")
}
func verifiedProvider(host string) bool {
	host = strings.ToLower(host)
	return host == "localhost" || host == "127.0.0.1" || strings.HasSuffix(host, ".tenbyte.test")
}
