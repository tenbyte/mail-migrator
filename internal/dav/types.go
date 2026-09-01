package dav

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/tenbyte/mail-migrator/internal/domain"
)

const DefaultMaxObjectSize int64 = 512 << 20

type ResourceInfo struct {
	Href string
	ETag string
	Size int64
}

type Inventory struct {
	Resources []ResourceInfo
	Deleted   []string
	SyncToken string
	Delta     bool
}

type Resource struct {
	Href        string
	ETag        string
	ContentType string
	Size        int64
	Body        io.ReadCloser
}

type PutOptions struct {
	IfMatch     string
	IfNoneMatch bool
}

type Client interface {
	Summary(context.Context) (domain.DAVAccountSummary, error)
	Inventory(context.Context, string, string, int) (Inventory, error)
	Get(context.Context, string, int64) (Resource, error)
	Put(context.Context, string, string, io.Reader, int64, PutOptions) (ResourceInfo, error)
	CreateCollection(context.Context, domain.DAVCollection, string, string) error
	Delete(context.Context, string, string) error
	Probe(context.Context, string) error
	Endpoint() string
}

type Factory interface {
	Connect(context.Context, domain.ServiceKind, domain.DAVEndpoint, time.Duration, time.Duration) (Client, error)
}

type RealFactory struct{}

type HTTPError struct {
	Method     string
	URL        string
	StatusCode int
	Status     string
	RetryAfter time.Duration
	Body       string
}

func (e *HTTPError) Error() string {
	switch e.StatusCode {
	case 401:
		return "[TB-DAV-AUTH-001] Authentication was rejected. Check the username, password, or app password."
	case 403:
		return "[TB-DAV-PERM-001] The DAV server rejected this operation. Check account sharing and write permissions."
	case 404:
		return "[TB-DAV-PATH-001] The DAV resource was not found. Run discovery again or check the manual URL."
	case 409:
		return "[TB-DAV-CONFLICT-001] The server reported a resource conflict. Check the collection mapping and server state."
	case 412:
		return "[TB-DAV-PRECONDITION-001] The destination changed between inspection and write; it will not be overwritten."
	case 429:
		return "[TB-DAV-RATE-001] The server is rate limiting requests. The operation follows Retry-After before trying again."
	case 507:
		return "[TB-DAV-QUOTA-001] The destination reported insufficient storage. Free quota or use another destination account."
	default:
		if e.StatusCode >= 500 {
			return fmt.Sprintf("[TB-DAV-SERVER-001] The DAV server is temporarily unavailable (HTTP %d).", e.StatusCode)
		}
		return fmt.Sprintf("[TB-DAV-HTTP-001] The DAV server rejected the operation (HTTP %d).", e.StatusCode)
	}
}

func IsPreconditionFailed(err error) bool {
	httpErr, ok := err.(*HTTPError)
	return ok && (httpErr.StatusCode == 409 || httpErr.StatusCode == 412)
}

func IsSyncTokenInvalid(err error) bool {
	httpErr, ok := err.(*HTTPError)
	return ok && (httpErr.StatusCode == 403 || httpErr.StatusCode == 409 || httpErr.StatusCode == 410)
}
