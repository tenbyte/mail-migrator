import { useEffect, useMemo, useState } from 'react'
import { Toaster, toast } from 'sonner'
import { backend } from './backend'
import { formatBytes, formatNumber, humanState } from './format'
import { buildResumeJobRequest, resumeCredentialsValid } from './resume'
import type { ResumePasswords } from './resume'
import {
  NEW_DESTINATION, chooseDAVDestination, chooseMailDestination, dryRunCards, mergeDAVMappings, mergeMailMappings,
  navigationLocked, selectableMailboxes,
} from './mapping'
import logoUrl from '../../branding/logo.png'
import type {
  AccountConfig, CollectionMapping, Conflict, DAVAccountSummary, DAVEndpoint, DAVServiceRequest, FolderMapping,
  JobPreflightResult, MailIssue, MailIssueResolution, MailKeyword, Progress, RecentMigration, ResumeRequirements, ServerSummary, ServiceKind, SourceDeletion, SourceDeletionAction, SourceDeletionResolution, StartJobRequest, TransferOptions, UpdateInfo,
} from './types'

const emptyMail = (): AccountConfig => ({ host: '', port: 993, encryption: 'tls', username: '', password: '', rememberCredential: false })
const emptyDAV = (): DAVEndpoint => ({ url: '', username: '', password: '', authMethod: 'auto', rememberCredential: false })
const fallbackOptions: TransferOptions = { concurrency: 2, maximumRetries: 8, connectionTimeout: 15, stallTimeout: 90, preserveFlags: true, excludedKeywords: [], preserveDate: true, duplicateProtection: true, verifyAfter: true, verificationMode: 'full_hash' }
const serviceLabels: Record<ServiceKind, string> = { mail: 'Mail', calendar: 'Calendar', contacts: 'Contacts' }
const resumableStates = ['INTERRUPTED', 'COMPLETED', 'COMPLETED_WITH_ERRORS', 'FAILED', 'CANCELLED']
type AppView = 'connections' | 'selection' | 'transfer'
export type ResetKind = 'migrations' | 'factory'

function Icon({ name }: { name: 'arrow' | 'check' | 'download' | 'support' }) {
  const paths = {
    arrow: <><path d="M5 12h14"/><path d="m14 7 5 5-5 5"/></>,
    check: <path d="m5 12 4 4L19 6"/>,
    download: <><path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/></>,
    support: <><circle cx="12" cy="12" r="9"/><path d="M9.7 9a2.5 2.5 0 1 1 3.6 2.25c-.8.42-1.3.86-1.3 1.75"/><path d="M12 17h.01"/></>,
  }
  return <svg className="icon" viewBox="0 0 24 24" aria-hidden="true">{paths[name]}</svg>
}

interface HeaderProps {
  view: AppView
  selectionAvailable: boolean
  transferAvailable: boolean
  locked: boolean
  onNavigate: (view: AppView) => void
}

function Header({ view, selectionAvailable, transferAvailable, locked, onNavigate }: HeaderProps) {
  const steps: Array<{ view: AppView; label: string }> = [
    { view: 'connections', label: 'Connections' }, { view: 'selection', label: 'Selection' }, { view: 'transfer', label: 'Transfer' },
  ]
  const activeIndex = steps.findIndex(item => item.view === view)
  return <header className="app-header"><nav className="workflow" aria-label="Migration steps">
    {steps.map((item, index) => {
      const available = item.view === 'connections' || (item.view === 'selection' ? selectionAvailable : transferAvailable)
      const complete = index < activeIndex
      return <button type="button" className={`${view === item.view ? 'active' : ''} ${complete ? 'complete' : ''}`} disabled={locked && item.view !== 'transfer' || !available} onClick={() => onNavigate(item.view)} key={item.view}><span>{complete ? '✓' : index + 1}</span>{item.label}</button>
    })}
  </nav><img className="header-logo" src={logoUrl} alt="" aria-hidden="true" draggable={false}/></header>
}

export function visibleServices(davAlphaEnabled: boolean): ServiceKind[] {
  return davAlphaEnabled ? ['mail', 'calendar', 'contacts'] : ['mail']
}

export function usesDAV(services?: ServiceKind[]) {
  return (services ?? ['mail']).some(kind => kind === 'calendar' || kind === 'contacts')
}

export function disabledDAVServices(current: Record<ServiceKind, boolean>): Record<ServiceKind, boolean> {
  return { ...current, mail: true, calendar: false, contacts: false }
}

export function buildDAVServiceRequest(kind: ServiceKind, alphaEnabled: boolean, serviceEnabled: boolean, source: DAVEndpoint, destination: DAVEndpoint, mappings: CollectionMapping[]): DAVServiceRequest {

  if (!alphaEnabled) return { kind, enabled: false, source: emptyDAV(), destination: emptyDAV(), mappings: [] }
  return { kind, enabled: alphaEnabled && serviceEnabled, source, destination, mappings: alphaEnabled ? mappings : [] }
}

function ServiceSelector({ services, enabled, onChange, active, onActive }: { services: ServiceKind[]; enabled: Record<ServiceKind, boolean>; onChange: (kind: ServiceKind, value: boolean) => void; active: ServiceKind; onActive: (kind: ServiceKind) => void }) {
  return <section className="service-selector" aria-label="Data types">
    {services.map(kind => <button key={kind} className={`${active === kind ? 'active' : ''} ${enabled[kind] ? 'enabled' : ''}`} onClick={() => { if (!enabled[kind]) onChange(kind, true); onActive(kind) }}>
      <span className="service-check" onClick={event => { event.stopPropagation(); onChange(kind, !enabled[kind]) }}>{enabled[kind] ? '✓' : ''}</span><span><strong>{serviceLabels[kind]}{kind !== 'mail' && <em className="alpha-badge">Alpha</em>}</strong><small>{kind === 'mail' ? 'IMAP' : kind === 'calendar' ? 'CalDAV' : 'CardDAV'}</small></span>
    </button>)}
  </section>
}

function MailFields({ account, onChange }: { account: AccountConfig; onChange: (value: AccountConfig) => void }) {
  const patch = (next: Partial<AccountConfig>) => onChange({ ...account, ...next })
  return <div className="form-grid">
    <label className="span-2">IMAP server<input value={account.host} autoCapitalize="none" spellCheck={false} placeholder="imap.example.com" onChange={event => patch({ host: event.target.value })}/></label>
    <label>Port<input type="number" min={1} max={65535} value={account.port} onChange={event => patch({ port: Number(event.target.value) })}/></label>
    <label>Connection<select value={account.encryption} onChange={event => patch({ encryption: event.target.value as AccountConfig['encryption'], port: event.target.value === 'tls' ? 993 : 143 })}><option value="tls">TLS</option><option value="starttls">STARTTLS</option></select></label>
    <label className="span-2">Username<input value={account.username} autoCapitalize="none" spellCheck={false} onChange={event => patch({ username: event.target.value })}/></label>
    <label className="span-2">Password<input type="password" value={account.password} autoComplete="off" onChange={event => patch({ password: event.target.value })}/></label>
    <Remember checked={account.rememberCredential} onChange={value => patch({ rememberCredential: value })}/>
  </div>
}

