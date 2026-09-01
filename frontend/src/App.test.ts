import { describe, expect, it } from 'vitest'
import { buildDAVServiceRequest, buildSourceDeletionActions, disabledDAVServices, displayedProgress, isTechnicalKeyword, keywordCountForMappings, usesDAV, visibleServices } from './App'
import type { FolderMapping, SourceDeletion } from './types'

describe('displayedProgress', () => {
  it('ignores historical totals during an indeterminate delta inventory', () => {
    expect(displayedProgress({
      state: 'RUNNING', runMode: 'reconcile', runItemsTotal: 0, runItemsDone: 0, runIndeterminate: true,
      messagesTotal: 100, messagesCopied: 100,
    })).toEqual({ percent: 0, done: 0, total: 0, indeterminate: true, runBased: true })
  })

  it('uses monotone current-run counters and reaches 100 only on completion', () => {
    expect(displayedProgress({ state: 'RUNNING', runMode: 'reconcile', runItemsTotal: 4, runItemsDone: 2, messagesTotal: 100, messagesCopied: 100 }).percent).toBe(50)
    expect(displayedProgress({ state: 'RUNNING', runMode: 'reconcile', runItemsTotal: 4, runItemsDone: 4, messagesTotal: 100, messagesCopied: 100 }).percent).toBe(99.9)
    expect(displayedProgress({ state: 'COMPLETED', runMode: 'reconcile', runItemsTotal: 4, runItemsDone: 4, messagesTotal: 100, messagesCopied: 100 }).percent).toBe(100)
  })
})

describe('source deletion choices', () => {
  it('submits only explicit mixed decisions', () => {
    const items = [{ id: 1 }, { id: 2 }, { id: 3 }] as SourceDeletion[]
    expect(buildSourceDeletionActions(items, { 1: 'keep', 2: '', 3: 'delete' })).toEqual([
      { id: 1, resolution: 'keep' },
      { id: 3, resolution: 'delete' },
    ])
  })
})

describe('mail keyword selection', () => {
  it('counts a keyword only in enabled source folders', () => {
    const mappings = [
      { enabled: true, source: { name: 'INBOX' } },
      { enabled: false, source: { name: 'Archive' } },
    ] as FolderMapping[]
    expect(keywordCountForMappings({ name: 'Project', occurrences: { INBOX: 3, Archive: 8 } }, mappings)).toBe(3)
  })

  it('marks attachment index keywords as technical', () => {
    expect(isTechnicalKeyword('$HasNoAttachment')).toBe(true)
    expect(isTechnicalKeyword('$MailFlagBit2')).toBe(true)
    expect(isTechnicalKeyword('Customer-A')).toBe(false)
  })
})

describe('DAV Alpha gate', () => {
  it('shows only mail before the session opt-in', () => {
    expect(visibleServices(false)).toEqual(['mail'])
    expect(visibleServices(true)).toEqual(['mail', 'calendar', 'contacts'])
  })

  it('disables DAV and restores mail when the opt-in is removed', () => {
    expect(disabledDAVServices({ mail: false, calendar: true, contacts: true })).toEqual({ mail: true, calendar: false, contacts: false })
  })

  it('does not send DAV mappings while the Alpha gate is closed', () => {
    const endpoint = { url: 'https://dav.example.test', username: 'user', password: 'secret', authMethod: 'auto' as const, rememberCredential: false }
    const mappings = [{ enabled: true, source: { path: '/calendar', name: 'Calendar', kind: 'calendar' as const, objects: 1, bytes: 10, components: ['VEVENT'], contentTypes: ['text/calendar'] }, destinationPath: '/target', destinationName: 'Target', destinationExists: true }]
    expect(buildDAVServiceRequest('calendar', false, true, endpoint, endpoint, mappings)).toMatchObject({ enabled: false, source: { url: '', username: '', password: '' }, destination: { url: '', username: '', password: '' }, mappings: [] })
    expect(buildDAVServiceRequest('calendar', true, true, endpoint, endpoint, mappings)).toMatchObject({ enabled: true, mappings })
  })

  it('identifies DAV histories that need a renewed opt-in', () => {
    expect(usesDAV(['mail'])).toBe(false)
    expect(usesDAV(['mail', 'contacts'])).toBe(true)
  })
})
