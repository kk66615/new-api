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
import * as React from 'react'

const MOBILE_BREAKPOINT = 768

export function useIsMobile() {
  // Resolve the real viewport width during the very first render.
  //
  // The previous implementation initialised the state to `undefined` and only
  // filled it in from an effect, so the first render always reported "desktop"
  // even on phones. Consumers that branch on this value (SidebarProvider's
  // toggleSidebar, Sidebar's mobile Sheet vs. fixed rail) therefore mounted the
  // desktop variant first and swapped afterwards, which made early taps on the
  // sidebar toggle/menu items go to the desktop code path and appear to do
  // nothing. Seeding the state lazily removes that first-render mismatch.
  const [isMobile, setIsMobile] = React.useState<boolean>(
    () =>
      typeof window !== 'undefined' && window.innerWidth < MOBILE_BREAKPOINT
  )

  React.useEffect(() => {
    const mql = window.matchMedia(`(max-width: ${MOBILE_BREAKPOINT - 1}px)`)
    const onChange = () => {
      setIsMobile(window.innerWidth < MOBILE_BREAKPOINT)
    }
    mql.addEventListener('change', onChange)
    setIsMobile(window.innerWidth < MOBILE_BREAKPOINT)
    return () => mql.removeEventListener('change', onChange)
  }, [])

  return isMobile
}