function DAVFields({ endpoint, onChange, kind, mailAccount }: { endpoint: DAVEndpoint; onChange: (value: DAVEndpoint) => void; kind: ServiceKind; mailAccount: AccountConfig }) {
  const patch = (next: Partial<DAVEndpoint>) => onChange({ ...endpoint, ...next })
  return <div className="form-grid">
    <label className="span-2">{serviceLabels[kind]} server<input value={endpoint.url} autoCapitalize="none" spellCheck={false} placeholder={kind === 'calendar' ? 'https://dav.example.com/caldav/' : 'https://dav.example.com/carddav/'} onChange={event => patch({ url: event.target.value })}/><small>Source and destination are configured separately. Leave this empty to discover the server from the email domain.</small></label>
    {mailAccount.username && mailAccount.password && <div className="dav-discovery span-2"><span><strong>Use the mail credentials?</strong><small>Only the username and password are copied, not the server address.</small></span><button type="button" className="secondary" onClick={() => onChange({ ...endpoint, username: mailAccount.username, password: mailAccount.password, rememberCredential: mailAccount.rememberCredential })}>Use mail credentials</button></div>}
    <label className="span-2">Account email / username<input value={endpoint.username} autoCapitalize="none" spellCheck={false} placeholder="name@example.com" onChange={event => patch({ username: event.target.value })}/></label>
    <label className="span-2">Password or app password<input type="password" value={endpoint.password} autoComplete="off" onChange={event => patch({ password: event.target.value })}/></label>
    <details className="dav-manual span-2"><summary>Set authentication manually</summary><div><label>Authentication<select value={endpoint.authMethod} onChange={event => patch({ authMethod: event.target.value as DAVEndpoint['authMethod'] })}><option value="auto">Automatic</option><option value="basic">Basic</option><option value="digest">Digest</option></select></label></div></details>
    <Remember checked={endpoint.rememberCredential} onChange={value => patch({ rememberCredential: value })}/>
  </div>
}

function Remember({ checked, onChange }: { checked: boolean; onChange: (value: boolean) => void }) {
  return <label className="check span-2"><input type="checkbox" checked={checked} onChange={event => onChange(event.target.checked)}/><span><strong>Save in the system credential store</strong><small>Used for later delta syncs.</small></span></label>
}

type SideStatus = ServerSummary | DAVAccountSummary | undefined
function statusMetrics(status: SideStatus) {
  if (!status) return undefined
  if ('folderCount' in status) return { collections: status.folderCount, objects: status.messages, bytes: status.bytes }
  return { collections: status.collectionCount, objects: status.objects, bytes: status.bytes }
}

function ConnectionPanel({ title, active, mail, dav, onMail, onDAV, status, busy, onTest }: {
  title: string; active: ServiceKind; mail: AccountConfig; dav: DAVEndpoint; onMail: (value: AccountConfig) => void; onDAV: (value: DAVEndpoint) => void
  status?: SideStatus; busy: boolean; onTest: () => void
}) {
  const ready = Boolean(status?.connected)
  const metrics = statusMetrics(status)
  const valid = active === 'mail' ? Boolean(mail.host && mail.username && mail.password) : Boolean(dav.username && dav.password && (dav.url || dav.username.includes('@')))
  return <section className="account-panel">
    <div className="panel-heading"><div><h2>{title}</h2><p>{serviceLabels[active]} account</p></div><span className={`connection-state ${ready ? 'connected' : busy ? 'checking' : ''}`}><i/>{ready ? 'Checked' : busy ? 'Checking' : 'Not checked'}</span></div>
    {active === 'mail' ? <MailFields account={mail} onChange={onMail}/> : <DAVFields endpoint={dav} onChange={onDAV} kind={active} mailAccount={mail}/>}
    {metrics && <div className="account-summary"><div><span>Collections</span><strong>{formatNumber(metrics.collections)}</strong></div><div><span>Objects</span><strong>{formatNumber(metrics.objects)}</strong></div><div><span>Data</span><strong>{formatBytes(metrics.bytes)}</strong></div></div>}
    <button className="secondary full" disabled={busy || !valid} onClick={onTest}>{busy && <span className="spinner"/>}{busy ? 'Checking connection' : ready ? 'Check again' : 'Check connection'}</button>
  </section>
}

function MappingView({ navigation, preflight, mappingWarnings, mailMappings, setMailMappings, calendarMappings, setCalendarMappings, contactMappings, setContactMappings, options, setOptions, onBack, onStart, busy }: {
  navigation: HeaderProps
  preflight: JobPreflightResult; mailMappings: FolderMapping[]; setMailMappings: (value: FolderMapping[]) => void
  calendarMappings: CollectionMapping[]; setCalendarMappings: (value: CollectionMapping[]) => void; contactMappings: CollectionMapping[]; setContactMappings: (value: CollectionMapping[]) => void
  options: TransferOptions; setOptions: (value: TransferOptions) => void
  mappingWarnings: string[]; onBack: () => void; onStart: () => void; busy: boolean
}) {
  const count = mailMappings.filter(item => item.enabled).length + calendarMappings.filter(item => item.enabled).length + contactMappings.filter(item => item.enabled).length
  return <><Header {...navigation}/><main className="workspace mapping-page">
    <div className="workspace-heading"><div><button className="back-link" onClick={onBack}>‹ Connections</button><h1>Review selection</h1><p>{count} folders and collections selected</p></div><button className="primary" disabled={busy || count === 0} onClick={onStart}>{busy && <span className="spinner"/>}{busy ? 'Starting migration' : 'Start migration'}</button></div>
    {[...(preflight.warnings ?? []), ...mappingWarnings].length > 0 && <div className="notice warning"><strong>Warnings</strong><div>{[...(preflight.warnings ?? []), ...mappingWarnings].map((warning, index) => <p key={index}>{warning}</p>)}</div></div>}
    {preflight.mail && <><MappingSection kind="mail" mappings={mailMappings} destinations={preflight.mail.destination.mailboxes ?? []} setMappings={setMailMappings}/><MailKeywordSelection keywords={preflight.mail.keywords ?? []} mappings={mailMappings} excluded={options.excludedKeywords ?? []} onChange={excludedKeywords => setOptions({ ...options, excludedKeywords })}/></>}
    {preflight.calendar && <DAVMappingSection kind="calendar" mappings={calendarMappings} destinations={preflight.calendar.destination.collections ?? []} setMappings={setCalendarMappings}/>}
    {preflight.contacts && <DAVMappingSection kind="contacts" mappings={contactMappings} destinations={preflight.contacts.destination.collections ?? []} setMappings={setContactMappings}/>}
    <DryRunSummary preflight={preflight} mailMappings={mailMappings}/>
  </main></>
}

export function keywordCountForMappings(keyword: MailKeyword, mappings: FolderMapping[]) {
  return mappings.reduce((total, mapping) => total + (mapping.enabled ? keyword.occurrences[mapping.source.name] ?? 0 : 0), 0)
}

export function isTechnicalKeyword(name: string) {
  return /^\$(?:HasAttachment|HasNoAttachment|MailFlagBit\d+)$/i.test(name)
}

