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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { createAppQueryClient } from '@/lib/query-client'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import type { ChannelLimitUsage } from '../../api'
import {
  CHANNEL_LIMIT_USAGE_REFRESH_MS,
  CHANNEL_LIMIT_USAGE_RETRY_MS,
  limitedChannelIds,
} from '../../lib/channel-limit-usage'
import { channelSchema, type Channel } from '../../types'
import {
  ChannelLimitUsageCell,
  ChannelLimitUsageProvider,
  ChannelLimitUsageSource,
} from '../channel-limit-usage'
import { ChannelRowActionsLayoutContext } from '../channel-row-actions-context'
import { ChannelsProvider, useChannels } from '../channels-provider'

const originalAuth = useAuthStore.getState().auth
let client: QueryClient
let usage: ChannelLimitUsage[]
let usageRequests: string[]
let usageFails: boolean
let usageFailure: unknown
/** When set, reads never finish until it is called. */
let usageHang: Array<() => void> | null

function HideSensitive() {
  const { sensitiveVisible, setSensitiveVisible } = useChannels()
  return (
    <button type='button' onClick={() => setSensitiveVisible(false)}>
      {sensitiveVisible ? 'hide' : 'hidden'}
    </button>
  )
}

function channel(id: number, limits: Partial<Channel> = {}): Channel {
  return channelSchema.parse({
    id,
    name: `Channel ${id}`,
    type: 1,
    key: '',
    status: 1,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    ...limits,
  })
}

const limited = channel(227, { rpm_limit: 60, daily_quota_limit: 5_000_000 })
const unlimited = channel(172)

function cells(channels: Channel[], layout: 'table' | 'card' = 'table') {
  return (
    <QueryClientProvider client={client}>
      <ChannelsProvider>
        <HideSensitive />
        <ChannelLimitUsageProvider>
          <ChannelLimitUsageSource channels={channels} />
          <ChannelRowActionsLayoutContext.Provider value={layout}>
            {channels.map((item) => (
              <div key={item.id} data-testid={`cell-${item.id}`}>
                <ChannelLimitUsageCell channel={item} />
              </div>
            ))}
          </ChannelRowActionsLayoutContext.Provider>
        </ChannelLimitUsageProvider>
      </ChannelsProvider>
    </QueryClientProvider>
  )
}

function renderCells(channels: Channel[], layout: 'table' | 'card' = 'table') {
  return render(cells(channels, layout))
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  useAuthStore.setState({
    auth: {
      ...originalAuth,
      user: { id: 1, username: 'root', role: ROLE.SUPER_ADMIN },
    },
  })
  usage = [
    {
      channel_id: 227,
      rpm: { used: 58, limit: 60 },
      daily: { used: 1_000_000, limit: 5_000_000, resets_at: 1_795_000_000 },
    },
  ]
  usageRequests = []
  usageFails = false
  usageFailure = new Error('usage unavailable')
  usageHang = null
  vi.spyOn(api, 'get').mockImplementation(async (url, config) => {
    if (url === '/api/channel/limit_usage') {
      const params = config?.params as { ids?: string } | undefined
      usageRequests.push(String(params?.ids))
      if (usageHang) await new Promise<void>((done) => usageHang?.push(done))
      if (usageFails) throw usageFailure
      return { data: { success: true, message: '', data: usage } }
    }
    return { data: { success: true, data: [] } }
  })
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
  useAuthStore.setState({ auth: originalAuth })
})

test('shows the live use of each configured limit and nothing for channels without limits', async () => {
  renderCells([limited, unlimited])

  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('58/60'))
  expect(cell.querySelector('[data-usage-kind=rpm]')).toHaveTextContent(
    'Requests'
  )
  expect(cell.querySelector('[data-usage-kind=daily]')).toHaveTextContent(
    'Today'
  )
  expect(cell.querySelector('[data-usage-kind=tpm]')).toBeNull()
  expect(cell.querySelector('[data-usage-kind=monthly]')).toBeNull()
  expect(screen.getByTestId('cell-172')).toBeEmptyDOMElement()
  // One request for the page, naming only the channel that has limits.
  expect(usageRequests).toEqual(['227'])
})

