package domain

import "time"

type ServiceKind string

const (
	ServiceMail     ServiceKind = "mail"
	ServiceCalendar ServiceKind = "calendar"
	ServiceContacts ServiceKind = "contacts"
)

type UpdateInfo struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

type Encryption string

const (
	EncryptionTLS      Encryption = "tls"
	EncryptionStartTLS Encryption = "starttls"
)

type AccountConfig struct {
	Host               string     `json:"host"`
	Port               int        `json:"port"`
	Encryption         Encryption `json:"encryption"`
	Username           string     `json:"username"`
	Password           string     `json:"password,omitempty"`
	RememberCredential bool       `json:"rememberCredential"`
	CredentialID       string     `json:"credentialId,omitempty"`
}

type Mailbox struct {
	Name        string   `json:"name"`
	Delimiter   rune     `json:"delimiter"`
	Attributes  []string `json:"attributes"`
	SpecialUse  string   `json:"specialUse,omitempty"`
	Selectable  bool     `json:"selectable"`
	Messages    uint32   `json:"messages"`
	UIDValidity uint32   `json:"uidValidity"`
	UIDNext     uint32   `json:"uidNext"`
	Size        int64    `json:"size"`
	SizeKnown   bool     `json:"sizeKnown"`
}

type FolderMapping struct {
	Source               Mailbox `json:"source"`
	DestinationName      string  `json:"destinationName"`
	DestinationDelimiter rune    `json:"destinationDelimiter"`
	DestinationExists    bool    `json:"destinationExists"`
	Enabled              bool    `json:"enabled"`
}

type ServerSummary struct {
	Connected           bool      `json:"connected"`
	Host                string    `json:"host"`
	Capabilities        []string  `json:"capabilities"`
	Mailboxes           []Mailbox `json:"mailboxes"`
	FolderCount         int       `json:"folderCount"`
	Messages            int64     `json:"messages"`
	Bytes               int64     `json:"bytes"`
	UIDPlus             bool      `json:"uidPlus"`
	AppendLimit         int64     `json:"appendLimit,omitempty"`
	QuotaAvailableBytes int64     `json:"quotaAvailableBytes,omitempty"`
	QuotaUsedBytes      int64     `json:"quotaUsedBytes,omitempty"`
	Warnings            []string  `json:"warnings"`
}

type PreflightResult struct {
	Source      ServerSummary   `json:"source"`
	Destination ServerSummary   `json:"destination"`
	Mappings    []FolderMapping `json:"mappings"`
	Keywords    []MailKeyword   `json:"keywords"`
	Warnings    []string        `json:"warnings"`
}

// MailKeyword is a custom IMAP keyword found on source messages. Standard
// system flags (for example \Seen or \Answered) are intentionally not exposed
// as tags and remain controlled by PreserveFlags.
type MailKeyword struct {
	Name        string           `json:"name"`
	Occurrences map[string]int64 `json:"occurrences"`
}

type MigrationState string

const (
	MigrationCreated             MigrationState = "CREATED"
	MigrationPreflight           MigrationState = "PREFLIGHT"
	MigrationReady               MigrationState = "READY"
	MigrationRunning             MigrationState = "RUNNING"
	MigrationPaused              MigrationState = "PAUSED"
	MigrationInterrupted         MigrationState = "INTERRUPTED"
	MigrationCompleted           MigrationState = "COMPLETED"
	MigrationCompletedWithErrors MigrationState = "COMPLETED_WITH_ERRORS"
	MigrationFailed              MigrationState = "FAILED"
	MigrationCancelled           MigrationState = "CANCELLED"
)

type MessageState string

const (
	MessagePending      MessageState = "PENDING"
	MessageTransferring MessageState = "TRANSFERRING"
	MessageCopied       MessageState = "COPIED"
	MessageRetryPending MessageState = "RETRY_PENDING"
	MessageUnknown      MessageState = "UNKNOWN"
	MessageVerifying    MessageState = "VERIFYING"
	MessageQuarantined  MessageState = "QUARANTINED"
	MessageFailed       MessageState = "FAILED"
	MessageSkipped      MessageState = "SKIPPED"
)

