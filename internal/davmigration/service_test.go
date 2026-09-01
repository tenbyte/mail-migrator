package davmigration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tenbyte/mail-migrator/internal/database"
	"github.com/tenbyte/mail-migrator/internal/dav"
	"github.com/tenbyte/mail-migrator/internal/domain"
)

type fakeFactory struct{ source, destination *fakeClient }

func (f fakeFactory) Connect(_ context.Context, _ domain.ServiceKind, endpoint domain.DAVEndpoint, _, _ time.Duration) (dav.Client, error) {
	if endpoint.URL == "https://source.test/dav" {
		return f.source, nil
	}
	if endpoint.URL == "https://destination.test/dav" {
		return f.destination, nil
	}
	return nil, fmt.Errorf("unknown endpoint %s", endpoint.URL)
}

type fakeObject struct {
	body []byte
	etag string
}

type fakeClient struct {
	mu         sync.Mutex
	endpoint   string
	collection domain.DAVCollection
	objects    map[string]fakeObject
	deleted    []string
	token      int
	deletes    int
}

func (c *fakeClient) Endpoint() string { return c.endpoint }
func (c *fakeClient) Summary(context.Context) (domain.DAVAccountSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	collection := c.collection
	collection.Objects = int64(len(c.objects))
	collection.Bytes = 0
	for _, object := range c.objects {
		collection.Bytes += int64(len(object.body))
	}
	return domain.DAVAccountSummary{Connected: true, Endpoint: c.endpoint, Kind: domain.ServiceCalendar, Collections: []domain.DAVCollection{collection}, CollectionCount: 1, Objects: collection.Objects, Bytes: collection.Bytes, Verified: true}, nil
}
func (c *fakeClient) Inventory(_ context.Context, _ string, syncToken string, _ int) (dav.Inventory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := dav.Inventory{SyncToken: fmt.Sprintf("token-%d", c.token), Delta: syncToken != ""}
	if syncToken != "" {
		result.Deleted = append(result.Deleted, c.deleted...)
		c.deleted = nil
		return result, nil
	}
	for href, object := range c.objects {
		result.Resources = append(result.Resources, dav.ResourceInfo{Href: href, ETag: object.etag, Size: int64(len(object.body))})
	}
	return result, nil
}
func (c *fakeClient) Get(_ context.Context, href string, _ int64) (dav.Resource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	object, ok := c.objects[href]
	if !ok {
		return dav.Resource{}, fmt.Errorf("not found: %s", href)
	}
	return dav.Resource{Href: href, ETag: object.etag, ContentType: "text/calendar", Size: int64(len(object.body)), Body: io.NopCloser(bytes.NewReader(object.body))}, nil
}
func (c *fakeClient) Put(_ context.Context, href, _ string, body io.Reader, _ int64, options dav.PutOptions) (dav.ResourceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists := c.objects[href]
	if options.IfNoneMatch && exists {
		return dav.ResourceInfo{}, &dav.HTTPError{StatusCode: 412, Status: "412 Precondition Failed"}
	}
	if options.IfMatch != "" && (!exists || current.etag != options.IfMatch) {
		return dav.ResourceInfo{}, &dav.HTTPError{StatusCode: 412, Status: "412 Precondition Failed"}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return dav.ResourceInfo{}, err
	}
	c.token++
	etag := fmt.Sprintf("etag-%d", c.token)
	c.objects[href] = fakeObject{body: data, etag: etag}
	return dav.ResourceInfo{Href: href, ETag: etag, Size: int64(len(data))}, nil
}
func (c *fakeClient) CreateCollection(context.Context, domain.DAVCollection, string, string) error {
	return nil
}
func (c *fakeClient) Delete(_ context.Context, href, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objects, href)
	c.deletes++
	return nil
}
func (c *fakeClient) Probe(context.Context, string) error { return nil }

func (c *fakeClient) remove(href string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objects, href)
	c.deleted = append(c.deleted, href)
	c.token++
}

func (c *fakeClient) edit(href string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token++
	c.objects[href] = fakeObject{body: body, etag: fmt.Sprintf("user-edit-%d", c.token)}
}

