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
import type { Channel } from '../types'
import { isTagAggregateRow } from './channel-utils'

/** How often the usage of the channels on the page is read again. */
export const CHANNEL_LIMIT_USAGE_REFRESH_MS = 3000
/** How often it is tried again while reads fail, to spare the API rate limit. */
export const CHANNEL_LIMIT_USAGE_RETRY_MS = 15_000

/** Whether the channel has any rate or quota limit configured. */
export function channelHasLimits(channel: Channel): boolean {
  return Boolean(
    channel.rpm_limit ||
    channel.tpm_limit ||
    channel.daily_quota_limit ||
    channel.monthly_quota_limit
  )
}

/** Ids of the listed channels (tag rows contribute their channels) that have limits. */
export function limitedChannelIds(channels: readonly Channel[]): number[] {
  const ids = new Set<number>()
  const visit = (channel: Channel) => {
    if (isTagAggregateRow(channel)) {
      channel.children.forEach(visit)
    } else if (channelHasLimits(channel)) {
      ids.add(channel.id)
    }
  }
  channels.forEach(visit)
  return [...ids].sort((a, b) => a - b)
}