type VerificationMode string

const (
	VerificationFullHash VerificationMode = "full_hash"
	VerificationMetadata VerificationMode = "metadata"
)

type TransferOptions struct {
	Concurrency         int              `json:"concurrency"`
	MaximumRetries      int              `json:"maximumRetries"`
	ConnectionTimeout   int              `json:"connectionTimeout"`
	StallTimeout        int              `json:"stallTimeout"`
	PreserveFlags       bool             `json:"preserveFlags"`
	ExcludedKeywords    []string         `json:"excludedKeywords,omitempty"`
	PreserveDate        bool             `json:"preserveDate"`
	DuplicateProtection bool             `json:"duplicateProtection"`
	VerifyAfter         bool             `json:"verifyAfter"`
	VerificationMode    VerificationMode `json:"verificationMode,omitempty"`
}

func DefaultTransferOptions() TransferOptions {
	return TransferOptions{Concurrency: 2, MaximumRetries: 8, ConnectionTimeout: 15, StallTimeout: 90, PreserveFlags: true, PreserveDate: true, DuplicateProtection: true, VerifyAfter: true, VerificationMode: VerificationFullHash}
}

type StartRequest struct {
	Source      AccountConfig   `json:"source"`
	Destination AccountConfig   `json:"destination"`
	Mappings    []FolderMapping `json:"mappings"`
	Options     TransferOptions `json:"options"`
	MigrationID int64           `json:"migrationId,omitempty"`
	Mode        string          `json:"mode,omitempty"`
}

// ResumeRequest contains credentials supplied by the currently visible
// connection form. Only the passwords and identity fields are used; folder
// mappings continue to come from the persisted migration.
type ResumeRequest struct {
	MigrationID int64           `json:"migrationId"`
	Source      AccountConfig   `json:"source"`
	Destination AccountConfig   `json:"destination"`
	Options     TransferOptions `json:"options"`
}

type Progress struct {
	MigrationID      int64             `json:"migrationId"`
	Service          ServiceKind       `json:"service,omitempty"`
	State            MigrationState    `json:"state"`
	CurrentFolder    string            `json:"currentFolder"`
	CurrentUID       uint32            `json:"currentUid"`
	MessagesTotal    int64             `json:"messagesTotal"`
	MessagesCopied   int64             `json:"messagesCopied"`
	MessagesFailed   int64             `json:"messagesFailed"`
	BytesTotal       int64             `json:"bytesTotal"`
	BytesCopied      int64             `json:"bytesCopied"`
	BytesPerSecond   float64           `json:"bytesPerSecond"`
	StartedAt        time.Time         `json:"startedAt"`
	LastError        string            `json:"lastError,omitempty"`
	RunMode          string            `json:"runMode,omitempty"`
	RunPhase         string            `json:"runPhase,omitempty"`
	RunItemsTotal    int64             `json:"runItemsTotal,omitempty"`
	RunItemsDone     int64             `json:"runItemsDone,omitempty"`
	RunIndeterminate bool              `json:"runIndeterminate,omitempty"`
	Services         []ServiceProgress `json:"services,omitempty"`
}