function MailKeywordSelection({ keywords, mappings, excluded, onChange }: { keywords: MailKeyword[]; mappings: FolderMapping[]; excluded: string[]; onChange: (value: string[]) => void }) {
  const visible = keywords.map(keyword => ({ ...keyword, count: keywordCountForMappings(keyword, mappings) })).filter(keyword => keyword.count > 0)
  const excludedSet = new Set(excluded.map(keyword => keyword.toLowerCase()))
  const updateVisible = (included: Set<string>) => {
    const visibleSet = new Set(visible.map(keyword => keyword.name.toLowerCase()))
    const preserved = excluded.filter(keyword => !visibleSet.has(keyword.toLowerCase()))
    onChange([...preserved, ...visible.filter(keyword => !included.has(keyword.name.toLowerCase())).map(keyword => keyword.name)])
  }
  const toggle = (name: string, checked: boolean) => {
    const next = new Set(visible.filter(keyword => !excludedSet.has(keyword.name.toLowerCase())).map(keyword => keyword.name.toLowerCase()))
    checked ? next.add(name.toLowerCase()) : next.delete(name.toLowerCase())
    updateVisible(next)
  }
  return <section className="keyword-selection"><div className="section-heading"><h2>Mail tags</h2><span>{visible.length} {visible.length === 1 ? 'tag' : 'tags'} found</span></div><div className="keyword-panel"><div className="keyword-intro"><div><strong>Select custom tags</strong><p>Only selected IMAP keywords are copied. Standard flags such as Seen, Answered and Flagged are preserved.</p></div>{visible.length > 0 && <div className="keyword-actions"><button className="secondary" onClick={() => updateVisible(new Set(visible.filter(keyword => !isTechnicalKeyword(keyword.name)).map(keyword => keyword.name.toLowerCase())))}>Recommended</button><button className="secondary" onClick={() => updateVisible(new Set(visible.map(keyword => keyword.name.toLowerCase())))}>All</button><button className="secondary" onClick={() => updateVisible(new Set())}>None</button></div>}</div>{visible.length ? <div className="keyword-grid">{visible.map(keyword => { const checked = !excludedSet.has(keyword.name.toLowerCase()); const technical = isTechnicalKeyword(keyword.name); return <label className="keyword-option" key={keyword.name}><input type="checkbox" checked={checked} onChange={event => toggle(keyword.name, event.target.checked)}/><span><span className="keyword-title"><strong>{keyword.name}</strong>{technical && <em>Technical</em>}</span><small>{formatNumber(keyword.count)} {keyword.count === 1 ? 'message' : 'messages'}</small></span></label> })}</div> : <p className="keyword-empty">No custom tags were found in the selected folders.</p>}</div></section>
}

function MappingSection({ kind, mappings, destinations, setMappings }: { kind: ServiceKind; mappings: FolderMapping[]; destinations: import('./types').Mailbox[]; setMappings: (value: FolderMapping[]) => void }) {
  const targets = selectableMailboxes(destinations)
  const update = (index: number, value: FolderMapping) => setMappings(mappings.map((item, current) => current === index ? value : item))
  return <section className="mapping-group"><div className="section-heading"><h2>{serviceLabels[kind]}</h2><span>{mappings.length} folders</span></div><div className="mapping-table"><div className="mapping-header"><span/><span>Source</span><span>Destination</span><span>Type</span></div>{mappings.map((mapping, index) => <div className={`mapping-row ${mapping.enabled ? '' : 'disabled'}`} key={mapping.source.name}><Toggle checked={mapping.enabled} label={mapping.source.name} onChange={value => update(index, { ...mapping, enabled: value })}/><div className="folder-name"><strong>{mapping.source.name}</strong><small>{formatNumber(mapping.source.messages)} messages · {formatBytes(mapping.source.size)}</small></div><div className="destination-field"><Icon name="arrow"/><div className="destination-control"><select aria-label={`Destination for ${mapping.source.name}`} value={mapping.destinationExists && targets.some(target => target.name === mapping.destinationName) ? mapping.destinationName : NEW_DESTINATION} onChange={event => update(index, chooseMailDestination(mapping, event.target.value, targets))}>{targets.map(target => <option value={target.name} key={target.name}>{target.name}</option>)}<option value={NEW_DESTINATION}>Create new folder...</option></select>{!mapping.destinationExists && <input aria-label={`Name of new destination folder for ${mapping.source.name}`} value={mapping.destinationName} placeholder="Folder name" onChange={event => update(index, { ...mapping, destinationName: event.target.value, destinationExists: false })}/>}</div></div><span className="category">{mapping.source.specialUse?.replace('\\', '') || 'Folder'}</span></div>)}</div></section>
}

function DAVMappingSection({ kind, mappings, destinations, setMappings }: { kind: ServiceKind; mappings: CollectionMapping[]; destinations: import('./types').DAVCollection[]; setMappings: (value: CollectionMapping[]) => void }) {
  const update = (index: number, value: CollectionMapping) => setMappings(mappings.map((item, current) => current === index ? value : item))
  return <section className="mapping-group"><div className="section-heading"><h2>{serviceLabels[kind]}</h2><span>{mappings.length} collections</span></div><div className="mapping-table"><div className="mapping-header"><span/><span>Source</span><span>Destination</span><span>Type</span></div>{mappings.map((mapping, index) => <div className={`mapping-row ${mapping.enabled ? '' : 'disabled'}`} key={mapping.source.path}><Toggle checked={mapping.enabled} label={mapping.source.name} onChange={value => update(index, { ...mapping, enabled: value })}/><div className="folder-name"><strong>{mapping.source.name}</strong><small>{formatNumber(mapping.source.objects)} objects · {formatBytes(mapping.source.bytes)}</small></div><div className="destination-field"><Icon name="arrow"/><div className="destination-control"><select aria-label={`Destination for ${mapping.source.name}`} value={mapping.destinationExists && destinations.some(target => target.path === mapping.destinationPath) ? mapping.destinationPath : NEW_DESTINATION} onChange={event => update(index, chooseDAVDestination(mapping, event.target.value, destinations))}>{destinations.map(target => <option value={target.path} key={target.path}>{target.name}</option>)}<option value={NEW_DESTINATION}>Create new collection...</option></select>{!mapping.destinationExists && <input aria-label={`Name of new destination collection for ${mapping.source.name}`} value={mapping.destinationName} placeholder="Collection name" onChange={event => update(index, { ...mapping, destinationName: event.target.value, destinationPath: '', destinationExists: false })}/>}</div></div><span className="category">{mapping.source.components?.join(', ') || (kind === 'contacts' ? 'vCard' : 'Calendar')}</span></div>)}</div></section>
}

function DryRunSummary({ preflight, mailMappings }: { preflight: JobPreflightResult; mailMappings: FolderMapping[] }) {
  const cards = dryRunCards(preflight, mailMappings)
  return <section className="dry-run"><div className="section-heading"><h2>Dry run completed</h2><span>{cards.length} {cards.length === 1 ? 'data type' : 'data types'}</span></div><div className="dry-run-grid">{cards.map(card => <article key={card.kind}><strong>{card.title}</strong><div>{card.metrics.map(metric => <span key={metric.label}><b>{metric.bytes ? formatBytes(metric.value) : formatNumber(metric.value)}</b>{metric.label}</span>)}</div></article>)}</div><small>No destination content is deleted. Destination changes are held as conflicts.</small></section>
}

function Toggle({ checked, label, onChange }: { checked: boolean; label: string; onChange: (value: boolean) => void }) { return <label className="toggle"><input aria-label={`Transfer ${label}`} type="checkbox" checked={checked} onChange={event => onChange(event.target.checked)}/><i/></label> }

export function displayedProgress(progress: Pick<Progress, 'state' | 'runMode' | 'runItemsTotal' | 'runItemsDone' | 'runIndeterminate' | 'messagesTotal' | 'messagesCopied'>) {
  const terminal = ['COMPLETED', 'COMPLETED_WITH_ERRORS'].includes(progress.state)
  if (terminal) return { percent: 100, done: progress.runItemsDone ?? progress.messagesCopied, total: progress.runItemsTotal ?? progress.messagesTotal, indeterminate: false, runBased: progress.runMode === 'reconcile' || progress.runItemsTotal !== undefined }
  if (progress.runMode === 'reconcile' || progress.runItemsTotal !== undefined) {
    const total = progress.runItemsTotal ?? 0; const done = progress.runItemsDone ?? 0
    return { percent: total ? Math.min(99.9, done / total * 100) : 0, done, total, indeterminate: Boolean(progress.runIndeterminate), runBased: true }
  }
  return { percent: progress.messagesTotal ? Math.min(99.9, progress.messagesCopied / progress.messagesTotal * 100) : 0, done: progress.messagesCopied, total: progress.messagesTotal, indeterminate: false, runBased: false }
}

