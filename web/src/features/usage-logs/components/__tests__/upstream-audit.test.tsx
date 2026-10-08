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
    expect(screen.getByText('api_key_id')).toBeInTheDocument()
  })

  test('explains each extra response key and what the row can and cannot show', () => {
    renderDetails({ ...replayed, extra_keys: ['provider', 'turnToken'] }, true)

    expect(
      screen.getByText(
        /Provider name labelled by a relay \(some relays fake or mask it\)/
      )
    ).toBeInTheDocument()
    expect(screen.getByText('turnToken').parentElement).toHaveTextContent(
      'turnToken — Meaning unknown'
    )
    expect(
      screen.getByText(/not evidence of model authenticity/)
    ).toBeInTheDocument()
  })

  test('ignores the removed prompt-token ratio on log rows written before it was dropped', () => {
    // rows logged by the previous build still carry pt_ratio in root_info
    const legacyRow = { ...replayed, pt_ratio: 1.25 } as unknown as UpstreamResponseAudit
    renderDetails(legacyRow, true)

    expect(screen.queryByText('1.25×')).not.toBeInTheDocument()
    expect(screen.queryByText('Prompt Token Ratio')).not.toBeInTheDocument()
    expect(screen.getByText('api_key_id')).toBeInTheDocument()
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

  test('shows the backend clue with inline confidence and a highlighted conflict', () => {
    renderDetails(
      {
        sent: 'gpt-5.1',
        model: 'gpt-5.1',
        mismatch: false,
        kind: 'exact',
        src: 'newapi_relay',
        backend: 'azure',
        backend_by: 'body',
        backend_conf: 'medium',
        backend_conflict: ['openai:header', 'azure:body'],
      },
      true
    )

    expect(
      screen.getByText('Vendor-specific response body fields')
    ).toBeInTheDocument()
    expect(screen.getByText('Backend Clues')).toBeInTheDocument()
    expect(
      screen.getByText('Azure OpenAI (Medium evidence)')
    ).toBeInTheDocument()
    expect(
      screen.getByText('openai:header, azure:body').parentElement
    ).toHaveClass('text-amber-600')
  })

  test('labels a DeepSeek body claim as a format, never as official', () => {
    renderDetails(
      {
        sent: 'glm-5.3-flash',
        model: 'glm-5.3-flash',
        mismatch: false,
        kind: 'exact',
        backend: 'deepseek',
        backend_by: 'body',
        backend_conf: 'weak',
        backend_conflict: ['deepseek:body', 'glm:model_name'],
      },
      true
    )

    expect(
      screen.getByText('DeepSeek format (Weak evidence)')
    ).toBeInTheDocument()
    expect(screen.queryByText(/官方/)).toBeNull()
    expect(
      screen.getByText('deepseek:body, glm:model_name')
    ).toBeInTheDocument()
  })

  test('does not present a backend known only from the model name as identified', () => {
    renderDetails(
      {
        sent: 'gemini-3-pro',
        model: 'gemini-3-pro',
        mismatch: false,
        kind: 'exact',
        backend: 'aistudio',
        backend_by: 'model_name',
        backend_conf: 'weak',
      },
      true
    )

    expect(
      screen.getByText(
        'Backend not identified · model name points to Gemini (not independently verified)'
      )
    ).toBeInTheDocument()
    expect(screen.queryByText(/AI Studio/)).toBeNull()
  })

  test('labels a neutral fallback backend as unidentified', () => {
    renderDetails(
      {
        sent: 'gpt-5',
        model: 'gpt-5',
        mismatch: false,
        kind: 'exact',
        src: 'gpt_relay',
        backend: 'gpt_relay',
        backend_by: 'fallback',
        backend_conf: 'none',
      },
      true
    )

    expect(
      screen.getByText(
        'OpenAI-compatible (backend unidentified) (No vendor evidence)'
      )
    ).toBeInTheDocument()
    expect(screen.getByText('OpenAI-compatible relay')).toBeInTheDocument()
    expect(screen.queryByText('Conflicting Evidence')).toBeNull()
  })

  test('hides the audit block from non-root viewers', () => {
    renderDetails(replayed, false)

    expect(screen.queryByText(/Suspected replay/)).toBeNull()
    expect(screen.queryByText('api_key_id')).toBeNull()
  })
})
