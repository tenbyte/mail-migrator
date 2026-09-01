export type Encryption = 'tls' | 'starttls'
export type ServiceKind = 'mail' | 'calendar' | 'contacts'
export type DAVAuthMethod = 'auto' | 'basic' | 'digest'

export interface UpdateInfo {
  currentVersion: string
  latestVersion?: string
  updateAvailable: boolean
}

export interface AccountConfig {
  host: string
  port: number
  encryption: Encryption
  username: string
  password: string
  rememberCredential: boolean
  credentialId?: string
}

export interface DAVEndpoint {
  url: string
  username: string
  password: string
  authMethod: DAVAuthMethod
  rememberCredential: boolean
  credentialId?: string
}

export interface Mailbox { name: string; delimiter: number; attributes: string[]; specialUse?: string; selectable: boolean; messages: number; uidValidity: number; uidNext: number; size: number; sizeKnown: boolean }
export interface FolderMapping { source: Mailbox; destinationName: string; destinationDelimiter: number; destinationExists: boolean; enabled: boolean }
export interface ServerSummary { connected: boolean; host: string; capabilities: string[]; mailboxes: Mailbox[]; folderCount: number; messages: number; bytes: number; uidPlus: boolean; appendLimit?: number; quotaAvailableBytes?: number; quotaUsedBytes?: number; warnings: string[] }
export interface MailKeyword { name: string; occurrences: Record<string, number> }
export interface PreflightResult { source: ServerSummary; destination: ServerSummary; mappings: FolderMapping[]; keywords: MailKeyword[]; warnings: string[] }

export interface DAVCollection {
  path: string; name: string; description?: string; kind: ServiceKind; components: string[]; contentTypes: string[]
  objects: number; bytes: number; maxResourceSize?: number; quotaAvailableBytes?: number; quotaUsedBytes?: number; syncToken?: string
}
export interface CollectionMapping { source: DAVCollection; destinationPath: string; destinationName: string; destinationExists: boolean; enabled: boolean }
export interface DAVAccountSummary {
  connected: boolean; endpoint: string; principal: string; homeSet: string; kind: ServiceKind; collections: DAVCollection[]
  capabilities: string[]; collectionCount: number; objects: number; bytes: number; verified: boolean; warnings: string[]
}
export interface ConversionWarning { resourceHref: string; kind: ServiceKind; code: string; message: string }
export interface DAVPreflightResult {
  kind: ServiceKind; source: DAVAccountSummary; destination: DAVAccountSummary; mappings: CollectionMapping[]
  objectsScanned: number; conversions: number; potentialConflicts: number; problems: ConversionWarning[]; warnings: string[]
}
export interface DAVServiceRequest { kind: ServiceKind; enabled: boolean; source: DAVEndpoint; destination: DAVEndpoint; mappings: CollectionMapping[] }
export interface JobPreflightResult { mail?: PreflightResult; calendar?: DAVPreflightResult; contacts?: DAVPreflightResult; warnings: string[] }

export interface TransferOptions {
  concurrency: number; maximumRetries: number; connectionTimeout: number; stallTimeout: number
  preserveFlags: boolean; excludedKeywords?: string[]; preserveDate: boolean; duplicateProtection: boolean; verifyAfter: boolean
  verificationMode: 'full_hash' | 'metadata'
}

export interface ServiceProgress {
  kind: ServiceKind; state: string; current: string; itemsTotal: number; itemsDone: number; itemsFailed: number
  bytesTotal: number; bytesDone: number; itemsVerified?: number; itemsQuarantined?: number; itemsUnknown?: number
  itemsSkipped?: number; itemsDeduplicated?: number; verificationFailed?: number; lastError?: string
  runMode?: string; runPhase?: string; runItemsTotal?: number; runItemsDone?: number; runIndeterminate?: boolean
}
export interface Progress {
  migrationId: number; service?: ServiceKind; state: string; currentFolder: string; currentUid: number
  messagesTotal: number; messagesCopied: number; messagesFailed: number; bytesTotal: number; bytesCopied: number
  bytesPerSecond: number; startedAt: string; lastError?: string; services?: ServiceProgress[]
  runMode?: string; runPhase?: string; runItemsTotal?: number; runItemsDone?: number; runIndeterminate?: boolean
}
export interface RecentMigration {
  id: number; createdAt: string; finishedAt?: string; state: string; sourceHost: string; destinationHost: string
  sourceUsername: string; destinationUsername: string
  messagesTotal: number; messagesCopied: number; messagesFailed: number; bytesTotal: number; bytesCopied: number; services?: ServiceKind[]
}
export interface Conflict { id: number; migrationId: number; kind: ServiceKind; resourceHref: string; sourceEtag: string; destinationEtag: string; resolution?: string }
export type MailIssueResolution = 'transfer_anyway' | 'retry' | 'verify_again' | 'keep_skipped'
export interface MailIssue {
  id: number; migrationId: number; folder: string; sourceUid: number; size: number; state: string
  errorCode: string; message: string; verification?: string; allowedActions: MailIssueResolution[]
}

export type SourceDeletionResolution = 'keep' | 'trash' | 'delete'
export interface SourceDeletion {
  id: number; migrationId: number; folder: string; destinationFolder: string; sourceUid: number; destinationUid: number
  subject: string; from: string; internalDate: string; size: number; resolution?: SourceDeletionResolution
  status: string; lastError?: string; detectedAt: string; updatedAt: string; resolvedAt?: string
}
export interface SourceDeletionAction { id: number; resolution: SourceDeletionResolution }
export interface ResolveSourceDeletionsRequest { migrationId: number; actions: SourceDeletionAction[] }

export interface StartJobRequest {
  mailEnabled: boolean; mailSource: AccountConfig; mailDestination: AccountConfig; mailMappings: FolderMapping[]
  calendar: DAVServiceRequest; contacts: DAVServiceRequest; options: TransferOptions; migrationId?: number; mode?: string
}

export interface CredentialRequirement {
  kind: ServiceKind
  sourceAvailable: boolean
  destinationAvailable: boolean
}

export interface ResumeRequirements {
  migration: RecentMigration
  credentials: CredentialRequirement[]
}

export interface ResumeCredentialInput {
  sourcePassword?: string
  destinationPassword?: string
}

export interface ResumeJobRequest {
  migrationId: number
  credentials: Partial<Record<ServiceKind, ResumeCredentialInput>>
  rememberNewCredentials: boolean
}
