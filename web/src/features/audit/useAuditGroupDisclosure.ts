import { useCallback, useLayoutEffect, useRef, useState } from 'react'

const autoExpandRunLimit=20

export function useAuditGroupDisclosure(groupIDs:readonly string[],visibleRunCount:number){
	const [expanded,setExpanded]=useState<ReadonlySet<string>>(()=>new Set())
	const manuallyCollapsed=useRef(new Set<string>())

	useLayoutEffect(()=>{
		if(visibleRunCount>autoExpandRunLimit)return
		setExpanded(current=>{
			const next=new Set(current)
			let changed=false
			for(const id of groupIDs)if(!manuallyCollapsed.current.has(id)&&!next.has(id)){next.add(id);changed=true}
			return changed?next:current
		})
	},[groupIDs,visibleRunCount])

	const setOpen=useCallback((id:string,open:boolean)=>setExpanded(current=>{
		if(open)manuallyCollapsed.current.delete(id);else manuallyCollapsed.current.add(id)
		if(current.has(id)===open)return current
		const next=new Set(current)
		if(open)next.add(id);else next.delete(id)
		return next
	}),[])
	const reveal=useCallback((ids:readonly string[])=>{
		if(!ids.length)return
		setExpanded(current=>{
			const next=new Set(current)
			let changed=false
			for(const id of ids){
				manuallyCollapsed.current.delete(id)
				if(!next.has(id)){next.add(id);changed=true}
			}
			return changed?next:current
		})
	},[])
	const forget=useCallback((ids:readonly string[])=>{
		if(!ids.length)return
		setExpanded(current=>{
			const next=new Set(current)
			let changed=false
			for(const id of ids){
				manuallyCollapsed.current.delete(id)
				if(next.delete(id))changed=true
			}
			return changed?next:current
		})
	},[])

	return{expanded,setOpen,reveal,forget}
}
