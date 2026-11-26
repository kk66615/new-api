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
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { RefreshCw } from 'lucide-react'
import {
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'
import { toIntlLocale } from '@/i18n/languages'
import { formatQuotaWithCurrency } from '@/lib/currency'
import {
  formatCompactNumber,
  formatNumber,
  formatTimestampToDate,
} from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'
import { getServerErrorMessage } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import { getChannelLimitUsage, type ChannelLimitUsageItem } from '../api'
import { channelsQueryKeys, isTagAggregateRow } from '../lib'
import {
  CHANNEL_LIMIT_USAGE_REFRESH_MS,
  CHANNEL_LIMIT_USAGE_RETRY_MS,
  channelHasLimits,
  limitedChannelIds,
} from '../lib/channel-limit-usage'
import type { Channel } from '../types'
import {
  ChannelLimitUsageContext,
  ChannelLimitUsageListedContext,
  type ChannelLimitUsageState,
} from './channel-limit-usage-context'
import { ChannelRowActionsLayoutContext } from './channel-row-actions-context'
import { useChannels } from './channels-provider'

const SENSITIVE_MASK = '••••'
/** Budget amounts longer than this are shortened in the cell. */
const MAX_INLINE_AMOUNT_CHARS = 8
/** From this share of a limit on, the value is shown as nearly used up. */
const NEARLY_FULL_RATIO = 0.8

/**
 * Reads the live limit usage of the listed channels once for the whole page
 * and again every few seconds while the page is in the foreground. Nothing is
 * requested while no listed channel has a limit. The list names its channels
 * through ChannelLimitUsageSource.
 */
export function ChannelLimitUsageProvider(props: { children: ReactNode }) {
  const [ids, setIds] = useState<number[]>([])
  const setListed = useCallback((next: number[]) => {
    setIds((current) =>
      current.length === next.length &&
      current.every((id, index) => id === next[index])
        ? current
        : next
    )
  }, [])
  const { data, refetch, isPaused, dataUpdatedAt, errorUpdatedAt } = useQuery({
    queryKey: channelsQueryKeys.limitUsage(ids),
    queryFn: async ({ signal }) => {
      try {
        return await getChannelLimitUsage(ids, signal)
      } catch (error) {
        // A background read must not send the page to the error screen the
        // way a failed page load does (the status is found through `cause`
        // too, so the original error is not attached); the cells show that
        // the read failed.
        throw new Error(getServerErrorMessage(error))
      }
    },
    enabled: ids.length > 0,
    refetchInterval: (query) =>
      query.state.errorUpdatedAt > query.state.dataUpdatedAt
        ? CHANNEL_LIMIT_USAGE_RETRY_MS
        : CHANNEL_LIMIT_USAGE_REFRESH_MS,
    refetchIntervalInBackground: false,
    // Coming back to the page reads at once instead of showing old numbers.
    refetchOnWindowFocus: 'always',
    placeholderData: keepPreviousData,
    retry: false,
    gcTime: 0,
    meta: { errorToast: false },
  })
  // The outcome of the last read that finished. `isError` would not do: it
  // is cleared while a retry is under way whenever this set of channels has
  // no data of its own yet, and the marker would blink away on every retry.
  const failed = isPaused || errorUpdatedAt > dataUpdatedAt
  const value = useMemo<ChannelLimitUsageState>(
    () => ({
      usageById: new Map(
        (data ?? []).map((usage) => [usage.channel_id, usage])
      ),
      failed,
      refresh: async () => {
        const result = await refetch()
        if (result.isError) throw result.error
      },
    }),
    [data, failed, refetch]
  )
  return (
    <ChannelLimitUsageListedContext.Provider value={setListed}>
      <ChannelLimitUsageContext.Provider value={value}>
        {props.children}
      </ChannelLimitUsageContext.Provider>
    </ChannelLimitUsageListedContext.Provider>
  )
}

/** Renders nothing: names the channels the list shows to the usage provider. */
export function ChannelLimitUsageSource(props: {
  channels: readonly Channel[]
}) {
  const setListed = useContext(ChannelLimitUsageListedContext)
  const ids = useMemo(() => limitedChannelIds(props.channels), [props.channels])
  useEffect(() => {
    setListed(ids)
  }, [ids, setListed])
  useEffect(() => () => setListed([]), [setListed])
  return null
}

type UsageKind = 'rpm' | 'tpm' | 'daily' | 'monthly'

const USAGE_KINDS: UsageKind[] = ['rpm', 'tpm', 'daily', 'monthly']

type UsageLevel = 'ok' | 'near' | 'full'

function usageLevel(item: ChannelLimitUsageItem): UsageLevel {
  if (item.used >= item.limit) return 'full'
  if (item.used >= item.limit * NEARLY_FULL_RATIO) return 'near'
  return 'ok'
}

const USAGE_LEVEL_CLASS: Record<UsageLevel, string> = {
  ok: '',
  near: 'text-warning',
  full: 'text-destructive',
}

/**
 * The live use of a channel's limits with a refresh button, shown in the
 * limit usage column of the table and in the footer of the channel card.
 * Renders nothing for tag rows and for channels without limits.
 */
export function ChannelLimitUsageCell({ channel }: { channel: Channel }) {
  const { t, i18n } = useTranslation()
  const layout = useContext(ChannelRowActionsLayoutContext)
  const { sensitiveVisible } = useChannels()
  const { usageById, failed, refresh } = useContext(ChannelLimitUsageContext)
  const [refreshing, setRefreshing] = useState(false)
  const handleRefresh = useCallback(async () => {
    setRefreshing(true)
    try {
      await refresh()
    } catch (error) {
      handleServerError(error, undefined, {
        title: t('Could not read the limit usage'),
        description: getServerErrorMessage(error),
      })
    } finally {
      setRefreshing(false)
    }
  }, [refresh, t])

  if (isTagAggregateRow(channel) || !channelHasLimits(channel)) {
    return null
  }

  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const usage = usageById.get(channel.id)
  const labels: Record<UsageKind, string> = {
    rpm: t('Requests'),
    tpm: t('Tokens'),
    daily: t('Today'),
    monthly: t('This month (limit usage)'),
  }
  const notes: Record<UsageKind, string> = {
    rpm: t('Sent to the upstream in the last 60 seconds, failed ones included'),
    tpm: t('Settled in the last 60 seconds'),
    daily: t('At list price, before group ratios'),
    monthly: t('At list price, before group ratios'),
  }
  const format = (kind: UsageKind, value: number, compact: boolean) => {
    if (!sensitiveVisible) return SENSITIVE_MASK
    if (kind === 'daily' || kind === 'monthly') {
      const exact = formatQuotaWithCurrency(value, {
        digitsLarge: 2,
        digitsSmall: 2,
        abbreviate: false,
        locale,
      })
      // Rounding $4.96 of $5 to "$5" would read as used up; shorten only
      // amounts too long for the cell.
      return compact && exact.length > MAX_INLINE_AMOUNT_CHARS
        ? formatQuotaWithCurrency(value, { compact: true, locale })
        : exact
    }
    return compact && value >= 10000
      ? formatCompactNumber(value, locale)
      : formatNumber(value, locale)
  }
  const items = USAGE_KINDS.flatMap((kind) => {
    const item = usage?.[kind]
    if (!item) return []
    const limit = format(kind, item.limit, true)
    let used = format(kind, item.used, true)
    // Shortening must not make a limit look used up: 1,960,000 of 2,000,000
    // would read "2M/2M".
    if (used === limit && item.used < item.limit && sensitiveVisible) {
      used = format(kind, item.used, false)
    }
    return [{ kind, item, used, limit }]
  })

  const refreshButton = (
    <Button
      type='button'
      variant='ghost'
      size='icon-xs'
      className='text-muted-foreground shrink-0'
      aria-label={t('Refresh limit usage')}
      title={t('Refresh limit usage')}
      disabled={refreshing}
      onClick={handleRefresh}
    >
      <RefreshCw className={cn('size-3', refreshing && 'animate-spin')} />
    </Button>
  )

  const staleNote = failed && (
    <span
      data-usage-stale
      className='text-destructive text-[11px] font-medium'
      title={t('Could not read the limit usage')}
    >
      {t('Not updated (limit usage)')}
    </span>
  )
  const summary =
    items.length === 0 ? (
      // Nothing read yet (or the first read failed): no details to open.
      <span className='flex items-baseline gap-2 text-sm'>
        <span className='text-muted-foreground'>-</span>
        {staleNote}
      </span>
    ) : (
      <Popover>
        <PopoverTrigger
          render={
            <button
              type='button'
              title={t('Limit usage details')}
              className='flex min-w-0 cursor-pointer flex-wrap items-baseline gap-x-3 gap-y-0.5 text-left text-sm tabular-nums'
            />
          }
        >
          {items.map(({ kind, item, used, limit }) => {
            const level = usageLevel(item)
            return (
              <span
                key={kind}
                data-usage-kind={kind}
                data-usage-level={level}
                className={cn(
                  'inline-flex max-w-full flex-wrap items-baseline gap-x-1',
                  failed && 'opacity-50'
                )}
              >
                <span className='text-muted-foreground text-[11px] font-medium'>
                  {labels[kind]}
                </span>
                <span className={USAGE_LEVEL_CLASS[level]}>
                  {used}/{limit}
                </span>
                {level === 'full' && (
                  <span className='text-destructive text-[11px] font-medium'>
                    {t('Full (limit usage)')}
                  </span>
                )}
              </span>
            )
          })}
          {staleNote}
        </PopoverTrigger>
        <PopoverContent
          className='w-auto max-w-[min(90vw,24rem)]'
          align='start'
        >
          {failed && (
            <p className='text-destructive text-xs'>
              {t('Could not read the limit usage')}
            </p>
          )}
          <dl className='grid grid-cols-[auto_1fr] gap-x-3 gap-y-2 text-sm'>
            {items.map(({ kind, item }) => {
              const level = usageLevel(item)
              return (
                <div key={kind} className='col-span-2 grid grid-cols-subgrid'>
                  <dt className='text-muted-foreground'>{labels[kind]}</dt>
                  <dd className='min-w-0'>
                    <div
                      className={cn('tabular-nums', USAGE_LEVEL_CLASS[level])}
                    >
                      {format(kind, item.used, false)} /{' '}
                      {format(kind, item.limit, false)}
                    </div>
                    <div className='text-muted-foreground text-xs'>
                      {notes[kind]}
                      {item.resets_at
                        ? ` · ${t('Resets {{time}}', {
                            time: formatTimestampToDate(item.resets_at),
                          })}`
                        : ''}
                    </div>
                    {level === 'full' && (
                      <div className='text-destructive text-xs'>
                        {t('At its limit: new requests go to other channels')}
                      </div>
                    )}
                  </dd>
                </div>
              )
            })}
          </dl>
        </PopoverContent>
      </Popover>
    )

  if (layout === 'card') {
    return (
      <div
        data-slot='channel-limit-usage'
        className='flex min-w-0 flex-col gap-1'
      >
        <div className='flex items-center gap-1'>
          <span className='text-muted-foreground text-[11px] font-medium select-none'>
            {t('Limit usage')}
          </span>
          {refreshButton}
        </div>
        {summary}
      </div>
    )
  }
  return (
    <div
      data-slot='channel-limit-usage'
      className='flex min-w-0 items-center gap-1'
    >
      {summary}
      {refreshButton}
    </div>
  )
}