export function StartupNotice({
  update,
  onClose,
  onViewRelease,
}: {
  update?: UpdateInfo;
  onClose: () => void;
  onViewRelease: () => void;
}) {
  return (
    <div className="dialog-backdrop" role="presentation">
      <section
        className="resume-dialog startup-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="startup-title"
      >
        <div className="dialog-heading">
          <div>
            <span className="eyebrow">Notice</span>

            <h2 id="startup-title">A quick note before you start</h2>

            <p>
              This mail migrator is still under active development. If you run
              into any issues, please report them with as much detail as
              possible. Your feedback helps us improve the migration process.
            </p>

            <div className="startup-safety">
              <strong>Use at your own risk</strong>
              <p>
                You are responsible for the accounts, settings, and actions you
                choose in this app. Keep an independent backup and try a small,
                non-critical migration first.
              </p>
            </div>

            <p>
              Currently, <strong>IMAP mail migration is supported</strong>.{" "}
              <strong>CalDAV and CardDAV are experimental Alpha features</strong>{" "}
              and may not work reliably yet.
            </p>

            <p className="artwork-credit">
              Courier artwork adapted from the Go Gopher by Renee French under
              CC BY 4.0. This project is unofficial and is not endorsed by the
              Go project or Google.
            </p>
          </div>
        </div>

        {update?.updateAvailable && (
          <div className="update-notice">
            <p>
              Version {update.latestVersion} is available. You are running{" "}
              {update.currentVersion}.
            </p>

            <button className="secondary" onClick={onViewRelease}>
              View release
            </button>
          </div>
        )}

        <div className="dialog-actions">
          <button autoFocus className="primary" onClick={onClose}>
            Continue
          </button>
        </div>
      </section>
    </div>
  );
}

export function ResetDialog({ kind, busy, onClose, onConfirm }: { kind: ResetKind; busy: boolean; onClose: () => void; onConfirm: () => void }) {
  const factory = kind === 'factory'
  const title = factory ? 'Reset the entire app?' : 'Reset migration data?'
  return <div className="dialog-backdrop" role="presentation" onMouseDown={event => { if (!busy && event.target === event.currentTarget) onClose() }}><section className="resume-dialog reset-dialog" role="dialog" aria-modal="true" aria-labelledby="reset-title"><div className="dialog-heading"><div><span className="eyebrow">Reset and recovery</span><h2 id="reset-title">{title}</h2><p>{factory ? 'This returns Tenbyte Mail Migrator to its first-run state.' : 'This removes the local migration cache and recovery history.'}</p></div><button className="dialog-close" aria-label="Close" disabled={busy} onClick={onClose}>×</button></div><div className="reset-summary"><div><strong>Deleted</strong><ul><li>Migration history and progress</li><li>Delta-sync and recovery state</li><li>Local database files and schema backups</li>{factory && <li>Passwords saved by this app in the system credential store</li>}</ul></div><div><strong>Kept</strong><ul>{!factory && <li>Saved passwords and the current connection form</li>}<li>Exported reports and support bundles</li><li>All data on source and destination servers</li></ul></div></div><div className="notice error"><strong>This cannot be undone</strong><p>{factory ? 'The app reloads immediately after the reset.' : 'A new empty migration database is created immediately.'}</p></div><div className="dialog-actions"><button className="secondary" disabled={busy} onClick={onClose}>Cancel</button><button autoFocus className="danger" disabled={busy} onClick={onConfirm}>{busy && <span className="spinner"/>}{busy ? 'Resetting...' : factory ? 'Reset entire app' : 'Reset migration data'}</button></div></section></div>
}

export function buildSourceDeletionActions(items: SourceDeletion[], choices: Record<number, SourceDeletionResolution | ''>): SourceDeletionAction[] {
  return items.flatMap(item => choices[item.id] ? [{ id: item.id, resolution: choices[item.id] as SourceDeletionResolution }] : [])
}

function SourceDeletionPanel({ items, onResolve }: { items: SourceDeletion[]; onResolve: (actions: SourceDeletionAction[]) => Promise<void> }) {
  const [choices, setChoices] = useState<Record<number, SourceDeletionResolution | ''>>({})
  const [confirming, setConfirming] = useState(false)
  const [busy, setBusy] = useState(false)
  const actions = buildSourceDeletionActions(items, choices)
  const destructive = actions.filter(action => action.resolution !== 'keep')
  const setAll = (resolution: SourceDeletionResolution) => setChoices(Object.fromEntries(items.map(item => [item.id, resolution])))
  const apply = async () => {
    if (!actions.length) return
    setBusy(true)
    try {
      await onResolve(actions)
      setChoices({})
      setConfirming(false)
    } finally {
      setBusy(false)
    }
  }
  return <section className="source-deletions"><div className="section-heading"><h2>Deleted at source</h2><span>{items.length} destination copies</span></div><p className="section-description">These messages no longer exist at the source but are still present at the destination. Nothing changes without an explicit selection.</p><div className="bulk-actions"><button className="secondary" onClick={() => setAll('keep')}>Keep all</button><button className="secondary" onClick={() => setAll('trash')}>Move all to trash</button><button className="danger" onClick={() => setAll('delete')}>Delete all permanently</button></div>{items.map(item => <article key={item.id}><div><strong>{item.subject || '(No subject)'}</strong><small>{item.from || 'Unknown sender'} · {item.folder} → {item.destinationFolder}</small><p>{new Date(item.internalDate).toLocaleString()} · UID {item.sourceUid} · {formatBytes(item.size)}</p>{item.lastError && <p className="deletion-error">{item.lastError}</p>}</div><select aria-label={`Action for ${item.subject || `UID ${item.sourceUid}`}`} value={choices[item.id] ?? ''} onChange={event => setChoices(current => ({ ...current, [item.id]: event.target.value as SourceDeletionResolution | '' }))}><option value="">No decision</option><option value="keep">Keep at destination</option><option value="trash">Move to trash</option><option value="delete">Delete permanently</option></select></article>)}<div className="deletion-submit"><button className={destructive.length ? 'danger' : 'primary'} disabled={!actions.length || busy} onClick={() => destructive.length ? setConfirming(true) : void apply()}>{busy ? 'Applying...' : `Apply ${actions.length} decisions`}</button></div>{confirming && <div className="dialog-backdrop" role="presentation"><section className="resume-dialog" role="dialog" aria-modal="true" aria-labelledby="deletion-confirm-title"><div className="dialog-heading"><div><h2 id="deletion-confirm-title">Change destination messages?</h2><p>Move {actions.filter(item => item.resolution === 'trash').length} to trash, permanently delete {actions.filter(item => item.resolution === 'delete').length}, and keep {actions.filter(item => item.resolution === 'keep').length}.</p></div></div><div className="notice error"><strong>Warning</strong><p>Permanently deleted messages cannot be recovered. Unsupported server operations stop without a fallback.</p></div><div className="dialog-actions"><button className="secondary" disabled={busy} onClick={() => setConfirming(false)}>Cancel</button><button className="danger" disabled={busy} onClick={() => void apply()}>{busy ? 'Applying...' : 'Confirm selection'}</button></div></section></div>}</section>
}

