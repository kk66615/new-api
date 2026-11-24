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
/**
 * Script-driven motion for the MX Brutalist preset:
 *  - dialogs grow out of the element that opened them and fold back on close
 *  - the dark/light switch spreads the new palette from the click point
 *  - helpers for numbers that roll up from zero (see components/count-up-text)
 *
 * Everything is a no-op unless the preset is active and the user has not asked
 * the OS for reduced motion. CSS for the same preset lives in
 * styles/mx-brutalist-motion.css.
 */
import { useCallback } from 'react'

export const BRUTALIST_PRESET = 'mx-brutalist'

export function prefersReducedMotion(): boolean {
  if (typeof window === 'undefined' || !window.matchMedia) return true
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches
}

export function isBrutalistMotionActive(): boolean {
  if (typeof document === 'undefined' || !document.body) return false
  return (
    document.body.getAttribute('data-theme-preset') === BRUTALIST_PRESET &&
    !prefersReducedMotion()
  )
}

/* ── Last pointer position ────────────────────────────────────────────── */

type Box = { left: number; top: number; width: number; height: number }

let lastPointer: { x: number; y: number; box: Box | null; at: number } | null =
  null

// A dialog opened by keyboard, or a theme flip from the OS, should not start
// from an unrelated old click.
const ORIGIN_MAX_AGE_MS = 1500

function recentPointer() {
  return lastPointer && performance.now() - lastPointer.at < ORIGIN_MAX_AGE_MS
    ? lastPointer
    : null
}
let pointerTrackingInstalled = false

const ORIGIN_SELECTOR = 'button, a, [role="button"], [role="menuitem"], tr'

function installPointerTracking() {
  if (pointerTrackingInstalled || typeof document === 'undefined') return
  pointerTrackingInstalled = true
  document.addEventListener(
    'pointerdown',
    (event) => {
      const target =
        event.target instanceof Element
          ? event.target.closest(ORIGIN_SELECTOR)
          : null
      const rect = target?.getBoundingClientRect()
      lastPointer = {
        x: event.clientX,
        y: event.clientY,
        at: performance.now(),
        box: rect
          ? {
              left: rect.left,
              top: rect.top,
              width: rect.width,
              height: rect.height,
            }
          : null,
      }
    },
    true
  )
}

// Track from module load so the very first click (e.g. the theme toggle) is
// already known when its handler runs.
installPointerTracking()

/* ── Dialog grows from its origin ─────────────────────────────────────── */

/**
 * Transform that maps a dialog's resting box onto the origin box: centre to
 * centre, scaled to the origin's size. Scale is clamped so a tiny trigger
 * still produces a visible starting frame.
 */
export function computeOriginTransform(from: Box, to: Box): string | null {
  if (to.width <= 0 || to.height <= 0) return null
  const dx = from.left + from.width / 2 - (to.left + to.width / 2)
  const dy = from.top + from.height / 2 - (to.top + to.height / 2)
  const sx = Math.min(Math.max(from.width / to.width, 0.05), 1)
  const sy = Math.min(Math.max(from.height / to.height, 0.05), 1)
  const round = (n: number) => Math.round(n * 1000) / 1000
  return `translate(${round(dx)}px, ${round(dy)}px) scale(${round(sx)}, ${round(sy)})`
}

const OPEN_DURATION = 560
const CLOSE_DURATION = 360

function playOrigin(el: HTMLElement, origin: Box | null, closing: boolean) {
  // Measure the resting layout box, not a half-finished animation frame.
  el.getAnimations().forEach((a) => a.cancel())
  if (!origin) return
  const transform = computeOriginTransform(origin, el.getBoundingClientRect())
  if (!transform) return
  const collapsed = { transform, opacity: 0.4 }
  const open = { transform: 'none', opacity: 1 }
  if (closing) {
    // Base UI waits for running animations before unmounting the popup, so
    // the fold-back completes before the dialog disappears.
    el.animate([open, collapsed], {
      duration: CLOSE_DURATION,
      easing: 'cubic-bezier(0.5, 0, 0.75, 0)',
      fill: 'forwards',
    })
    return
  }
  el.animate([collapsed, open], {
    duration: OPEN_DURATION,
    easing: 'cubic-bezier(0.2, 1.15, 0.32, 1)',
  })
  for (const child of el.children) {
    child.animate(
      [{ opacity: 0 }, { opacity: 0, offset: 0.3 }, { opacity: 1 }],
      {
        duration: 480,
        easing: 'ease-out',
      }
    )
  }
}

/**
 * Wire one dialog popup element. Some dialogs stay mounted while closed and
 * are reopened in place, so open/close is derived from the element's current
 * Base UI state attributes on every change rather than from mount/unmount.
 */
