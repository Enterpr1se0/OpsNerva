import { useEffect, useEffectEvent, useRef, useState } from 'react'

export function useDocumentVisible(){
	const [visible,setVisible]=useState(()=>document.visibilityState==='visible')
	useEffect(()=>{
		const sync=()=>setVisible(document.visibilityState==='visible')
		sync()
		document.addEventListener('visibilitychange',sync)
		return()=>document.removeEventListener('visibilitychange',sync)
	},[])
	return visible
}

export function useDebouncedValue<T>(value:T,delay:number){
	const [settled,setSettled]=useState(value)
	useEffect(()=>{
		const timer=window.setTimeout(()=>setSettled(value),delay)
		return()=>window.clearTimeout(timer)
	},[value,delay])
	return settled
}

export function useAutoCollapseDetails(open:boolean,onClose:()=>void){
	const detailsRef=useRef<HTMLDetailsElement>(null)
	const close=useEffectEvent(onClose)
	useEffect(()=>{
		if(!open)return
		const outside=(event:Event)=>{
			const target=event.target
			if(target instanceof Node&&!detailsRef.current?.contains(target))close()
		}
		const escape=(event:KeyboardEvent)=>{
			if(event.key!=='Escape')return
			event.preventDefault()
			close()
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
