/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { LogOtherData, UpstreamResponseAudit } from '../../types'
import { DetailsDialog } from '../dialogs/details-dialog'

const queryClients: QueryClient[] = []

function makeLog(other: LogOtherData): UsageLog {
  return {
    id: 1,
    user_id: 1,
    created_at: 1,
    type: 2,
    content: '',
    username: 'user',
    token_name: 'token',
    model_name: 'gpt-5',
    quota: 1,
    prompt_tokens: 1,
    completion_tokens: 1,
    use_time: 1,
    is_stream: true,
    channel: 1,
    channel_name: 'channel',
    token_id: 1,
    group: 'default',
    ip: '',
    other: JSON.stringify(other),
    request_id: 'req-1',
    upstream_request_id: '',
  }
}

function renderDetails(ursp: UpstreamResponseAudit, isRoot: boolean): void {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(['status'], {}, { updatedAt: Date.now() + 60_000 })
  queryClients.push(queryClient)
  render(
    <QueryClientProvider client={queryClient}>
      <DetailsDialog
        log={makeLog({ root_info: { ursp } })}
        isAdmin
        isRoot={isRoot}
        open
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
}

afterEach(() => {
  for (const queryClient of queryClients) {
    queryClient.clear()
  }
  queryClients.length = 0
})

const replayed: UpstreamResponseAudit = {
  sent: 'gpt-5',
  sent_field: 'gpt-5-mapped',
  model: 'openai/gpt-5-mini',
  prefix: 'openai',
  mismatch: false,
  kind: 'vendor_prefix',
  obs: 'full',
  h: 'openai_chat_stream',
  mid_stream_change: true,
  models: ['gpt-5', 'gpt-5-mini'],
  replay: true,
  dup_id: true,
  stale_created: true,
  extra_keys: ['api_key_id', 'provider'],
  pt_ratio: 1.25,
  retry: 1,
}

describe('upstream response audit details', () => {
  test('shows replay, mid-stream and authenticity signals to root', () => {
    renderDetails(replayed, true)

    expect(screen.getByText(/Suspected replay/)).toBeInTheDocument()
    expect(
      screen.getByText(
        'The upstream declared more than one model within this stream'
      )
    ).toBeInTheDocument()
    expect(screen.getByText('gpt-5 → gpt-5-mini')).toBeInTheDocument()
    expect(screen.getByText('gpt-5-mapped')).toBeInTheDocument()
    expect(
      screen.getByText('Observed · openai_chat_stream')
    ).toBeInTheDocument()
    expect(
      screen.getByText('Duplicate response ID, Stale created timestamp')
    ).toBeInTheDocument()
    expect(screen.getByText('api_key_id, provider')).toBeInTheDocument()
    expect(screen.getByText('1.25×')).toBeInTheDocument()
  })

  test('labels a pinned build answered by the bare alias without a mismatch warning', () => {
    renderDetails(
      {
        sent: 'deepseek-v4-pro-0813',
        model: 'deepseek-v4-pro',
        mismatch: false,
        kind: 'pinned_to_alias',
      },
      true
    )

    expect(
      screen.getByText(
        'Pinned build answered by the bare alias (pin not honoured)'
      )
    ).toBeInTheDocument()
    expect(
      screen.queryByText(
        'Upstream declared a different model than the one sent'
      )
    ).toBeNull()
  })

  test('shows a version downgrade next to the mismatch verdict', () => {
    renderDetails(
      {
        sent: 'gemini-3.7-flash',
        model: '3.1-flash-lite-preview-03-2026',
        mismatch: true,
        kind: 'mismatch',
        tier_downgrade: true,
        version_downgrade: true,
      },
      true
    )

    expect(
      screen.getByText('Older model version than the one sent')
    ).toBeInTheDocument()
  })

  test('hides the audit block from non-root viewers', () => {
    renderDetails(replayed, false)

    expect(screen.queryByText(/Suspected replay/)).toBeNull()
    expect(screen.queryByText('api_key_id, provider')).toBeNull()
  })
})
