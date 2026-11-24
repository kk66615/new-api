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
import { useEffect, useState } from 'react'

import { useThemeCustomization } from '@/context/theme-customization-provider'
import {
  BRUTALIST_PRESET,
  countUpProgress,
  formatCountUp,
  parseCountUpTarget,
  prefersReducedMotion,
} from '@/lib/brutalist-motion'

const DURATION_MS = 1100

/**
 * Renders a formatted figure ("$2.53", "61") and, under the MX Brutalist
 * preset, rolls it up from zero when it first appears or changes. Any other
 * preset, reduced motion, or a value that is not a plain number renders the
 * text as-is.
 */
export function CountUpText(props: { text: string | number }) {
  const text = String(props.text)
  const { customization } = useThemeCustomization()
  const animate =
    customization.preset === BRUTALIST_PRESET && !prefersReducedMotion()
  const [display, setDisplay] = useState(text)

  useEffect(() => {
    const target = animate ? parseCountUpTarget(text) : null
    if (!target || target.value === 0) {
      setDisplay(text)
      return
    }
    let frame = 0
    const start = performance.now()
    const step = (now: number) => {
      const progress = countUpProgress(now - start, DURATION_MS)
      if (progress >= 1) {
        setDisplay(text)
        return
      }
      setDisplay(formatCountUp(target, target.value * progress))
      frame = requestAnimationFrame(step)
    }
    setDisplay(formatCountUp(target, 0))
    frame = requestAnimationFrame(step)
    return () => cancelAnimationFrame(frame)
  }, [text, animate])

  return <>{display}</>
}
