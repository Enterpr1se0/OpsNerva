import { useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { fileListIndexes, fileListWindow } from './fileListWindow'

type Props<T>={entries:readonly T[];active:boolean;className:string;label:string;rowHeight:number;entryKey:(entry:T)=>string;renderEntry:(entry:T)=>ReactNode;empty:ReactNode}

export function VirtualFileList<T>({entries,active,className,label,rowHeight,entryKey,renderEntry,empty}:Props<T>){
	const container=useRef<HTMLDivElement>(null)
	const [window,setWindow]=useState(()=>fileListWindow(entries.length,0,0,rowHeight))
	const [focused,setFocused]=useState<number|null>(null)
	const pendingFocus=useRef<{index:number;last:boolean}|null>(null)
	useLayoutEffect(()=>{
		const element=container.current
		if(!element||!active)return
		let frame:number|undefined
		const measure=()=>{
			frame=undefined
			const next=fileListWindow(entries.length,element.scrollTop,element.clientHeight,rowHeight)
			setWindow(current=>current.start===next.start&&current.end===next.end?current:next)
		}
		const schedule=()=>{if(frame===undefined)frame=requestAnimationFrame(measure)}
		const observer=new ResizeObserver(schedule)
		observer.observe(element)
		element.addEventListener('scroll',schedule,{passive:true})
		measure()
		return()=>{observer.disconnect();element.removeEventListener('scroll',schedule);if(frame!==undefined)cancelAnimationFrame(frame)}
	},[active,entries.length,rowHeight])
	useLayoutEffect(()=>{
		const pending=pendingFocus.current
		if(!pending)return
		const buttons=container.current?.querySelectorAll<HTMLButtonElement>(`[data-file-index="${pending.index}"] button:not(:disabled)`)
		const button=pending.last?buttons?.[buttons.length-1]:buttons?.[0]
		if(button){pendingFocus.current=null;button.focus({preventScroll:true})}
	},[focused,window])
	const focusRow=(index:number,last=false)=>{
		const element=container.current
		if(!element||index<0||index>=entries.length)return
		pendingFocus.current={index,last}
		const top=index*rowHeight
		if(top<element.scrollTop)element.scrollTop=top
		else if(top+rowHeight>element.scrollTop+element.clientHeight)element.scrollTop=top+rowHeight-element.clientHeight
		setFocused(index)
		setWindow(fileListWindow(entries.length,element.scrollTop,element.clientHeight,rowHeight))
	}
	const keyDown=(event:KeyboardEvent<HTMLDivElement>)=>{
		if(event.altKey||event.ctrlKey||event.metaKey)return
		const row=(event.target as HTMLElement).closest<HTMLElement>('[data-file-index]')
		const index=row?Number(row.dataset.fileIndex):-1
		if(event.key==='Tab'&&row){
			const buttons=row.querySelectorAll('button:not(:disabled)')
			const last=event.shiftKey
			const boundary=last?buttons[0]:buttons[buttons.length-1]
			const next=index+(last?-1:1)
			if(event.target===boundary&&next>=0&&next<entries.length&&!container.current?.querySelector(`[data-file-index="${next}"]`)){
				event.preventDefault();focusRow(next,last)
			}
			return
		}
		const page=Math.max(1,Math.floor((container.current?.clientHeight||rowHeight)/rowHeight))
		const next=event.key==='ArrowDown'?index+1:event.key==='ArrowUp'?index-1:event.key==='Home'?0:event.key==='End'?entries.length-1:event.key==='PageDown'?index+page:event.key==='PageUp'?index-page:null
		if(next!==null&&entries.length){event.preventDefault();focusRow(Math.max(0,Math.min(entries.length-1,next)))}
	}
	const currentWindow={start:Math.min(window.start,entries.length),end:Math.min(window.end,entries.length)}
	return <div ref={container} className={`${className} virtual-file-list`} role="list" aria-label={label} tabIndex={0} onKeyDown={keyDown} onFocusCapture={event=>{
		const row=(event.target as HTMLElement).closest<HTMLElement>('[data-file-index]')
		if(row)setFocused(Number(row.dataset.fileIndex))
	}} onBlurCapture={event=>{if(!event.currentTarget.contains(event.relatedTarget))setFocused(null)}}>
		{active&&(entries.length?<div className="virtual-file-content" style={{height:entries.length*rowHeight}}>
			{fileListIndexes(currentWindow,focused,entries.length).map(index=><div className="virtual-file-row" role="listitem" aria-posinset={index+1} aria-setsize={entries.length} data-file-index={index} key={entryKey(entries[index])} style={{top:index*rowHeight,height:rowHeight}}>{renderEntry(entries[index])}</div>)}
		</div>:empty)}
	</div>
}
