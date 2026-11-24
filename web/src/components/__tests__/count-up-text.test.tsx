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
import { act, cleanup, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { CountUpText } from '@/components/count-up-text'
import { ThemeCustomizationProvider } from '@/context/theme-customization-provider'

// Manual animation clock: frames only run when the test advances time.
let now = 0
let queue: Array<{ id: number; cb: FrameRequestCallback }> = []
let nextId = 1

function advance(ms: number) {
  now += ms
  const due = queue
  queue = []
  act(() => {
    for (const frame of due) frame.cb(now)
  })
}

function stubMotion(reduce: boolean) {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: query.includes('reduce') ? reduce : false,
      media: query,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    }))
  )
}

function Figure(props: { text: string }) {
  return (
    <ThemeCustomizationProvider>
      <output aria-label='figure'>
        <CountUpText text={props.text} />
      </output>
    </ThemeCustomizationProvider>
  )
}

const figure = () => screen.getByLabelText('figure')

beforeEach(() => {
  now = 0
  queue = []
  vi.spyOn(performance, 'now').mockImplementation(() => now)
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    const id = nextId++
    queue.push({ id, cb })
    return id
  })
  vi.stubGlobal(
    'cancelAnimationFrame',
    vi.fn((id: number) => {
      queue = queue.filter((frame) => frame.id !== id)
    })
  )
  localStorage.clear()
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  localStorage.clear()
  document.body.removeAttribute('data-theme-preset')
})

describe('CountUpText', () => {
  it('rolls up from zero and lands on the exact text under the default preset', () => {
    stubMotion(false)
    render(<Figure text='$2,340.53' />)

    expect(figure()).toHaveTextContent('$0.00')
    advance(200)
    const midway = Number(figure().textContent?.replace(/[$,]/g, ''))
    expect(midway).toBeGreaterThan(0)
    expect(midway).toBeLessThan(2340.53)
    advance(2000)
    expect(figure()).toHaveTextContent('$2,340.53')
    expect(queue).toHaveLength(0)
  })

  it('restarts from zero when the value changes', () => {
    stubMotion(false)
    const view = render(<Figure text='61' />)
    advance(2000)
    expect(figure()).toHaveTextContent('61')

    view.rerender(<Figure text='75' />)
    expect(figure()).toHaveTextContent('0')
    advance(2000)
    expect(figure()).toHaveTextContent('75')
  })

  it('shows non-numeric text, zero, reduced motion and other presets as-is', () => {
    stubMotion(false)
    const view = render(<Figure text='--' />)
    expect(figure()).toHaveTextContent('--')
    view.unmount()

    render(<Figure text='$0' />)
    expect(figure()).toHaveTextContent('$0')
    expect(queue).toHaveLength(0)
    cleanup()

    stubMotion(true)
    render(<Figure text='$9.99' />)
    expect(figure()).toHaveTextContent('$9.99')
    cleanup()

    stubMotion(false)
    localStorage.setItem('newapi:theme:v1:preset', 'default')
    render(<Figure text='$9.99' />)
    expect(figure()).toHaveTextContent('$9.99')
    expect(queue).toHaveLength(0)
  })

  it('stops its frame loop on unmount', () => {
    stubMotion(false)
    const view = render(<Figure text='1,000' />)
    expect(queue).toHaveLength(1)
    view.unmount()
    expect(queue).toHaveLength(0)
  })
})
