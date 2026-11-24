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
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ThemeCustomizationProvider } from '@/context/theme-customization-provider'
import { ThemeProvider, useTheme } from '@/context/theme-provider'

function Toggle() {
  const { setTheme } = useTheme()
  return (
    <>
      <button type='button' onClick={() => setTheme('dark')}>
        Dark
      </button>
      <button type='button' onClick={() => setTheme('light')}>
        Light
      </button>
    </>
  )
}

function Fixture() {
  return (
    <ThemeProvider>
      <ThemeCustomizationProvider>
        <Toggle />
      </ThemeCustomizationProvider>
    </ThemeProvider>
  )
}

let startViewTransition: ReturnType<typeof vi.fn>

beforeEach(() => {
  localStorage.clear()
  localStorage.setItem('newapi:theme:v1:mode', 'light')
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }))
  )
  startViewTransition = vi.fn((update: () => void) => {
    update()
    return { finished: Promise.resolve() }
  })
  Object.assign(document, { startViewTransition })
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  Reflect.deleteProperty(document, 'startViewTransition')
  localStorage.clear()
  document.documentElement.classList.remove('light', 'dark')
  document.documentElement.removeAttribute('data-ink-reveal')
  document.body.removeAttribute('data-theme-preset')
})

describe('theme mode switch under MX Brutalist', () => {
  it('does not animate the first application, then reveals user switches', async () => {
    const user = userEvent.setup()
    render(<Fixture />)

    expect(document.documentElement).toHaveClass('light')
    expect(startViewTransition).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Dark' }))

    expect(startViewTransition).toHaveBeenCalledOnce()
    expect(document.documentElement).toHaveClass('dark')
    expect(document.documentElement).not.toHaveClass('light')
  })

  it('switches without a transition under another preset', async () => {
    localStorage.setItem('newapi:theme:v1:preset', 'default')
    const user = userEvent.setup()
    render(<Fixture />)

    await user.click(screen.getByRole('button', { name: 'Dark' }))

    expect(startViewTransition).not.toHaveBeenCalled()
    expect(document.documentElement).toHaveClass('dark')
  })

  it('does not start a transition when the mode is already applied', async () => {
    const user = userEvent.setup()
    render(<Fixture />)

    await user.click(screen.getByRole('button', { name: 'Light' }))

    expect(startViewTransition).not.toHaveBeenCalled()
    expect(document.documentElement).toHaveClass('light')
  })
})
