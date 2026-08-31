import { useCallback, useEffect, useRef, useState, type TransitionEvent } from 'react'

type DisclosurePhase = 'closed' | 'opening' | 'open' | 'closing'

const reducedMotionQuery = '(prefers-reduced-motion: reduce)'
const detailsContentSelector = 'selector(details::details-content)'

export type ChatDisclosurePositionHandler = (disclosure: symbol, summary: HTMLElement | null, holdAnchor: boolean) => void

function canAnimateDisclosure() {
	return !window.matchMedia(reducedMotionQuery).matches
		&& CSS.supports('interpolate-size', 'allow-keywords')
		&& CSS.supports(detailsContentSelector)
}

export function useChatCardDisclosure(onDisclosure: ChatDisclosurePositionHandler, transitionTargetClass: string) {
	const [phase, setPhase] = useState<DisclosurePhase>('closed')
	const disclosure = useRef(Symbol('chat-card-disclosure'))
	const transitionSummary = useRef<HTMLElement | null>(null)
	useEffect(() => {
		if (phase !== 'opening') return
		const frame = window.requestAnimationFrame(() => setPhase(current => current === 'opening' ? 'open' : current))
		return () => window.cancelAnimationFrame(frame)
	}, [phase])
	useEffect(() => () => {
		if (!transitionSummary.current) return
		transitionSummary.current = null
		onDisclosure(disclosure.current, null, false)
	}, [onDisclosure])

	const toggle = useCallback((summary: HTMLElement) => {
		if (phase === 'opening') {
			transitionSummary.current = null
			onDisclosure(disclosure.current, summary, false)
			setPhase('closed')
			return false
		}
		const opening = phase === 'closed' || phase === 'closing'
		const animated = canAnimateDisclosure()
		if (animated) {
			transitionSummary.current = summary
			onDisclosure(disclosure.current, summary, true)
			if (phase === 'closed') setPhase('opening')
			else if (phase === 'closing') setPhase('open')
			else setPhase('closing')
		} else {
			transitionSummary.current = null
			onDisclosure(disclosure.current, summary, false)
			setPhase(opening ? 'open' : 'closed')
		}
		return opening
	}, [onDisclosure, phase])

	const finishTransition = useCallback((event: TransitionEvent<HTMLDetailsElement>) => {
		const target = event.target
		if (event.propertyName !== 'transform' || !(target instanceof SVGElement) || !target.classList.contains(transitionTargetClass)) return
		const summary = transitionSummary.current
		transitionSummary.current = null
		if (summary) onDisclosure(disclosure.current, summary, false)
		if (phase === 'closing') setPhase('closed')
	}, [onDisclosure, phase, transitionTargetClass])

	return {
		expanded: phase === 'open',
		renderBody: phase !== 'closed',
		toggle,
		finishTransition,
	}
}