function ProgressView({ navigation, progress, conflicts, mailIssues, sourceDeletions, paused, onPause, onCancel, onNew, onDelta, onExport, onSupport, onResolve, onResolveMailIssue, onResolveSourceDeletions }: {
  navigation: HeaderProps; progress: Progress; conflicts: Conflict[]; mailIssues: MailIssue[]; sourceDeletions: SourceDeletion[]; paused: boolean; onPause: () => void; onCancel: () => void; onNew: () => void; onDelta: () => void; onExport: () => void; onSupport: () => void
  onResolve: (id: number, resolution: 'source' | 'destination') => void
  onResolveMailIssue: (id: number, resolution: MailIssueResolution) => void
  onResolveSourceDeletions: (actions: SourceDeletionAction[]) => Promise<void>
}) {
  const display = displayedProgress(progress)
  const percent = display.percent
  const finished = ['COMPLETED', 'COMPLETED_WITH_ERRORS', 'FAILED', 'CANCELLED'].includes(progress.state)
  return <><Header {...navigation}/><main className="workspace progress-page">
    <div className="workspace-heading"><div><span className={`run-state ${finished ? 'finished' : paused ? 'paused' : 'running'}`}><i/>{humanState(progress.state)}</span><h1>Transfer</h1><p>Migration #{progress.migrationId}</p></div><div className="toolbar-actions">{finished ? <><button className="secondary" onClick={onSupport}><Icon name="support"/>Support</button><button className="secondary" onClick={onExport}><Icon name="download"/>Report</button><button className="secondary" onClick={onDelta}>Delta sync</button><button className="primary" onClick={onNew}>New migration</button></> : <><button className="secondary" onClick={onPause}>{paused ? 'Resume' : 'Pause'}</button><button className="danger" onClick={onCancel}>Cancel</button></>}</div></div>
    <section className="transfer-panel"><div className="transfer-current"><div className={`activity-indicator ${finished ? 'finished' : ''}`}>{finished ? <Icon name="check"/> : <span className="spinner"/>}</div><div><span>{progress.runPhase || 'Current operation'}</span><strong>{finished ? humanState(progress.state) : progress.currentFolder || 'Preparing'}</strong><small>{finished ? 'The status is stored locally.' : display.indeterminate ? 'Determining scope...' : `${formatNumber(display.done)} of ${formatNumber(display.total)} ${progress.runMode === 'reconcile' ? 'checks' : 'messages'}`}</small></div><strong className="percent">{display.indeterminate ? '...' : `${percent.toFixed(1)} %`}</strong></div><div className={`progress-track ${display.indeterminate ? 'indeterminate' : ''}`}><div style={display.indeterminate ? undefined : { width: `${percent}%` }}/></div><div className="progress-detail"><span>{display.runBased ? `${formatNumber(display.done)} of ${formatNumber(display.total)} ${progress.runMode === 'reconcile' ? 'checks' : 'messages'}` : `${formatNumber(progress.messagesCopied)} of ${formatNumber(progress.messagesTotal)} objects`}</span><span>{formatBytes(progress.bytesCopied)} of {formatBytes(progress.bytesTotal)}</span></div></section>
    <section className="service-progress-grid">{(progress.services ?? []).map(service => { const runBased = service.runMode === 'reconcile' || service.runItemsTotal !== undefined; const total = runBased ? service.runItemsTotal ?? 0 : service.itemsTotal; const done = runBased ? service.runItemsDone ?? 0 : service.itemsDone; const servicePercent = total ? Math.min(service.state === 'RUNNING' ? 99.9 : 100, done / total * 100) : 0; const details = service.kind === 'mail' ? [`${formatNumber(service.itemsVerified ?? 0)} verified`, service.itemsDeduplicated ? `${service.itemsDeduplicated} already present` : '', service.itemsQuarantined ? `${service.itemsQuarantined} quarantined` : '', service.itemsUnknown ? `${service.itemsUnknown} unknown` : '', service.verificationFailed ? `${service.verificationFailed} verification failures` : ''].filter(Boolean).join(' · ') : (service.itemsFailed ? `${service.itemsFailed} errors` : 'No errors'); return <article key={service.kind}><div><strong>{serviceLabels[service.kind]}</strong><span>{service.runPhase || humanState(service.state)}</span></div><div className={`mini-track ${service.runIndeterminate ? 'indeterminate' : ''}`}><i style={service.runIndeterminate ? undefined : { width: `${servicePercent}%` }}/></div><small>{service.runIndeterminate ? 'Determining scope' : `${formatNumber(done)} / ${formatNumber(total)}`} · {details}</small></article> })}</section>
    {conflicts.filter(item => !item.resolution).length > 0 && <section className="conflicts"><div className="section-heading"><h2>Conflicts</h2><span>Decision required</span></div>{conflicts.filter(item => !item.resolution).map(conflict => <article key={conflict.id}><div><strong>{serviceLabels[conflict.kind]}</strong><small>{conflict.resourceHref}</small></div><button className="secondary" onClick={() => onResolve(conflict.id, 'destination')}>Keep destination</button><button className="primary" onClick={() => onResolve(conflict.id, 'source')}>Use source</button></article>)}</section>}
    {finished && mailIssues.length > 0 && <section className="mail-issues"><div className="section-heading"><h2>Message issues</h2><span>{mailIssues.length} cases</span></div><p className="section-description">Approved actions are applied safely during the next manual delta sync.</p>{mailIssues.map(issue => <article key={issue.id}><div><strong>{issue.folder} · UID {issue.sourceUid}</strong><small>{issue.errorCode} · {formatBytes(issue.size)}</small><p>{issue.message}</p></div><div className="issue-actions">{issue.allowedActions.map(action => <button className={action === 'transfer_anyway' ? 'primary' : 'secondary'} key={action} onClick={() => onResolveMailIssue(issue.id, action)}>{action === 'transfer_anyway' ? 'Transfer anyway' : action === 'retry' ? 'Try again' : action === 'verify_again' ? 'Verify again' : 'Leave skipped'}</button>)}</div></article>)}</section>}
    {finished && sourceDeletions.length > 0 && <SourceDeletionPanel items={sourceDeletions} onResolve={onResolveSourceDeletions}/>}
    {progress.lastError && <div className="notice error"><strong>Last error</strong><p>{progress.lastError}</p></div>}
  </main></>
}

