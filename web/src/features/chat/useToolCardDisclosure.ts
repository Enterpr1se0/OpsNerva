import { useCallback, useRef, useState, type AnimationEvent } from 'react'

type DisclosurePhase = 'closed' | 'open' | 'closing'

const closeAnimationName = 'app-disclosure-content-out'
const reducedMotionQuery = '(prefers-reduced-motion: reduce)'

export function useToolCardDisclosure(onDisclosure: (summary: HTMLElement) => void) {
	const [phase, setPhase] = useState<DisclosurePhase>('closed')
	const closingSummary = useRef<HTMLElement | null>(null)

	const toggle = useCallback((summary: HTMLElement) => {
		if (phase === 'open') {
			if (window.matchMedia(reducedMotionQuery).matches) {
				onDisclosure(summary)
				setPhase('closed')
			} else {
				closingSummary.current = summary
				setPhase('closing')
			}
			return false
		}

		closingSummary.current = null
		onDisclosure(summary)
		setPhase('open')
		return true
	}, [onDisclosure, phase])

	const finishClosing = useCallback((event: AnimationEvent<HTMLElement>) => {
		if (phase !== 'closing' || event.target !== event.currentTarget || event.animationName !== closeAnimationName) return
		const summary = closingSummary.current
		closingSummary.current = null
		if (summary) onDisclosure(summary)
		setPhase('closed')
	}, [onDisclosure, phase])

	return {
		open: phase !== 'closed',
		closing: phase === 'closing',
		toggle,
		finishClosing,
	}
}
