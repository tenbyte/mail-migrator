import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'
import { MailboxNoticeDialog, MailboxNoticePreview, MailboxNoticeSettingsView } from './App'

describe('mailbox notices', () => {
  it('renders custom text as text in the preview', () => {
    const html = renderToStaticMarkup(<MailboxNoticePreview side="source" customText={'Server: mail.example\n<script>alert(1)</script>'}/>)
    expect(html).toContain('Your mailbox has been migrated')
    expect(html).toContain('Server: mail.example\n&lt;script&gt;alert(1)&lt;/script&gt;')
    expect(html).not.toContain('<script>')
  })

  it('shows independent settings, character limits, and an explicit save action', () => {
    const html = renderToStaticMarkup(<MailboxNoticeSettingsView settings={{ source: { enabled: true, customText: 'Source details' }, destination: { enabled: false, customText: '' } }} dirty busy={false} onChange={vi.fn()} onSave={vi.fn()} onBack={vi.fn()}/>)
    expect(html).toContain('Notice in source mailbox')
    expect(html).toContain('Notice in destination mailbox')
    expect(html.match(/max[Ll]ength="10000"/g)).toHaveLength(2)
    expect(html).toContain('Save settings')
  })

  it('requires an explicit risk confirmation for completed migrations with errors', () => {
    const html = renderToStaticMarkup(<MailboxNoticeDialog overview={{ migrationId: 7, migrationState: 'COMPLETED_WITH_ERRORS', eligible: true, requiresErrorConfirmation: true, sourceCredentialAvailable: true, destinationCredentialAvailable: true, notices: [{ side: 'destination', enabled: true, subject: 'Please check your migrated mailbox', status: 'pending' }] }} busy={false} onClose={vi.fn()} onConfirm={vi.fn()}/>)
    expect(html).toContain('This migration completed with errors')
    expect(html).toContain('disabled=""')
  })
})
