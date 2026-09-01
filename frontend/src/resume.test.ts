import { describe, expect, it } from 'vitest'
import { buildResumeJobRequest, resumeCredentialsValid } from './resume'
import type { ResumeRequirements } from './types'

const requirements: ResumeRequirements = {
  migration: {
    id: 5, createdAt: '2026-08-31T10:45:18Z', state: 'COMPLETED', sourceHost: 'old.example', destinationHost: 'new.example',
    sourceUsername: 'source@example.com', destinationUsername: 'destination@example.com',
    messagesTotal: 36, messagesCopied: 36, messagesFailed: 0, bytesTotal: 305339, bytesCopied: 305339, services: ['mail'],
  },
  credentials: [{ kind: 'mail', sourceAvailable: true, destinationAvailable: false }],
}

describe('delta credentials', () => {
  it('requires only credentials missing from the keychain', () => {
    expect(resumeCredentialsValid(requirements, {})).toBe(false)
    expect(resumeCredentialsValid(requirements, { mail: { source: '', destination: 'secret' } })).toBe(true)
  })

  it('sends only optional replacement passwords with the existing migration id', () => {
    expect(buildResumeJobRequest(requirements, { mail: { source: '', destination: 'secret' } }, true)).toEqual({
      migrationId: 5,
      credentials: { mail: { sourcePassword: '', destinationPassword: 'secret' } },
      rememberNewCredentials: true,
    })
  })
})