type ServiceProgress struct {
	Kind               ServiceKind    `json:"kind"`
	State              MigrationState `json:"state"`
	Current            string         `json:"current"`
	ItemsTotal         int64          `json:"itemsTotal"`
	ItemsDone          int64          `json:"itemsDone"`
	ItemsFailed        int64          `json:"itemsFailed"`
	ItemsVerified      int64          `json:"itemsVerified,omitempty"`
	ItemsQuarantined   int64          `json:"itemsQuarantined,omitempty"`
	ItemsUnknown       int64          `json:"itemsUnknown,omitempty"`
	ItemsSkipped       int64          `json:"itemsSkipped,omitempty"`
	ItemsDeduplicated  int64          `json:"itemsDeduplicated,omitempty"`
	VerificationFailed int64          `json:"verificationFailed,omitempty"`
	BytesTotal         int64          `json:"bytesTotal"`
	BytesDone          int64          `json:"bytesDone"`
	LastError          string         `json:"lastError,omitempty"`
	RunMode            string         `json:"runMode,omitempty"`
	RunPhase           string         `json:"runPhase,omitempty"`
	RunItemsTotal      int64          `json:"runItemsTotal,omitempty"`
	RunItemsDone       int64          `json:"runItemsDone,omitempty"`
	RunIndeterminate   bool           `json:"runIndeterminate,omitempty"`
}

type RecentMigration struct {
	ID                  int64          `json:"id"`
	CreatedAt           time.Time      `json:"createdAt"`
	FinishedAt          *time.Time     `json:"finishedAt,omitempty"`
	State               MigrationState `json:"state"`
	SourceHost          string         `json:"sourceHost"`
	DestinationHost     string         `json:"destinationHost"`
	SourceUsername      string         `json:"sourceUsername"`
	DestinationUsername string         `json:"destinationUsername"`
	MessagesTotal       int64          `json:"messagesTotal"`
	MessagesCopied      int64          `json:"messagesCopied"`
	MessagesFailed      int64          `json:"messagesFailed"`
	BytesTotal          int64          `json:"bytesTotal"`
	BytesCopied         int64          `json:"bytesCopied"`
	Services            []ServiceKind  `json:"services,omitempty"`
}

type Report struct {
	Migration              RecentMigration   `json:"migration"`
	Folders                int64             `json:"folders"`
	Warnings               int64             `json:"warnings"`
	Errors                 int64             `json:"errors"`
	Verification           string            `json:"verification"`
	Services               []ServiceProgress `json:"services,omitempty"`
	Updated                int64             `json:"updated"`
	Converted              int64             `json:"converted"`
	Skipped                int64             `json:"skipped"`
	Conflicts              int64             `json:"conflicts"`
	Repaired               int64             `json:"repaired"`
	Verified               int64             `json:"verified"`
	Quarantined            int64             `json:"quarantined"`
	Unknown                int64             `json:"unknown"`
	VerificationFailed     int64             `json:"verificationFailed"`
	Deduplicated           int64             `json:"deduplicated"`
	WarningDetails         []ReportEvent     `json:"warningDetails"`
	ErrorDetails           []ReportEvent     `json:"errorDetails"`
	MailIssues             []MailIssue       `json:"mailIssues,omitempty"`
	SourceDeletionsKept    int64             `json:"sourceDeletionsKept"`
	SourceDeletionsTrashed int64             `json:"sourceDeletionsTrashed"`
	SourceDeletionsDeleted int64             `json:"sourceDeletionsDeleted"`
	SourceDeletionErrors   int64             `json:"sourceDeletionErrors"`
}