test('requests nothing when no listed channel has a limit', async () => {
  renderCells([unlimited, channel(173)])
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 20))
  })
  expect(usageRequests).toEqual([])
})

test('marks a limit that is used up and one that is nearly used up', async () => {
  usage = [
    {
      channel_id: 227,
      rpm: { used: 60, limit: 60 },
      daily: { used: 4_500_000, limit: 5_000_000, resets_at: 1_795_000_000 },
    },
  ]
  renderCells([limited])

  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('60/60'))
  const rpm = cell.querySelector('[data-usage-kind=rpm]')
  expect(rpm).toHaveTextContent('Full')
  expect(rpm).toHaveAttribute('data-usage-level', 'full')
  const daily = cell.querySelector('[data-usage-kind=daily]')
  expect(daily).not.toHaveTextContent('Full')
  expect(daily).toHaveAttribute('data-usage-level', 'near')
})

test('the refresh button beside the usage reads it again at once', async () => {
  const user = userEvent.setup()
  renderCells([limited], 'card')
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('58/60'))
  expect(cell).toHaveTextContent('Limit usage')

  usage = [{ channel_id: 227, rpm: { used: 12, limit: 60 } }]
  await user.click(screen.getByRole('button', { name: 'Refresh limit usage' }))
  await waitFor(() => expect(cell).toHaveTextContent('12/60'))
  expect(usageRequests).toEqual(['227', '227'])
})

test('reads the usage again after each refresh interval', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  renderCells([limited])
  await waitFor(() => expect(usageRequests).toHaveLength(1))

  usage = [{ channel_id: 227, rpm: { used: 59, limit: 60 } }]
  await act(async () => {
    await vi.advanceTimersByTimeAsync(CHANNEL_LIMIT_USAGE_REFRESH_MS + 50)
  })
  await waitFor(() => expect(usageRequests).toHaveLength(2))
  await waitFor(() =>
    expect(screen.getByTestId('cell-227')).toHaveTextContent('59/60')
  )
})

test('opens the details with the reset time of a budget', async () => {
  const user = userEvent.setup()
  renderCells([limited])
  await waitFor(() =>
    expect(screen.getByTestId('cell-227')).toHaveTextContent('58/60')
  )
  await user.click(screen.getByTitle('Limit usage details'))
  expect(
    await screen.findByText(/Sent to the upstream in the last 60 seconds/)
  ).toBeVisible()
  expect(screen.getByText(/Resets 20\d\d-/)).toBeVisible()
})

test('collects limited channels from tag rows too', () => {
  const tagRow = {
    ...channel(-1),
    tag: 'vertex',
    children: [limited, unlimited, channel(300, { tpm_limit: 1000 })],
  } as unknown as Channel
  expect(
    limitedChannelIds([tagRow, channel(5, { monthly_quota_limit: 9 })])
  ).toEqual([5, 227, 300])
})

test('marks the values as not updated when a read fails and keeps the last ones', async () => {
  const user = userEvent.setup()
  const error = vi.spyOn(toast, 'error').mockImplementation(() => '')
  renderCells([limited])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('58/60'))
  expect(cell).not.toHaveTextContent('Not updated')

  usageFails = true
  await user.click(screen.getByRole('button', { name: 'Refresh limit usage' }))
  await waitFor(() => expect(cell).toHaveTextContent('Not updated'))
  expect(cell).toHaveTextContent('58/60')
  // The person who asked for the refresh is told it failed.
  expect(error).toHaveBeenCalled()

  usageFails = false
  usage = [{ channel_id: 227, rpm: { used: 3, limit: 60 } }]
  await user.click(screen.getByRole('button', { name: 'Refresh limit usage' }))
  await waitFor(() => expect(cell).toHaveTextContent('3/60'))
  expect(cell).not.toHaveTextContent('Not updated')
})