func TestReconcileRepairsDeletedTargetKeepsSourceDeletionsAndDetectsTargetEdits(t *testing.T) {
	store, err := database.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calendar := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:event-1\r\nDTSTAMP:20260830T080000Z\r\nDTSTART:20260830T100000Z\r\nSUMMARY:Test\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	source := &fakeClient{endpoint: "https://source.test/dav", collection: domain.DAVCollection{Path: "/source/calendar/", Name: "Kalender", Kind: domain.ServiceCalendar}, objects: map[string]fakeObject{"/source/calendar/event-1.ics": {body: calendar, etag: "source-1"}}, token: 1}
	destination := &fakeClient{endpoint: "https://destination.test/dav", collection: domain.DAVCollection{Path: "/destination/calendar/", Name: "Kalender", Kind: domain.ServiceCalendar}, objects: map[string]fakeObject{}}
	request := domain.DAVServiceRequest{Kind: domain.ServiceCalendar, Enabled: true, Source: domain.DAVEndpoint{URL: source.endpoint}, Destination: domain.DAVEndpoint{URL: destination.endpoint}, Mappings: []domain.CollectionMapping{{Source: source.collection, DestinationPath: destination.collection.Path, DestinationName: destination.collection.Name, DestinationExists: true, Enabled: true}}}
	job := domain.StartJobRequest{Calendar: request, Options: domain.DefaultTransferOptions()}
	migrationID, err := store.CreateJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan domain.Progress, 32)
	service := New(store, fakeFactory{source: source, destination: destination}, func(progress domain.Progress) { events <- progress })
	runAndWait(t, service, migrationID, request, "", events)
	targetHref := "/destination/calendar/event-1.ics"
	if _, ok := destination.objects[targetHref]; !ok {
		t.Fatalf("initial object was not copied: %#v", destination.objects)
	}

	destination.remove(targetHref)
	runAndWait(t, service, migrationID, request, "reconcile", events)
	if _, ok := destination.objects[targetHref]; !ok {
		t.Fatal("deleted target copy was not repaired")
	}
	report, err := store.Report(context.Background(), migrationID)
	if err != nil || report.Repaired != 1 {
		t.Fatalf("repair was not reported: %#v, %v", report, err)
	}

	source.remove("/source/calendar/event-1.ics")
	runAndWait(t, service, migrationID, request, "reconcile", events)
	if _, ok := destination.objects[targetHref]; !ok || destination.deletes != 0 {
		t.Fatal("a source deletion must never delete the target copy")
	}

	destination.edit(targetHref, []byte("user edit"))
	runAndWait(t, service, migrationID, request, "reconcile", events)
	conflicts, err := store.Conflicts(context.Background(), migrationID)
	if err != nil || len(conflicts) != 1 || conflicts[0].DestinationETag == "" {
		t.Fatalf("target edit was not recorded as conflict: %#v, %v", conflicts, err)
	}
}

func runAndWait(t *testing.T, service *Service, migrationID int64, request domain.DAVServiceRequest, mode string, events <-chan domain.Progress) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := service.Start(context.Background(), migrationID, request, domain.DefaultTransferOptions(), mode)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	first := true
	lastDone := int64(0)
	sawIndeterminate := false
	for {
		select {
		case progress := <-events:
			if progress.Service != domain.ServiceCalendar {
				continue
			}
			if first {
				first = false
				if progress.RunItemsDone != 0 || !progress.RunIndeterminate {
					t.Fatalf("DAV run did not start at zero/indeterminate: %#v", progress)
				}
			}
			if progress.RunItemsDone < lastDone {
				t.Fatalf("DAV run progress moved backwards: %d after %d", progress.RunItemsDone, lastDone)
			}
			lastDone = progress.RunItemsDone
			sawIndeterminate = sawIndeterminate || progress.RunIndeterminate
			if progress.Service == domain.ServiceCalendar && (progress.State == domain.MigrationCompleted || progress.State == domain.MigrationCompletedWithErrors || progress.State == domain.MigrationFailed) {
				if !sawIndeterminate || progress.RunItemsTotal == 0 || progress.RunItemsDone != progress.RunItemsTotal {
					t.Fatalf("DAV run did not finish with complete current-run counters: %#v", progress)
				}
				time.Sleep(5 * time.Millisecond)
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("DAV migration did not finish")
		}
	}
}
