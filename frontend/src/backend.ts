import type {
  AccountConfig, Conflict, DAVAccountSummary, DAVEndpoint, DAVServiceRequest, JobPreflightResult, Progress,
  MailIssue, MailIssueResolution, RecentMigration, ResolveSourceDeletionsRequest, ResumeJobRequest, ResumeRequirements, ServiceKind, SourceDeletion, StartJobRequest, TransferOptions, UpdateInfo,
} from './types'

type Backend = {
  Defaults(): Promise<TransferOptions>
  CheckForUpdate(): Promise<UpdateInfo>
  OpenLatestRelease(): Promise<void>
  TestAccount(account: AccountConfig): Promise<import('./types').ServerSummary>
  TestDAVAccount(kind: ServiceKind, endpoint: DAVEndpoint): Promise<DAVAccountSummary>
  AnalyseJob(request: {
    mailEnabled: boolean; mailSource: AccountConfig; mailDestination: AccountConfig
    calendar: DAVServiceRequest; contacts: DAVServiceRequest
  }): Promise<JobPreflightResult>
  StartJob(request: StartJobRequest): Promise<number>
  ResumeRequirements(id: number): Promise<ResumeRequirements>
  ResumeJob(request: ResumeJobRequest): Promise<number>
  PauseJob(id: number): Promise<void>
  ContinueJob(id: number): Promise<void>
  CancelJob(id: number): Promise<void>
  RecentMigrations(): Promise<RecentMigration[]>
  ExportReport(id: number): Promise<string>
  ExportSupportBundle(id: number): Promise<string>
  JobConflicts(id: number): Promise<Conflict[]>
  ResolveJobConflict(id: number, resolution: string): Promise<void>
  JobMailIssues(id: number): Promise<MailIssue[]>
  ResolveMailIssue(id: number, resolution: MailIssueResolution): Promise<void>
  JobSourceDeletions(id: number): Promise<SourceDeletion[]>
  ResolveSourceDeletions(request: ResolveSourceDeletionsRequest): Promise<void>
  DiscardSourceDeletionCredential(id: number): Promise<void>
  ResetMigrationData(): Promise<void>
  FactoryReset(): Promise<void>
}

declare global {
  interface Window {
    go?: { main?: { App?: Backend } }
    runtime?: { EventsOn?: (name: string, callback: (payload: Progress) => void) => () => void }
  }
}

function api(): Backend {
  const desktop = window.go?.main?.App
  if (!desktop) throw new Error('The desktop connection is unavailable. Start the interface through Wails.')
  return desktop
}

function call<T>(operation: (desktop: Backend) => Promise<T>): Promise<T> {
  return Promise.resolve().then(() => operation(api()))
}

export const backend = {
  available: () => Boolean(window.go?.main?.App),
  defaults: () => call(desktop => desktop.Defaults()),
  checkForUpdate: () => call(desktop => desktop.CheckForUpdate()),
  openLatestRelease: () => call(desktop => desktop.OpenLatestRelease()),
  testMail: (account: AccountConfig) => call(desktop => desktop.TestAccount(account)),
  testDAV: (kind: ServiceKind, endpoint: DAVEndpoint) => call(desktop => desktop.TestDAVAccount(kind, endpoint)),
  analyseJob: (request: Parameters<Backend['AnalyseJob']>[0]) => call(desktop => desktop.AnalyseJob(request)),
  startJob: (request: StartJobRequest) => call(desktop => desktop.StartJob(request)),
  resumeRequirements: (id: number) => call(desktop => desktop.ResumeRequirements(id)),
  resumeJob: (request: ResumeJobRequest) => call(desktop => desktop.ResumeJob(request)),
  pause: (id: number) => call(desktop => desktop.PauseJob(id)),
  continue: (id: number) => call(desktop => desktop.ContinueJob(id)),
  cancel: (id: number) => call(desktop => desktop.CancelJob(id)),
  recent: () => call(desktop => desktop.RecentMigrations()).then(items => items ?? []),
  exportReport: (id: number) => call(desktop => desktop.ExportReport(id)),
  exportSupport: (id: number) => call(desktop => desktop.ExportSupportBundle(id)),
  conflicts: (id: number) => call(desktop => desktop.JobConflicts(id)).then(items => items ?? []),
  resolveConflict: (id: number, resolution: 'source' | 'destination') => call(desktop => desktop.ResolveJobConflict(id, resolution)),
  mailIssues: (id: number) => call(desktop => desktop.JobMailIssues(id)).then(items => items ?? []),
  resolveMailIssue: (id: number, resolution: MailIssueResolution) => call(desktop => desktop.ResolveMailIssue(id, resolution)),
  sourceDeletions: (id: number) => call(desktop => desktop.JobSourceDeletions(id)).then(items => items ?? []),
  resolveSourceDeletions: (request: ResolveSourceDeletionsRequest) => call(desktop => desktop.ResolveSourceDeletions(request)),
  discardSourceDeletionCredential: (id: number) => call(desktop => desktop.DiscardSourceDeletionCredential(id)),
  resetMigrationData: () => call(desktop => desktop.ResetMigrationData()),
  factoryReset: () => call(desktop => desktop.FactoryReset()),
  onProgress: (callback: (progress: Progress) => void) => {
    try { return window.runtime?.EventsOn?.('job:progress', callback) ?? (() => undefined) }
    catch { return () => undefined }
  },
}
