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
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  attachDialogOriginMotion,
  computeOriginTransform,
  countUpProgress,
  formatCountUp,
  parseCountUpTarget,
  runInkThemeTransition,
} from '@/lib/brutalist-motion'

function stubMotionAllowed(reduce = false) {
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

function stubAnimations(el: HTMLElement) {
  const calls: Array<{
    keyframes: Keyframe[]
    options: KeyframeAnimationOptions
  }> = []
  const running: Array<{ cancel: () => void }> = []
  el.animate = ((keyframes: Keyframe[], options: KeyframeAnimationOptions) => {
    calls.push({ keyframes, options })
    const anim = { cancel: vi.fn() }
    running.push(anim)
    return anim as unknown as Animation
  }) as HTMLElement['animate']
  el.getAnimations = () => running.splice(0) as unknown as Animation[]
  el.getBoundingClientRect = () =>
    ({ left: 400, top: 200, width: 400, height: 300 }) as DOMRect
  return calls
}

async function flush() {
  await Promise.resolve()
}

describe('count-up helpers', () => {
  it('splits currency, grouping, decimals and suffixes', () => {
    expect(parseCountUpTarget('$2,340.53')).toEqual({
      prefix: '$',
      value: 2340.53,
      decimals: 2,
      grouped: true,
      suffix: '',
    })
    expect(parseCountUpTarget('61 次')).toEqual({
      prefix: '',
      value: 61,
      decimals: 0,
      grouped: false,
      suffix: ' 次',
    })
    expect(parseCountUpTarget('--')).toBeNull()
    expect(parseCountUpTarget('1.2K / 3.4K')).toBeNull()
  })

  it('formats intermediate values in the same shape as the final text', () => {
    const target = parseCountUpTarget('$2,340.53')
    if (!target) throw new Error('expected a parsable figure')
    expect(formatCountUp(target, 0)).toBe('$0.00')
    expect(formatCountUp(target, 1234.5)).toBe('$1,234.50')
    expect(formatCountUp(target, target.value)).toBe('$2,340.53')
  })

  it('moves fast at first and lands exactly on the end value', () => {
    expect(countUpProgress(0, 1000)).toBe(0)
    expect(countUpProgress(250, 1000)).toBeGreaterThan(0.8)
    expect(countUpProgress(999, 1000)).toBeLessThan(1)
    expect(countUpProgress(1000, 1000)).toBe(1)
    expect(countUpProgress(5000, 1000)).toBe(1)
  })
})

describe('computeOriginTransform', () => {
  it('maps the dialog box onto the trigger box', () => {
    const to = { left: 400, top: 200, width: 400, height: 300 }
    const from = { left: 100, top: 50, width: 40, height: 30 }
    expect(computeOriginTransform(from, to)).toBe(
      'translate(-480px, -285px) scale(0.1, 0.1)'
    )
  })

  it('never enlarges past the resting size and skips empty boxes', () => {
    const to = { left: 0, top: 0, width: 100, height: 100 }
    expect(
      computeOriginTransform({ left: 0, top: 0, width: 500, height: 500 }, to)
    ).toBe('translate(200px, 200px) scale(1, 1)')
    expect(
      computeOriginTransform(to, { left: 0, top: 0, width: 0, height: 0 })
    ).toBeNull()
  })
})

describe('attachDialogOriginMotion', () => {
  let el: HTMLDivElement
  let detach: () => void

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0)
      return 1
    })
    el = document.createElement('div')
    document.body.appendChild(el)
  })

  afterEach(() => {
    detach?.()
    el.remove()
    document.body.removeAttribute('data-theme-preset')
    vi.unstubAllGlobals()
  })

  function clickAt(x: number, y: number) {
    const trigger = document.createElement('button')
    trigger.getBoundingClientRect = () =>
      ({ left: x - 10, top: y - 5, width: 20, height: 10 }) as DOMRect
    document.body.appendChild(trigger)
    trigger.dispatchEvent(
      new MouseEvent('pointerdown', { bubbles: true, clientX: x, clientY: y })
    )
    trigger.remove()
  }

  it('grows from the trigger on open and folds back on close, including reopen of a kept-mounted popup', async () => {
    document.body.setAttribute('data-theme-preset', 'mx-brutalist')
    stubMotionAllowed()
    const calls = stubAnimations(el)
    detach = attachDialogOriginMotion(el)
    expect(el).toHaveAttribute('data-origin-motion')

    clickAt(100, 50)
    el.setAttribute('data-open', '')
    await flush()
    expect(calls).toHaveLength(1)
    expect(calls[0].keyframes[0].transform).toMatch(/^translate\(/)
    expect(calls[0].keyframes[1].transform).toBe('none')

    el.removeAttribute('data-open')
    el.setAttribute('data-closed', '')
    await flush()
    expect(calls).toHaveLength(2)
    expect(calls[1].keyframes[1].transform).toMatch(/^translate\(/)
    expect(calls[1].options.fill).toBe('forwards')

    // Same element reopened in place (Base UI keeps some popups mounted).
    clickAt(300, 400)
    el.removeAttribute('data-closed')
    el.setAttribute('data-open', '')
    await flush()
    expect(calls).toHaveLength(3)
    expect(calls[2].keyframes[1].transform).toBe('none')
  })

  it('does nothing for other presets or reduced motion', async () => {
    document.body.setAttribute('data-theme-preset', 'default')
    stubMotionAllowed()
    const calls = stubAnimations(el)
    detach = attachDialogOriginMotion(el)
    clickAt(100, 50)
    el.setAttribute('data-open', '')
    await flush()
    expect(calls).toHaveLength(0)
    expect(el).not.toHaveAttribute('data-origin-motion')

    detach()
    document.body.setAttribute('data-theme-preset', 'mx-brutalist')
    stubMotionAllowed(true)
    const other = document.createElement('div')
    const otherCalls = stubAnimations(other)
    detach = attachDialogOriginMotion(other)
    clickAt(100, 50)
    other.setAttribute('data-open', '')
    await flush()
    expect(otherCalls).toHaveLength(0)
  })
})