export function attachDialogOriginMotion(el: HTMLElement): () => void {
  installPointerTracking()
  let state: 'idle' | 'open' | 'closing' = 'idle'
  let origin: Box | null = null
  let pendingOpen = 0
  const cancelPendingOpen = () => {
    if (pendingOpen && typeof cancelAnimationFrame === 'function') {
      cancelAnimationFrame(pendingOpen)
    }
    pendingOpen = 0
  }

  const sync = () => {
    const active = isBrutalistMotionActive()
    if (active) el.setAttribute('data-origin-motion', '')
    else el.removeAttribute('data-origin-motion')

    const closing =
      el.hasAttribute('data-closed') || el.hasAttribute('data-ending-style')
    if (closing) {
      if (state !== 'open') return
      state = 'closing'
      // An open animation still waiting for its frame must not restart after
      // the close (it would grow the dialog back before it disappears).
      cancelPendingOpen()
      if (active) playOrigin(el, origin, true)
      return
    }
    if (!el.hasAttribute('data-open') || state === 'open') return
    state = 'open'
    // A kept-mounted popup may still hold the filled fold-back frame from its
    // last close; clear it even when motion is now off.
    el.getAnimations?.().forEach((a) => a.cancel())
    const pointer = recentPointer()
    origin = pointer
      ? (pointer.box ?? {
          left: pointer.x - 8,
          top: pointer.y - 8,
          width: 16,
          height: 16,
        })
      : null
    if (active) {
      pendingOpen = requestAnimationFrame(() => playOrigin(el, origin, false))
    }
  }

  const observer = new MutationObserver(sync)
  observer.observe(el, {
    attributes: true,
    attributeFilter: ['data-open', 'data-closed', 'data-ending-style'],
  })
  sync()
  return () => {
    observer.disconnect()
    cancelPendingOpen()
  }
}

/**
 * Callback ref for dialog popups (`Dialog` and `AlertDialog`). React 19 runs
 * the returned cleanup when the popup unmounts.
 */
export function useDialogOriginMotionRef() {
  return useCallback((node: HTMLElement | null) => {
    if (!node) return
    return attachDialogOriginMotion(node)
  }, [])
}

/* ── Dark/light ink reveal ────────────────────────────────────────────── */

type ViewTransitionDocument = Document & {
  startViewTransition?: (update: () => void) => { finished: Promise<void> }
}

/**
 * Run a theme-mode DOM update. With the preset active, the new palette spreads
 * as a circle from the last click; otherwise the update is applied directly.
 */
export function runInkThemeTransition(apply: () => void) {
  installPointerTracking()
  const doc = document as ViewTransitionDocument
  if (
    !isBrutalistMotionActive() ||
    typeof doc.startViewTransition !== 'function'
  ) {
    apply()
    return
  }
  const root = document.documentElement
  // A recent click spreads from the click; a keyboard or OS-driven switch
  // spreads from the top centre.
  const pointer = recentPointer()
  const x = pointer?.x ?? window.innerWidth / 2
  const y = pointer?.y ?? 0
  root.style.setProperty('--ink-x', `${Math.round(x)}px`)
  root.style.setProperty('--ink-y', `${Math.round(y)}px`)
  root.setAttribute('data-ink-reveal', '')
  const clearMarker = () => root.removeAttribute('data-ink-reveal')
  try {
    doc.startViewTransition(apply).finished.then(clearMarker).catch(clearMarker)
  } catch {
    clearMarker()
    apply()
  }
}

/* ── Count-up helpers ─────────────────────────────────────────────────── */

export type CountUpTarget = {
  prefix: string
  value: number
  decimals: number
  grouped: boolean
  suffix: string
}

const COUNT_UP_PATTERN = /^(\D*?)(-?\d[\d,]*(?:\.\d+)?)(\D*)$/

/** Split a formatted figure such as "$2,340.53" or "61 次" into parts. */
export function parseCountUpTarget(text: string): CountUpTarget | null {
  const match = COUNT_UP_PATTERN.exec(text.trim())
  if (!match) return null
  const [, prefix, numeric, suffix] = match
  const value = Number(numeric.replaceAll(',', ''))
  if (!Number.isFinite(value)) return null
  const decimals = numeric.includes('.') ? numeric.split('.')[1].length : 0
  return { prefix, value, decimals, grouped: numeric.includes(','), suffix }
}

export function formatCountUp(target: CountUpTarget, current: number): string {
  const body = current.toLocaleString('en-US', {
    minimumFractionDigits: target.decimals,
    maximumFractionDigits: target.decimals,
    useGrouping: target.grouped,
  })
  return `${target.prefix}${body}${target.suffix}`
}

/** Ease-out-expo: fast at first, slowing towards the final value. */
export function countUpProgress(elapsedMs: number, durationMs: number): number {
  if (durationMs <= 0 || elapsedMs >= durationMs) return 1
  if (elapsedMs <= 0) return 0
  return 1 - Math.pow(2, (-10 * elapsedMs) / durationMs)
}
