import { useEffect, useRef } from 'react'

export function useAutoCollapseDetails(open:boolean,onClose:()=>void){
	const detailsRef=useRef<HTMLDetailsElement>(null)
	const closeRef=useRef(onClose)
	closeRef.current=onClose
	useEffect(()=>{
		if(!open)return
		const outside=(event:Event)=>{
			const target=event.target
			if(target instanceof Node&&!detailsRef.current?.contains(target))closeRef.current()
		}
		const escape=(event:KeyboardEvent)=>{
			if(event.key!=='Escape')return
			event.preventDefault()
			closeRef.current()
			detailsRef.current?.querySelector<HTMLElement>('summary')?.focus()
		}
		document.addEventListener('pointerdown',outside,true)
		document.addEventListener('focusin',outside,true)
		document.addEventListener('keydown',escape,true)
		return()=>{
			document.removeEventListener('pointerdown',outside,true)
			document.removeEventListener('focusin',outside,true)
			document.removeEventListener('keydown',escape,true)
		}
	},[open])
	return detailsRef
}