describe('attachDialogOriginMotion edge cases', () => {
  let queue: Array<{ id: number; cb: FrameRequestCallback }>
  let el: HTMLDivElement
  let detach: () => void

  beforeEach(() => {
    queue = []
    let id = 1
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      queue.push({ id, cb })
      return id++
    })
    vi.stubGlobal('cancelAnimationFrame', (handle: number) => {
      queue = queue.filter((frame) => frame.id !== handle)
    })
    document.body.setAttribute('data-theme-preset', 'mx-brutalist')
    stubMotionAllowed()
    el = document.createElement('div')
    document.body.appendChild(el)
  })

  afterEach(() => {
    detach?.()
    el.remove()
    document.body.removeAttribute('data-theme-preset')
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('opens in place when there is no recent click (keyboard open)', async () => {
    const calls = stubAnimations(el)
    // Any earlier click is now far in the past.
    vi.spyOn(performance, 'now').mockReturnValue(1e9)
    detach = attachDialogOriginMotion(el)
    el.setAttribute('data-open', '')
    await flush()
    for (const frame of queue.splice(0)) frame.cb(0)
    expect(calls).toHaveLength(0)
  })

  it('drops the queued grow-in when the dialog closes in the same frame', async () => {
    const calls = stubAnimations(el)
    detach = attachDialogOriginMotion(el)
    const trigger = document.createElement('button')
    trigger.getBoundingClientRect = () =>
      ({ left: 0, top: 0, width: 20, height: 10 }) as DOMRect
    document.body.appendChild(trigger)
    trigger.dispatchEvent(
      new MouseEvent('pointerdown', { bubbles: true, clientX: 5, clientY: 5 })
    )
    trigger.remove()

    el.setAttribute('data-open', '')
    await flush()
    expect(queue).toHaveLength(1)
    el.removeAttribute('data-open')
    el.setAttribute('data-closed', '')
    await flush()
    for (const frame of queue.splice(0)) frame.cb(0)

    expect(calls).toHaveLength(1)
    expect(calls[0].options.fill).toBe('forwards')
  })
})

describe('runInkThemeTransition', () => {
  afterEach(() => {
    document.body.removeAttribute('data-theme-preset')
    document.documentElement.removeAttribute('data-ink-reveal')
    Reflect.deleteProperty(document, 'startViewTransition')
    vi.unstubAllGlobals()
  })

  it('applies directly when the preset is not active', () => {
    stubMotionAllowed()
    const apply = vi.fn()
    const start = vi.fn()
    Object.assign(document, { startViewTransition: start })
    runInkThemeTransition(apply)
    expect(apply).toHaveBeenCalledOnce()
    expect(start).not.toHaveBeenCalled()
  })

  it('wraps the update in a view transition and clears the marker afterwards', async () => {
    document.body.setAttribute('data-theme-preset', 'mx-brutalist')
    stubMotionAllowed()
    const apply = vi.fn()
    let finish!: () => void
    const finished = new Promise<void>((resolve) => (finish = resolve))
    Object.assign(document, {
      startViewTransition: vi.fn((update: () => void) => {
        update()
        return { finished }
      }),
    })
    runInkThemeTransition(apply)
    expect(apply).toHaveBeenCalledOnce()
    expect(document.documentElement).toHaveAttribute('data-ink-reveal')
    finish()
    await finished
    await flush()
    expect(document.documentElement).not.toHaveAttribute('data-ink-reveal')
  })
})