function ResumeDialog({ requirements, onClose, onConfirm, busy }: { requirements: ResumeRequirements; onClose: () => void; onConfirm: (passwords: ResumePasswords, remember: boolean) => void; busy: boolean }) {
  const services = requirements.credentials
  const [passwords, setPasswords] = useState<ResumePasswords>(() => Object.fromEntries(services.map(item => [item.kind, { source: '', destination: '' }])) as ResumePasswords)
  const [remember, setRemember] = useState(false)
  const valid = resumeCredentialsValid(requirements, passwords)
  const hasMissing = services.some(item => !item.sourceAvailable || !item.destinationAvailable)
  const update = (kind: ServiceKind, side: 'source' | 'destination', value: string) => setPasswords(current => ({ ...current, [kind]: { source: current[kind]?.source ?? '', destination: current[kind]?.destination ?? '', [side]: value } }))
  return <div className="dialog-backdrop" role="presentation" onMouseDown={event => { if (event.target === event.currentTarget) onClose() }}><section className="resume-dialog" role="dialog" aria-modal="true" aria-labelledby="resume-title"><div className="dialog-heading"><div><h2 id="resume-title">Delta sync</h2><p>Confirm credentials for migration #{requirements.migration.id}.</p></div><button className="dialog-close" aria-label="Close" onClick={onClose}>×</button></div><form onSubmit={event => { event.preventDefault(); if (valid) onConfirm(passwords, remember) }}>{services.map((item, index) => <fieldset key={item.kind}><legend>{serviceLabels[item.kind]}</legend>{item.sourceAvailable ? <div className="stored-credential"><span>Source</span><strong>Available in the credential store</strong></div> : <label>Source<input autoFocus={index === 0} type="password" autoComplete="current-password" value={passwords[item.kind]?.source ?? ''} onChange={event => update(item.kind, 'source', event.target.value)}/></label>}{item.destinationAvailable ? <div className="stored-credential"><span>Destination</span><strong>Available in the credential store</strong></div> : <label>Destination<input autoFocus={index === 0 && item.sourceAvailable} type="password" autoComplete="current-password" value={passwords[item.kind]?.destination ?? ''} onChange={event => update(item.kind, 'destination', event.target.value)}/></label>}</fieldset>)}{hasMissing && <label className="check dialog-remember"><input type="checkbox" checked={remember} onChange={event => setRemember(event.target.checked)}/><span><strong>Save new passwords in the credential store</strong><small>Used for the next delta sync.</small></span></label>}<div className="dialog-actions"><button type="button" className="secondary" onClick={onClose}>Cancel</button><button type="submit" className="primary" disabled={!valid || busy}>{busy && <span className="spinner"/>}{busy ? 'Starting sync' : 'Start delta sync'}</button></div></form></section></div>
}

