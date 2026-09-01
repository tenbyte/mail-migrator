import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { ResetDialog } from './App'

describe('reset confirmation', () => {
  it('explains what a migration-data reset deletes and keeps', () => {
    const html = renderToStaticMarkup(<ResetDialog kind="migrations" busy={false} onClose={vi.fn()} onConfirm={vi.fn()}/>)
    expect(html).toContain('Reset migration data?')
    expect(html).toContain('Migration history and progress')
    expect(html).toContain('Saved passwords and the current connection form')
    expect(html).toContain('All data on source and destination servers')
    expect(html).not.toContain('Passwords saved by this app in the system credential store')
  })

  it('includes credentials in a factory reset and locks actions while running', () => {
    const html = renderToStaticMarkup(<ResetDialog kind="factory" busy onClose={vi.fn()} onConfirm={vi.fn()}/>)
    expect(html).toContain('Reset the entire app?')
    expect(html).toContain('Passwords saved by this app in the system credential store')
    expect(html).toContain('The app reloads immediately after the reset.')
    expect(html.match(/disabled=""/g)).toHaveLength(3)
  })
})
