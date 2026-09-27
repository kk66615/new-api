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
import { describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import type { UpstreamResponseAudit } from '../../types'
import { formatModelName, isUpstreamAuditAnomaly } from '../format'

describe('isUpstreamAuditAnomaly', () => {
  test.each<[string, UpstreamResponseAudit]>([
    ['model mismatch', { mismatch: true }],
    ['suspected replay', { replay: true }],
    ['model change inside one stream', { mid_stream_change: true }],
    ['source flip', { src_flip: 'azure->gpt_relay' }],
    ['backend flip', { backend_flip: 'aws->claude_relay' }],
  ])('highlights a row with %s', (_name, ursp) => {
    expect(isUpstreamAuditAnomaly(ursp)).toBe(true)
  })

  test('does not highlight a pinned build answered by the bare alias', () => {
    expect(
      isUpstreamAuditAnomaly({ mismatch: false, kind: 'pinned_to_alias' })
    ).toBe(false)
  })

  test('leaves a clean row with only weak signals unhighlighted', () => {
    expect(
      isUpstreamAuditAnomaly({
        mismatch: false,
        kind: 'snapshot_date',
        stale_created: true,
        dup_output: true,
        extra_keys: ['provider'],
      })
    ).toBe(false)
    expect(isUpstreamAuditAnomaly(undefined)).toBe(false)
  })
})

function logWithOther(other: object): UsageLog {
  return { model_name: 'gpt-5', other: JSON.stringify(other) } as UsageLog
}

describe('formatModelName mapped model source', () => {
  test('reads the mapped model from admin_info', () => {
    const result = formatModelName(
      logWithOther({
        is_model_mapped: true,
        admin_info: { upstream_model_name: 'gpt-5-mapped' },
      })
    )
    expect(result.isMapped).toBe(true)
    expect(result.actualModel).toBe('gpt-5-mapped')
  })

  test('falls back to the legacy top-level field of older rows', () => {
    const result = formatModelName(
      logWithOther({ is_model_mapped: true, upstream_model_name: 'legacy' })
    )
    expect(result.actualModel).toBe('legacy')
  })

  test('is not mapped when the user projection carries no model name', () => {
    const result = formatModelName(logWithOther({ is_model_mapped: true }))
    expect(result.isMapped).toBe(false)
    expect(result.actualModel).toBeUndefined()
  })
})
