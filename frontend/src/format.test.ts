import { describe, expect, it } from 'vitest'
import { formatBytes, humanState } from './format'

describe('formatting', () => {
  it('formats transfer sizes without invalid values', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1024)).toContain('KB')
  })

  it('presents migration states in English', () => {
    expect(humanState('INTERRUPTED')).toBe('Interrupted')
  })
})