test('says so when the first read fails, with no details to open', async () => {
  usageFails = true
  renderCells([limited])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('Not updated'))
  expect(screen.queryByTitle('Limit usage details')).toBeNull()
})

test('hides the numbers while sensitive values are hidden', async () => {
  const user = userEvent.setup()
  renderCells([limited])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('58/60'))

  await user.click(screen.getByRole('button', { name: 'hide' }))
  await waitFor(() => expect(cell).not.toHaveTextContent('58'))
  expect(cell).toHaveTextContent('••••/••••')
})

test('does not round a nearly spent budget up to its limit', async () => {
  // 500,000 quota units are one dollar by default: $4.96 of $5.
  usage = [
    {
      channel_id: 227,
      daily: { used: 2_480_000, limit: 2_500_000, resets_at: 1_795_000_000 },
    },
  ]
  renderCells([limited])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('$4.96/$5'))
  expect(cell).not.toHaveTextContent('Full')
})

test('asks for many channels in batches the server accepts', async () => {
  const many = Array.from({ length: 250 }, (_, index) =>
    channel(1000 + index, { rpm_limit: 10 })
  )
  usage = []
  renderCells(many)
  await waitFor(() => expect(usageRequests).toHaveLength(2))
  expect(usageRequests[0].split(',')).toHaveLength(200)
  expect(usageRequests[1].split(',')).toHaveLength(50)
})

test('keeps saying so while reads keep failing and a retry is under way', async () => {
  usageFails = true
  vi.useFakeTimers({ shouldAdvanceTime: true })
  renderCells([limited])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('Not updated'))

  // The retry starts and does not finish: the marker must not blink away.
  usageHang = []
  await act(async () => {
    await vi.advanceTimersByTimeAsync(CHANNEL_LIMIT_USAGE_RETRY_MS + 50)
  })
  await waitFor(() => expect(usageHang).toHaveLength(1))
  expect(cell).toHaveTextContent('Not updated')
  usageHang.forEach((finish) => finish())
})

test("does not pass off the previous page's numbers as current when the next read fails", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  const view = renderCells([limited])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('58/60'))

  // Another channel joins the list and from now on reads fail.
  usageFails = true
  view.rerender(cells([limited, channel(300, { rpm_limit: 5 })]))
  await waitFor(() => expect(cell).toHaveTextContent('Not updated'))
  usageHang = []
  await act(async () => {
    await vi.advanceTimersByTimeAsync(CHANNEL_LIMIT_USAGE_RETRY_MS + 50)
  })
  await waitFor(() => expect(usageHang).toHaveLength(1))
  expect(cell).toHaveTextContent('58/60')
  expect(cell).toHaveTextContent('Not updated')
  usageHang.forEach((finish) => finish())
})

test('a failing background read never sends the page to the error screen', async () => {
  const toErrorScreen = vi.fn()
  client = createAppQueryClient(toErrorScreen)
  const toastError = vi.spyOn(toast, 'error').mockImplementation(() => '')
  usageFails = true
  usageFailure = Object.assign(new Error('Request failed'), {
    isAxiosError: true,
    response: { status: 500, data: {} },
  })
  renderCells([limited])
  await waitFor(() =>
    expect(screen.getByTestId('cell-227')).toHaveTextContent('Not updated')
  )
  expect(toErrorScreen).not.toHaveBeenCalled()
  expect(toastError).not.toHaveBeenCalled()
})

test('does not shorten a large count into looking used up', async () => {
  usage = [{ channel_id: 227, tpm: { used: 1_960_000, limit: 2_000_000 } }]
  renderCells([channel(227, { tpm_limit: 2_000_000 })])
  const cell = screen.getByTestId('cell-227')
  await waitFor(() => expect(cell).toHaveTextContent('1,960,000/2M'))
  expect(cell).not.toHaveTextContent('Full')
})
