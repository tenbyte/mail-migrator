import { describe, expect, it } from 'vitest'
import {
  NEW_DESTINATION, chooseDAVDestination, chooseMailDestination, dryRunCards, mergeDAVMappings, mergeMailMappings,
  navigationLocked, selectableMailboxes,
} from './mapping'
import type { CollectionMapping, FolderMapping, JobPreflightResult, Mailbox } from './types'

const mailbox = (name: string, selectable = true): Mailbox => ({ name, delimiter: 47, attributes: [], selectable, messages: 2, uidValidity: 1, uidNext: 3, size: 20, sizeKnown: true })
const mailMapping = (source = 'INBOX', destination = 'INBOX', exists = true): FolderMapping => ({ source: mailbox(source), destinationName: destination, destinationDelimiter: 47, destinationExists: exists, enabled: true })
const collectionMapping = (path = '/source/', destinationPath = '/target/'): CollectionMapping => ({ source: { path, name: 'Privat', kind: 'calendar', components: ['VEVENT'], contentTypes: [], objects: 2, bytes: 20 }, destinationPath, destinationName: 'Privat', destinationExists: Boolean(destinationPath), enabled: true })

describe('destination selection', () => {
  it('excludes non-selectable IMAP containers', () => {
    expect(selectableMailboxes([mailbox('INBOX'), mailbox('Container', false)]).map(item => item.name)).toEqual(['INBOX'])
  })

  it('sets existing and new mail targets correctly', () => {
    const targets = [mailbox('INBOX')]
    expect(chooseMailDestination(mailMapping('Inbox', 'Neu', false), 'INBOX', targets).destinationExists).toBe(true)
    expect(chooseMailDestination(mailMapping(), NEW_DESTINATION, targets).destinationExists).toBe(false)
  })

  it('sets existing and new DAV targets correctly', () => {
    const target = { ...collectionMapping().source, path: '/target/' }
    expect(chooseDAVDestination(collectionMapping('/source/', ''), '/target/', [target]).destinationPath).toBe('/target/')
    expect(chooseDAVDestination(collectionMapping(), NEW_DESTINATION, [target]).destinationPath).toBe('')
  })
})

describe('preflight refresh', () => {
  it('preserves manual mail mappings and turns vanished targets into new folders', () => {
    const merged = mergeMailMappings([mailMapping()], [{ ...mailMapping('INBOX', 'Alt'), enabled: false }], [mailbox('INBOX')])
    expect(merged.mappings[0]).toMatchObject({ enabled: false, destinationName: 'Alt', destinationExists: false })
    expect(merged.warnings).toHaveLength(1)
  })

  it('preserves DAV mappings by service and source path', () => {
    const previous = [{ ...collectionMapping(), enabled: false }]
    const target = { ...collectionMapping().source, path: '/target/' }
    const merged = mergeDAVMappings('calendar', [collectionMapping('/source/', '')], previous, [target])
    expect(merged.mappings[0]).toMatchObject({ enabled: false, destinationPath: '/target/', destinationExists: true })
  })
})

describe('navigation and dry run', () => {
  it('locks navigation only while running or paused', () => {
    expect(navigationLocked({ state: 'RUNNING' } as never)).toBe(true)
    expect(navigationLocked({ state: 'PAUSED' } as never)).toBe(true)
    expect(navigationLocked({ state: 'FAILED' } as never)).toBe(false)
  })

  it('does not emit DAV wording for a mail-only dry run', () => {
    const preflight = { mail: { source: {}, destination: {}, mappings: [], warnings: [] }, warnings: [] } as unknown as JobPreflightResult
    const labels = dryRunCards(preflight, [mailMapping()]).flatMap(card => card.metrics.map(metric => metric.label))
    expect(labels.join(' ')).not.toContain('DAV')
    expect(labels).toContain('messages')
  })
})
