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
import { createContext } from 'react'

import type { ChannelLimitUsage } from '../api'

/** The live limit usage of the channels the list currently shows. */
export type ChannelLimitUsageState = {
  usageById: ReadonlyMap<number, ChannelLimitUsage>
  /** The last read failed, so the values shown are not current. */
  failed: boolean
  /** Reads the usage again at once; rejects when the read fails. */
  refresh: () => Promise<void>
}

export const ChannelLimitUsageContext = createContext<ChannelLimitUsageState>({
  usageById: new Map(),
  failed: false,
  refresh: async () => undefined,
})

/** Tells the usage provider which limited channels the list shows. */
export const ChannelLimitUsageListedContext = createContext<
  (ids: number[]) => void
>(() => undefined)
