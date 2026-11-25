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
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { initializeFrontendCache } from '@/lib/frontend-cache'
import { THEME_STORAGE_KEYS } from '@/lib/theme-storage'

// The inline script in index.html that picks the colour preset before React
// mounts. Run it as the browser would.
function runFirstPaintScript() {
  const here = path.dirname(fileURLToPath(import.meta.url))
  const html = readFileSync(path.resolve(here, '../../../index.html'), 'utf8')
  const doc = new DOMParser().parseFromString(html, 'text/html')
  const script = [...doc.body.querySelectorAll('script')].find((el) =>
    el.textContent?.includes('data-theme-preset')
  )
  if (!script?.textContent) throw new Error('first-paint preset script missing')
  new Function(script.textContent)()
}

const PRESET_KEY = THEME_STORAGE_KEYS.preset
const ADOPTED_KEY = THEME_STORAGE_KEYS.presetAdopted
const preset = () => document.body.getAttribute('data-theme-preset')

beforeEach(() => localStorage.clear())
afterEach(() => {
  vi.restoreAllMocks()
  localStorage.clear()
  document.body.removeAttribute('data-theme-preset')
})

describe('first-paint preset', () => {
  it('starts a first-time visitor on MX Brutalist', () => {
    runFirstPaintScript()
    expect(preset()).toBe('mx-brutalist')
    expect(localStorage.getItem(ADOPTED_KEY)).toBe('mx-brutalist')
  })

  it('moves a returning visitor off a preset saved before the switch', () => {
    localStorage.setItem(PRESET_KEY, 'anthropic')
    runFirstPaintScript()
    expect(preset()).toBe('mx-brutalist')
    expect(localStorage.getItem(PRESET_KEY)).toBeNull()
  })

  it('keeps a preset chosen after the switch', () => {
    runFirstPaintScript()
    localStorage.setItem(PRESET_KEY, 'default')
    runFirstPaintScript()
    expect(preset()).toBe('default')
    expect(localStorage.getItem(PRESET_KEY)).toBe('default')
  })

  it('keeps a choice made on the first visit across the cache reset', () => {
    // main.tsx resets the frontend cache after this script on a first visit.
    runFirstPaintScript()
    initializeFrontendCache()
    localStorage.setItem(PRESET_KEY, 'default')

    runFirstPaintScript()
    initializeFrontendCache()

    expect(preset()).toBe('default')
    expect(localStorage.getItem(PRESET_KEY)).toBe('default')
  })

  it('falls back to MX Brutalist when storage is unavailable', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked')
    })
    runFirstPaintScript()
    expect(preset()).toBe('mx-brutalist')
  })
})