type ReportEvent struct {
	Folder    string    `json:"folder,omitempty"`
	SourceUID uint32    `json:"sourceUid,omitempty"`
	Level     string    `json:"level"`
	Code      string    `json:"code"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type CredentialRequirement struct {
	Kind                 ServiceKind `json:"kind"`
	SourceAvailable      bool        `json:"sourceAvailable"`
	DestinationAvailable bool        `json:"destinationAvailable"`
}

type ResumeRequirements struct {
	Migration   RecentMigration         `json:"migration"`
	Credentials []CredentialRequirement `json:"credentials"`
}

type ResumeCredentialInput struct {
	SourcePassword      string `json:"sourcePassword,omitempty"`
	DestinationPassword string `json:"destinationPassword,omitempty"`
}

type ResumeJobRequest struct {
	MigrationID            int64                                 `json:"migrationId"`
	Credentials            map[ServiceKind]ResumeCredentialInput `json:"credentials"`
	RememberNewCredentials bool                                  `json:"rememberNewCredentials"`
}

type MailIssueResolution string

const (
	MailIssueTransferAnyway MailIssueResolution = "transfer_anyway"
	MailIssueRetry          MailIssueResolution = "retry"
	MailIssueVerifyAgain    MailIssueResolution = "verify_again"
	MailIssueKeepSkipped    MailIssueResolution = "keep_skipped"
)

type MailIssue struct {
	ID             int64                 `json:"id"`
	MigrationID    int64                 `json:"migrationId"`
	Folder         string                `json:"folder"`
	SourceUID      uint32                `json:"sourceUid"`
	Size           int64                 `json:"size"`
	State          MessageState          `json:"state"`
	ErrorCode      string                `json:"errorCode"`
	Message        string                `json:"message"`
	Verification   string                `json:"verification"`
	AllowedActions []MailIssueResolution `json:"allowedActions"`
}

type SourceDeletionResolution string

const (
	SourceDeletionKeep   SourceDeletionResolution = "keep"
	SourceDeletionTrash  SourceDeletionResolution = "trash"
	SourceDeletionDelete SourceDeletionResolution = "delete"
)

type SourceDeletion struct {
	ID                int64                    `json:"id"`
	MigrationID       int64                    `json:"migrationId"`
	Folder            string                   `json:"folder"`
	DestinationFolder string                   `json:"destinationFolder"`
	SourceUID         uint32                   `json:"sourceUid"`
	DestinationUID    uint32                   `json:"destinationUid"`
	Subject           string                   `json:"subject"`
	From              string                   `json:"from"`
	InternalDate      time.Time                `json:"internalDate"`
	Size              int64                    `json:"size"`
	Resolution        SourceDeletionResolution `json:"resolution,omitempty"`
	Status            string                   `json:"status"`
	LastError         string                   `json:"lastError,omitempty"`
	DetectedAt        time.Time                `json:"detectedAt"`
	UpdatedAt         time.Time                `json:"updatedAt"`
	ResolvedAt        *time.Time               `json:"resolvedAt,omitempty"`
}

type SourceDeletionAction struct {
	ID         int64                    `json:"id"`
	Resolution SourceDeletionResolution `json:"resolution"`
}

type ResolveSourceDeletionsRequest struct {
	MigrationID int64                  `json:"migrationId"`
	Actions     []SourceDeletionAction `json:"actions"`
}

type DAVAuthMethod string

const (
	DAVAuthAuto   DAVAuthMethod = "auto"
	DAVAuthBasic  DAVAuthMethod = "basic"
	DAVAuthDigest DAVAuthMethod = "digest"
)

type DAVEndpoint struct {
	URL                string        `json:"url"`
	Username           string        `json:"username"`
	Password           string        `json:"password,omitempty"`
	AuthMethod         DAVAuthMethod `json:"authMethod"`
	RememberCredential bool          `json:"rememberCredential"`
	CredentialID       string        `json:"credentialId,omitempty"`
}

type DAVCollection struct {
	Path                string      `json:"path"`
	Name                string      `json:"name"`
	Description         string      `json:"description,omitempty"`
	Kind                ServiceKind `json:"kind"`
	Components          []string    `json:"components"`
	ContentTypes        []string    `json:"contentTypes"`
	Objects             int64       `json:"objects"`
	Bytes               int64       `json:"bytes"`
	MaxResourceSize     int64       `json:"maxResourceSize,omitempty"`
	QuotaAvailableBytes int64       `json:"quotaAvailableBytes,omitempty"`
	QuotaUsedBytes      int64       `json:"quotaUsedBytes,omitempty"`
	SyncToken           string      `json:"syncToken,omitempty"`
}

type CollectionMapping struct {
	Source            DAVCollection `json:"source"`
	DestinationPath   string        `json:"destinationPath"`
	DestinationName   string        `json:"destinationName"`
	DestinationExists bool          `json:"destinationExists"`
	Enabled           bool          `json:"enabled"`
}

type DAVAccountSummary struct {
	Connected       bool            `json:"connected"`
	Endpoint        string          `json:"endpoint"`
	Principal       string          `json:"principal"`
	HomeSet         string          `json:"homeSet"`
	Kind            ServiceKind     `json:"kind"`
	Collections     []DAVCollection `json:"collections"`
	Capabilities    []string        `json:"capabilities"`
	CollectionCount int             `json:"collectionCount"`
	Objects         int64           `json:"objects"`
	Bytes           int64           `json:"bytes"`
	Verified        bool            `json:"verified"`
	Warnings        []string        `json:"warnings"`
}

type DAVPreflightResult struct {
	Kind               ServiceKind         `json:"kind"`
	Source             DAVAccountSummary   `json:"source"`
	Destination        DAVAccountSummary   `json:"destination"`
	Mappings           []CollectionMapping `json:"mappings"`
	ObjectsScanned     int64               `json:"objectsScanned"`
	Conversions        int64               `json:"conversions"`
	PotentialConflicts int64               `json:"potentialConflicts"`
	Problems           []ConversionWarning `json:"problems"`
	Warnings           []string            `json:"warnings"`
}

type DAVServiceRequest struct {
	Kind        ServiceKind         `json:"kind"`
	Enabled     bool                `json:"enabled"`
	Source      DAVEndpoint         `json:"source"`
	Destination DAVEndpoint         `json:"destination"`
	Mappings    []CollectionMapping `json:"mappings"`
}

type JobPreflightRequest struct {
	MailEnabled     bool              `json:"mailEnabled"`
	MailSource      AccountConfig     `json:"mailSource"`
	MailDestination AccountConfig     `json:"mailDestination"`
	Calendar        DAVServiceRequest `json:"calendar"`
	Contacts        DAVServiceRequest `json:"contacts"`
}

type JobPreflightResult struct {
	Mail     *PreflightResult    `json:"mail,omitempty"`
	Calendar *DAVPreflightResult `json:"calendar,omitempty"`
	Contacts *DAVPreflightResult `json:"contacts,omitempty"`
	Warnings []string            `json:"warnings"`
}

type StartJobRequest struct {
	MailEnabled     bool              `json:"mailEnabled"`
	MailSource      AccountConfig     `json:"mailSource"`
	MailDestination AccountConfig     `json:"mailDestination"`
	MailMappings    []FolderMapping   `json:"mailMappings"`
	Calendar        DAVServiceRequest `json:"calendar"`
	Contacts        DAVServiceRequest `json:"contacts"`
	Options         TransferOptions   `json:"options"`
	MigrationID     int64             `json:"migrationId,omitempty"`
	Mode            string            `json:"mode,omitempty"`
}

type DAVResourceState struct {
	ID              int64        `json:"id"`
	MigrationID     int64        `json:"migrationId"`
	CollectionID    int64        `json:"collectionId"`
	Kind            ServiceKind  `json:"kind"`
	SourceHref      string       `json:"sourceHref"`
	SourceUID       string       `json:"sourceUid"`
	SourceETag      string       `json:"sourceEtag"`
	SourceHash      string       `json:"sourceHash"`
	DestinationHref string       `json:"destinationHref"`
	DestinationETag string       `json:"destinationEtag"`
	Size            int64        `json:"size"`
	State           MessageState `json:"state"`
	LastError       string       `json:"lastError,omitempty"`
}

type ConversionWarning struct {
	ResourceHref string      `json:"resourceHref"`
	Kind         ServiceKind `json:"kind"`
	Code         string      `json:"code"`
	Message      string      `json:"message"`
}

type Conflict struct {
	ID              int64       `json:"id"`
	MigrationID     int64       `json:"migrationId"`
	Kind            ServiceKind `json:"kind"`
	ResourceHref    string      `json:"resourceHref"`
	SourceETag      string      `json:"sourceEtag"`
	DestinationETag string      `json:"destinationEtag"`
	Resolution      string      `json:"resolution,omitempty"`
}