export default function App() {
  const [view, setView] = useState<AppView>('connections')
  const [davAlphaEnabled, setDAVAlphaEnabled] = useState(false)
  const [enabled, setEnabled] = useState<Record<ServiceKind, boolean>>({ mail: true, calendar: false, contacts: false })
  const [active, setActive] = useState<ServiceKind>('mail')
  const [mailSource, setMailSource] = useState(emptyMail); const [mailDestination, setMailDestination] = useState(emptyMail)
  const [calendarSource, setCalendarSource] = useState(emptyDAV); const [calendarDestination, setCalendarDestination] = useState(emptyDAV)
  const [contactSource, setContactSource] = useState(emptyDAV); const [contactDestination, setContactDestination] = useState(emptyDAV)
  const [statuses, setStatuses] = useState<Record<string, SideStatus>>({})
  const [testing, setTesting] = useState(''); const [preflight, setPreflight] = useState<JobPreflightResult>()
  const [preflightDirty, setPreflightDirty] = useState(true); const [mappingWarnings, setMappingWarnings] = useState<string[]>([])
  const [mailMappings, setMailMappings] = useState<FolderMapping[]>([]); const [calendarMappings, setCalendarMappings] = useState<CollectionMapping[]>([]); const [contactMappings, setContactMappings] = useState<CollectionMapping[]>([])
  const [options, setOptions] = useState(fallbackOptions); const [advanced, setAdvanced] = useState(false); const [busy, setBusy] = useState(false); const [error, setError] = useState('')
  const [progress, setProgress] = useState<Progress>(); const [paused, setPaused] = useState(false); const [history, setHistory] = useState<RecentMigration[]>([]); const [conflicts, setConflicts] = useState<Conflict[]>([]); const [mailIssues, setMailIssues] = useState<MailIssue[]>([]); const [sourceDeletions, setSourceDeletions] = useState<SourceDeletion[]>([]); const [resuming, setResuming] = useState<number>(); const [preparingResume, setPreparingResume] = useState<number>(); const [resumeTarget, setResumeTarget] = useState<ResumeRequirements>(); const [showStartupNotice, setShowStartupNotice] = useState(true); const [updateInfo, setUpdateInfo] = useState<UpdateInfo>()
  const [resetTarget, setResetTarget] = useState<ResetKind>(); const [resetting, setResetting] = useState<ResetKind>()

  const davEndpoints = (kind: ServiceKind) => kind === 'calendar' ? [calendarSource, calendarDestination] as const : [contactSource, contactDestination] as const
  const currentMail = [mailSource, mailDestination] as const; const currentDAV = davEndpoints(active)
  const refreshHistory = () => backend.recent().then(setHistory).catch(() => undefined)
  const refreshConflicts = (id: number) => backend.conflicts(id).then(setConflicts).catch(() => undefined)
  const refreshMailIssues = (id: number) => backend.mailIssues(id).then(setMailIssues).catch(() => undefined)
  const refreshSourceDeletions = (id: number) => backend.sourceDeletions(id).then(items => { setSourceDeletions(items); return items }).catch(() => [] as SourceDeletion[])
  useEffect(() => { backend.defaults().then(value => setOptions({ ...value, verificationMode: value.verificationMode || (value.verifyAfter ? 'full_hash' : 'metadata') })).catch(() => undefined); backend.checkForUpdate().then(setUpdateInfo).catch(() => undefined); refreshHistory(); return backend.onProgress(next => { setProgress(next); setView('transfer'); setPaused(next.state === 'PAUSED'); refreshConflicts(next.migrationId); if (['COMPLETED', 'COMPLETED_WITH_ERRORS', 'FAILED', 'CANCELLED'].includes(next.state)) { refreshHistory(); refreshMailIssues(next.migrationId); refreshSourceDeletions(next.migrationId) } }) }, [])
  useEffect(() => { if (!error) return; toast.error('Operation failed', { description: error.replace(/^Error:\s*/, '') }); setError('') }, [error])

  const requestDAV = (kind: ServiceKind, mappings: CollectionMapping[]): DAVServiceRequest => { const endpoints = davEndpoints(kind); return buildDAVServiceRequest(kind, davAlphaEnabled, enabled[kind], endpoints[0], endpoints[1], mappings) }
  const jobRequest = (migrationId?: number): StartJobRequest => ({ mailEnabled: enabled.mail, mailSource, mailDestination, mailMappings, calendar: requestDAV('calendar', calendarMappings), contacts: requestDAV('contacts', contactMappings), options, migrationId })
  const canAnalyse = useMemo(() => (Object.keys(enabled) as ServiceKind[]).some(kind => enabled[kind]) && (Object.keys(enabled) as ServiceKind[]).every(kind => { if (!enabled[kind]) return true; if (kind === 'mail') return mailSource.host && mailSource.username && mailSource.password && mailDestination.host && mailDestination.username && mailDestination.password; const endpoints = davEndpoints(kind); return endpoints.every(endpoint => endpoint.username && endpoint.password && (endpoint.url || endpoint.username.includes('@'))) }), [enabled, mailSource, mailDestination, calendarSource, calendarDestination, contactSource, contactDestination])

  async function test(side: 'source' | 'destination') {
    const key = `${active}-${side}`
    setTesting(key)
    setError('')
    try {
      const status = active === 'mail'
        ? await backend.testMail(currentMail[side === 'source' ? 0 : 1])
        : await backend.testDAV(active, currentDAV[side === 'source' ? 0 : 1])
      setStatuses(value => ({ ...value, [key]: status }))
      toast.success(`${side === 'source' ? 'Source' : 'Destination'} connection checked successfully.`)
    } catch (cause) {
      setError(String(cause))
    } finally {
      setTesting('')
    }
  }
  async function analyse(openSelection = true) {
    setBusy(true)
    setError('')
    try {
      const result = await backend.analyseJob({ mailEnabled: enabled.mail, mailSource, mailDestination, calendar: requestDAV('calendar', []), contacts: requestDAV('contacts', []) })
      const mail = mergeMailMappings(result.mail?.mappings ?? [], mailMappings, result.mail?.destination.mailboxes ?? [])
      const calendar = mergeDAVMappings('calendar', result.calendar?.mappings ?? [], calendarMappings, result.calendar?.destination.collections ?? [])
      const contacts = mergeDAVMappings('contacts', result.contacts?.mappings ?? [], contactMappings, result.contacts?.destination.collections ?? [])
      setPreflight(result)
      setMailMappings(mail.mappings)
      setCalendarMappings(calendar.mappings)
      setContactMappings(contacts.mappings)
      setMappingWarnings([...mail.warnings, ...calendar.warnings, ...contacts.warnings])
      setPreflightDirty(false)
      if (openSelection) setView('selection')
      return true
    } catch (cause) {
      setError(String(cause))
      return false
    } finally {
      setBusy(false)
    }
  }
  async function start() { setBusy(true); setError(''); try { const id = await backend.startJob(jobRequest()); const total = (preflight?.mail?.source.messages ?? 0) + (preflight?.calendar?.source.objects ?? 0) + (preflight?.contacts?.source.objects ?? 0); const bytes = (preflight?.mail?.source.bytes ?? 0) + (preflight?.calendar?.source.bytes ?? 0) + (preflight?.contacts?.source.bytes ?? 0); setProgress({ migrationId: id, state: 'RUNNING', currentFolder: 'Preparing migration', currentUid: 0, messagesTotal: total, messagesCopied: 0, messagesFailed: 0, bytesTotal: bytes, bytesCopied: 0, bytesPerSecond: 0, startedAt: new Date().toISOString(), services: [], runMode: 'migration', runPhase: 'Preparation', runItemsTotal: total, runItemsDone: 0 }); setSourceDeletions([]); setView('transfer') } catch (cause) { setError(String(cause)) } finally { setBusy(false) } }
  async function prepareResume(id: number) { setPreparingResume(id); setError(''); try { setResumeTarget(await backend.resumeRequirements(id)) } catch (cause) { setError(String(cause)) } finally { setPreparingResume(undefined) } }
  async function resume(requirements: ResumeRequirements, passwords: ResumePasswords, remember: boolean) { const item = requirements.migration; setResuming(item.id); setError(''); try { const id = await backend.resumeJob(buildResumeJobRequest(requirements, passwords, remember)); setResumeTarget(undefined); setProgress({ migrationId: id, state: 'RUNNING', currentFolder: 'Reconciling source and destination', currentUid: 0, messagesTotal: item.messagesTotal, messagesCopied: item.messagesCopied, messagesFailed: item.messagesFailed, bytesTotal: item.bytesTotal, bytesCopied: item.bytesCopied, bytesPerSecond: 0, startedAt: new Date().toISOString(), services: [], runMode: 'reconcile', runPhase: 'Inventory', runItemsTotal: 0, runItemsDone: 0, runIndeterminate: true }); setSourceDeletions([]); setView('transfer') } catch (cause) { setError(String(cause)) } finally { setResuming(undefined) } }
  async function performReset(kind: ResetKind) {
    setResetting(kind)
    setError('')
    try {
      if (kind === 'factory') {
        await backend.factoryReset()
        return
      }
      await backend.resetMigrationData()
      setPreflight(undefined); setPreflightDirty(true); setMappingWarnings([]); setMailMappings([]); setCalendarMappings([]); setContactMappings([])
      setProgress(undefined); setPaused(false); setHistory([]); setConflicts([]); setMailIssues([]); setSourceDeletions([]); setResumeTarget(undefined); setView('connections')
      setResetTarget(undefined)
      toast.success('Local migration data was reset.', { description: 'Connection details and saved passwords were kept.' })
    } catch (cause) {
      setError(String(cause))
    } finally {
      setResetting(undefined)
    }
  }
  function reset() { if (progress) void backend.discardSourceDeletionCredential(progress.migrationId); setPreflight(undefined); setPreflightDirty(true); setMappingWarnings([]); setProgress(undefined); setView('connections'); setConflicts([]); setMailIssues([]); setSourceDeletions([]); setStatuses({}); setOptions(current => ({ ...current, excludedKeywords: [] })); setError(''); refreshHistory() }
  function markConfigurationChanged() { setPreflightDirty(true); setMappingWarnings([]) }
  function disableDAVAlpha() { setDAVAlphaEnabled(false); setEnabled(disabledDAVServices); setActive('mail'); setCalendarSource(emptyDAV()); setCalendarDestination(emptyDAV()); setContactSource(emptyDAV()); setContactDestination(emptyDAV()); setCalendarMappings([]); setContactMappings([]); setStatuses(current => Object.fromEntries(Object.entries(current).filter(([key]) => !key.startsWith('calendar-') && !key.startsWith('contacts-')))); setPreflight(current => current ? { ...current, calendar: undefined, contacts: undefined } : current); markConfigurationChanged() }
  function toggleService(kind: ServiceKind, value: boolean) { setEnabled(current => ({ ...current, [kind]: value })); markConfigurationChanged(); if (!value && active === kind) setActive((Object.keys(enabled) as ServiceKind[]).find(candidate => candidate !== kind && enabled[candidate]) ?? 'mail') }
  async function navigate(next: AppView) {
    if (next === view) return
    if (navigationLocked(progress) && next !== 'transfer') return
    if (next === 'selection') {
      if (!canAnalyse) {
        setError('Enter credentials for every selected service first.')
        return
      }
      if (!preflight || preflightDirty) {
        await analyse(true)
        return
      }
    }
    if (next === 'transfer' && !progress) return
    setView(next)
  }
  const navigation: HeaderProps = { view, selectionAvailable: Boolean(preflight) || canAnalyse, transferAvailable: Boolean(progress), locked: navigationLocked(progress), onNavigate: next => { void navigate(next) } }
  const resumeDialog = resumeTarget && <ResumeDialog key={resumeTarget.migration.id} requirements={resumeTarget} busy={resuming === resumeTarget.migration.id} onClose={() => { if (!resuming) setResumeTarget(undefined) }} onConfirm={(passwords, remember) => resume(resumeTarget, passwords, remember)}/>
  const resetDialog = resetTarget && <ResetDialog kind={resetTarget} busy={resetting === resetTarget} onClose={() => { if (!resetting) setResetTarget(undefined) }} onConfirm={() => { void performReset(resetTarget) }}/>
  const overlays = <><Toaster richColors closeButton theme="system" position="bottom-right"/>{resumeDialog}{resetDialog}{showStartupNotice && <StartupNotice update={updateInfo} onViewRelease={() => { void backend.openLatestRelease() }} onClose={() => setShowStartupNotice(false)}/>}</>

  if (view === 'transfer' && progress) return <><ProgressView navigation={navigation} progress={progress} conflicts={conflicts} mailIssues={mailIssues} sourceDeletions={sourceDeletions} paused={paused} onPause={async () => { paused ? await backend.continue(progress.migrationId) : await backend.pause(progress.migrationId); setPaused(!paused) }} onCancel={() => backend.cancel(progress.migrationId)} onNew={reset} onDelta={() => { if (!davAlphaEnabled && usesDAV(progress.services?.map(service => service.kind))) { toast.warning('Enable DAV Alpha under Advanced settings before starting this delta sync.'); return } void prepareResume(progress.migrationId) }} onExport={() => backend.exportReport(progress.migrationId)} onSupport={() => backend.exportSupport(progress.migrationId)} onResolve={async (id, resolution) => { await backend.resolveConflict(id, resolution); refreshConflicts(progress.migrationId) }} onResolveMailIssue={async (id, resolution) => { try { await backend.resolveMailIssue(id, resolution); setProgress(current => current ? { ...current, messagesFailed: Math.max(0, current.messagesFailed - 1), services: current.services?.map(service => service.kind === 'mail' ? { ...service, itemsFailed: Math.max(0, service.itemsFailed - 1), itemsQuarantined: resolution === 'transfer_anyway' ? Math.max(0, (service.itemsQuarantined ?? 0) - 1) : service.itemsQuarantined, itemsUnknown: resolution === 'verify_again' ? Math.max(0, (service.itemsUnknown ?? 0) - 1) : service.itemsUnknown } : service) } : current); await refreshMailIssues(progress.migrationId) } catch (cause) { setError(String(cause)) } }} onResolveSourceDeletions={async actions => { try { await backend.resolveSourceDeletions({ migrationId: progress.migrationId, actions }); const remaining = await refreshSourceDeletions(progress.migrationId); if (remaining.some(item => item.lastError)) toast.warning('Some destination messages were left unchanged.', { description: 'Details are shown with the affected messages.' }); else toast.success('Destination message processing completed.') } catch (cause) { await refreshSourceDeletions(progress.migrationId); setError(String(cause)) } }}/>{overlays}</>
  if (view === 'selection' && preflight) return <><MappingView navigation={navigation} preflight={preflight} mappingWarnings={mappingWarnings} mailMappings={mailMappings} setMailMappings={setMailMappings} calendarMappings={calendarMappings} setCalendarMappings={setCalendarMappings} contactMappings={contactMappings} setContactMappings={setContactMappings} options={options} setOptions={setOptions} onBack={() => { void navigate('connections') }} onStart={start} busy={busy}/>{overlays}</>

  const sourceDAV = active === 'calendar' ? calendarSource : contactSource; const destinationDAV = active === 'calendar' ? calendarDestination : contactDestination
  return <><Header {...navigation}/><main className="workspace"><div className="workspace-heading"><div><h1>Connections</h1><p>Select data types and check the source and destination accounts.</p></div></div>
    <ServiceSelector services={visibleServices(davAlphaEnabled)} enabled={enabled} onChange={toggleService} active={active} onActive={setActive}/>
    <section className="connection-workspace">
      <ConnectionPanel title="Source" active={active} mail={mailSource} dav={sourceDAV} onMail={value => { setMailSource(value); setOptions(current => ({ ...current, excludedKeywords: [] })); markConfigurationChanged(); setStatuses(current => ({ ...current, [`${active}-source`]: undefined })) }} onDAV={value => { active === 'calendar' ? setCalendarSource(value) : setContactSource(value); markConfigurationChanged(); setStatuses(current => ({ ...current, [`${active}-source`]: undefined })) }} status={statuses[`${active}-source`]} busy={testing === `${active}-source`} onTest={() => test('source')}/>
      <div className="route"><span><Icon name="arrow"/></span></div>
      <ConnectionPanel title="Destination" active={active} mail={mailDestination} dav={destinationDAV} onMail={value => { setMailDestination(value); markConfigurationChanged(); setStatuses(current => ({ ...current, [`${active}-destination`]: undefined })) }} onDAV={value => { active === 'calendar' ? setCalendarDestination(value) : setContactDestination(value); markConfigurationChanged(); setStatuses(current => ({ ...current, [`${active}-destination`]: undefined })) }} status={statuses[`${active}-destination`]} busy={testing === `${active}-destination`} onTest={() => test('destination')}/>
    </section>
    <section className="action-bar"><div className="readiness"><span className={canAnalyse ? 'ready' : ''}><i/>Configuration</span><small>{canAnalyse ? preflight && !preflightDirty ? 'The preflight result is current. You can review the selection.' : 'Every selected service is configured.' : 'Enter credentials for every selected service.'}</small></div><button className="primary" disabled={!canAnalyse || busy} onClick={() => { void navigate('selection') }}>{busy && <span className="spinner"/>}{busy ? 'Running preflight' : preflight && !preflightDirty ? 'Review selection' : 'Run preflight'}</button></section>
    <section className="settings-section"><button className="disclosure" onClick={() => setAdvanced(!advanced)} aria-expanded={advanced}><span>Advanced settings</span><span>{advanced ? '−' : '+'}</span></button>{advanced && <div className="advanced-panel"><label>Concurrency<select value={options.concurrency} onChange={event => { setOptions({ ...options, concurrency: Number(event.target.value) }); markConfigurationChanged() }}>{[1, 2, 4, 6, 8].map(value => <option key={value}>{value}</option>)}</select></label><label>Maximum attempts<input type="number" min={1} max={20} value={options.maximumRetries} onChange={event => { setOptions({ ...options, maximumRetries: Number(event.target.value) }); markConfigurationChanged() }}/></label><label>Connection timeout<input type="number" min={5} value={options.connectionTimeout} onChange={event => { setOptions({ ...options, connectionTimeout: Number(event.target.value) }); markConfigurationChanged() }}/><small>Seconds</small></label><label>Stall timeout<input type="number" min={30} value={options.stallTimeout} onChange={event => { setOptions({ ...options, stallTimeout: Number(event.target.value) }); markConfigurationChanged() }}/><small>Seconds</small></label><label>Mail verification<span className="advanced-value">Full SHA-256 comparison</span><small>The source and destination are read in full.</small></label><label className="alpha-opt-in"><input type="checkbox" checked={davAlphaEnabled} onChange={event => event.target.checked ? setDAVAlphaEnabled(true) : disableDAVAlpha()}/><span><strong>Enable CalDAV and CardDAV (Alpha)</strong><small>This unfinished feature may fail and should not be used for critical migrations.</small></span></label><section className="reset-settings" aria-labelledby="reset-settings-title"><div><strong id="reset-settings-title">Reset and recovery</strong><small>Use these options when local migration state prevents the app from working correctly.</small></div><div><button className="secondary" disabled={busy || Boolean(testing) || resetting !== undefined || navigationLocked(progress)} onClick={() => setResetTarget('migrations')}>Reset migration data</button><button className="danger" disabled={busy || Boolean(testing) || resetting !== undefined || navigationLocked(progress)} onClick={() => setResetTarget('factory')}>Reset entire app</button></div></section></div>}</section>
    {history.length > 0 && <section className="history"><div className="section-heading"><h2>Recent migrations</h2><span>{history.length} entries</span></div><div className="history-list">{history.map(item => { const davLocked = !davAlphaEnabled && usesDAV(item.services); const migratedAt = item.finishedAt ?? item.createdAt; return <article key={item.id}><span className={`history-state-dot ${item.state.toLowerCase()}`}><i/></span><div className="history-main"><strong>{item.sourceHost} <span>→</span> {item.destinationHost}</strong>{(item.sourceUsername || item.destinationUsername) && <span className="history-accounts">{item.sourceUsername || '—'} <span>→</span> {item.destinationUsername || '—'}</span>}<small>{new Date(migratedAt).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' })} · {(item.services ?? ['mail']).map(kind => serviceLabels[kind]).join(', ')} · {formatNumber(item.messagesCopied)} / {formatNumber(item.messagesTotal)} objects</small></div><span className="history-state">{humanState(item.state)}</span>{resumableStates.includes(item.state) && <button className="text-button" title={davLocked ? 'Enable DAV Alpha under Advanced settings first.' : undefined} disabled={davLocked || resuming !== undefined || preparingResume !== undefined} onClick={() => { void prepareResume(item.id) }}>{preparingResume === item.id ? 'Preparing...' : 'Delta sync'}</button>}<button className="icon-button" title="Export report" onClick={() => backend.exportReport(item.id)}><Icon name="download"/></button></article> })}</div></section>}
  </main>{overlays}</>
}
