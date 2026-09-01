import type {
  CollectionMapping, DAVCollection, DAVPreflightResult, FolderMapping, JobPreflightResult, Mailbox, Progress, ServiceKind,
} from './types'

export const NEW_DESTINATION = '__new_destination__'

const key = (value: string) => value.trim().toLocaleLowerCase()

export function selectableMailboxes(mailboxes: Mailbox[]): Mailbox[] {
  return mailboxes.filter(mailbox => mailbox.selectable)
}

export function chooseMailDestination(mapping: FolderMapping, value: string, destinations: Mailbox[]): FolderMapping {
  if (value === NEW_DESTINATION) {
    return { ...mapping, destinationExists: false }
  }
  const target = selectableMailboxes(destinations).find(mailbox => mailbox.name === value)
  if (!target) return { ...mapping, destinationExists: false }
  return {
    ...mapping,
    destinationName: target.name,
    destinationDelimiter: target.delimiter || mapping.destinationDelimiter || 47,
    destinationExists: true,
  }
}

export function chooseDAVDestination(mapping: CollectionMapping, value: string, destinations: DAVCollection[]): CollectionMapping {
  if (value === NEW_DESTINATION) {
    return { ...mapping, destinationPath: '', destinationExists: false }
  }
  const target = destinations.find(collection => collection.path === value)
  if (!target) return { ...mapping, destinationPath: '', destinationExists: false }
  return { ...mapping, destinationPath: target.path, destinationName: target.name, destinationExists: true }
}

export interface MergeResult<T> { mappings: T[]; warnings: string[] }

export function mergeMailMappings(fresh: FolderMapping[], previous: FolderMapping[], destinations: Mailbox[]): MergeResult<FolderMapping> {
  const oldBySource = new Map(previous.map(mapping => [mapping.source.name, mapping]))
  const targets = selectableMailboxes(destinations)
  const warnings: string[] = []
  const mappings = fresh.map(recommended => {
    const old = oldBySource.get(recommended.source.name)
    if (!old) return recommended
    const target = targets.find(mailbox => key(mailbox.name) === key(old.destinationName))
    if (target) {
      return {
        ...recommended,
        enabled: old.enabled,
        destinationName: target.name,
        destinationDelimiter: target.delimiter || recommended.destinationDelimiter,
        destinationExists: true,
      }
    }
    if (old.destinationExists && old.destinationName) {
      warnings.push(`The previous mail destination "${old.destinationName}" was not found and will be treated as a new folder.`)
    }
    return {
      ...recommended,
      enabled: old.enabled,
      destinationName: old.destinationName || recommended.destinationName,
      destinationExists: false,
    }
  })
  return { mappings, warnings }
}

export function mergeDAVMappings(kind: ServiceKind, fresh: CollectionMapping[], previous: CollectionMapping[], destinations: DAVCollection[]): MergeResult<CollectionMapping> {
  const oldBySource = new Map(previous.map(mapping => [`${mapping.source.kind}:${mapping.source.path}`, mapping]))
  const warnings: string[] = []
  const mappings = fresh.map(recommended => {
    const old = oldBySource.get(`${kind}:${recommended.source.path}`)
    if (!old) return recommended
    const target = destinations.find(collection =>
      (old.destinationPath && collection.path === old.destinationPath) || key(collection.name) === key(old.destinationName),
    )
    if (target) {
      return {
        ...recommended,
        enabled: old.enabled,
        destinationPath: target.path,
        destinationName: target.name,
        destinationExists: true,
      }
    }
    if (old.destinationExists && old.destinationName) {
      warnings.push(`The previous ${kind === 'calendar' ? 'calendar' : 'contacts'} destination "${old.destinationName}" was not found and will be created again.`)
    }
    return {
      ...recommended,
      enabled: old.enabled,
      destinationPath: '',
      destinationName: old.destinationName || recommended.destinationName,
      destinationExists: false,
    }
  })
  return { mappings, warnings }
}

export function navigationLocked(progress?: Progress): boolean {
  return progress?.state === 'RUNNING' || progress?.state === 'PAUSED'
}

export interface DryRunCard {
  kind: ServiceKind
  title: string
  metrics: Array<{ value: number; label: string; bytes?: boolean }>
}

function davCard(kind: 'calendar' | 'contacts', result: DAVPreflightResult): DryRunCard {
  return {
    kind,
    title: kind === 'calendar' ? 'Calendar' : 'Contacts',
    metrics: [
      { value: result.objectsScanned ?? 0, label: kind === 'calendar' ? 'calendar objects checked' : 'contacts checked' },
      { value: result.conversions ?? 0, label: 'Konvertierungen' },
      { value: result.potentialConflicts ?? 0, label: 'possible destination conflicts' },
      { value: result.problems?.length ?? 0, label: 'problematische Objekte' },
    ],
  }
}

export function dryRunCards(preflight: JobPreflightResult, mailMappings: FolderMapping[]): DryRunCard[] {
  const cards: DryRunCard[] = []
  if (preflight.mail) {
    const selected = mailMappings.filter(mapping => mapping.enabled)
    cards.push({
      kind: 'mail',
      title: 'Mail',
      metrics: [
        { value: selected.length, label: 'folders selected' },
        { value: selected.reduce((sum, mapping) => sum + mapping.source.messages, 0), label: 'messages' },
        { value: selected.reduce((sum, mapping) => sum + mapping.source.size, 0), label: 'Datenmenge', bytes: true },
        { value: selected.reduce((sum, mapping) => sum + mapping.source.size, 0), label: 'additional full verification', bytes: true },
        { value: selected.filter(mapping => mapping.destinationExists).length, label: 'bestehende Ziele' },
        { value: selected.filter(mapping => !mapping.destinationExists).length, label: 'neue Ziele' },
      ],
    })
  }
  if (preflight.calendar) cards.push(davCard('calendar', preflight.calendar))
  if (preflight.contacts) cards.push(davCard('contacts', preflight.contacts))
  return cards
}
