import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { Check, ChevronDown } from 'lucide-react'
import type { ModelMetadata } from './types'

export type SelectOption = {
	value: string
	label: string
	disabled?: boolean
}

export function AppSelect({value,options,onChange,ariaLabel,disabled=false,className=''}:{value:string;options:SelectOption[];onChange:(value:string)=>void;ariaLabel:string;disabled?:boolean;className?:string}){
	const [open,setOpen]=useState(false)
	const [activeIndex,setActiveIndex]=useState(-1)
	const rootRef=useRef<HTMLDivElement>(null)
	const listID=useId()
	const selected=options.find(option=>option.value===value)
	const enabledIndexes=options.map((option,index)=>option.disabled?-1:index).filter(index=>index>=0)

	useEffect(()=>{
		if(!open)return
		const close=(event:PointerEvent)=>{if(!rootRef.current?.contains(event.target as Node))setOpen(false)}
		document.addEventListener('pointerdown',close)
		return()=>document.removeEventListener('pointerdown',close)
	},[open])

	const begin=()=>{
		if(disabled)return
		const selectedIndex=options.findIndex(option=>option.value===value&&!option.disabled)
		setActiveIndex(selectedIndex>=0?selectedIndex:(enabledIndexes[0]??-1))
		setOpen(current=>!current)
	}
	const move=(direction:1|-1)=>{
		if(!enabledIndexes.length)return
		const position=enabledIndexes.indexOf(activeIndex)
		setActiveIndex(enabledIndexes[(position+direction+enabledIndexes.length)%enabledIndexes.length])
	}
	const choose=(next:string)=>{onChange(next);setOpen(false)}

	return <div className={`app-select ${open?'open ':''}${className}`.trim()} ref={rootRef} onKeyDown={event=>{
		if(event.key==='Escape'){setOpen(false);return}
		if(event.key==='ArrowDown'||event.key==='ArrowUp'){
			event.preventDefault()
			if(!open){begin();return}
			move(event.key==='ArrowDown'?1:-1)
			return
		}
		if(open&&(event.key==='Enter'||event.key===' ')){
			event.preventDefault()
			const option=options[activeIndex]
			if(option&&!option.disabled)choose(option.value)
		}
	}}>
		<button type="button" className="app-select-trigger" aria-label={ariaLabel} aria-haspopup="listbox" aria-controls={listID} aria-expanded={open} disabled={disabled} onClick={begin}><span>{selected?.label||value}</span><ChevronDown size={14}/></button>
		{open&&<div id={listID} className="app-select-menu" role="listbox" aria-label={ariaLabel}>{options.map((option,index)=><button type="button" role="option" aria-selected={option.value===value} className={`${option.value===value?'selected ':''}${index===activeIndex?'active':''}`} disabled={option.disabled} onPointerMove={()=>setActiveIndex(index)} onClick={()=>choose(option.value)} key={option.value}><span>{option.label}</span>{option.value===value&&<Check size={13}/>}</button>)}</div>}
	</div>
}

export function ModelCombobox({value,models,metadata,onChange,placeholder,ariaLabel,invalid=false}:{value:string;models:string[];metadata?:Record<string,ModelMetadata>;onChange:(value:string)=>void;placeholder:string;ariaLabel:string;invalid?:boolean}){
	const [open,setOpen]=useState(false)
	const [showAll,setShowAll]=useState(false)
	const [activeIndex,setActiveIndex]=useState(-1)
	const rootRef=useRef<HTMLDivElement>(null)
	const inputRef=useRef<HTMLInputElement>(null)
	const listID=useId()
	const filtered=useMemo(()=>{
		const needle=showAll?'':value.trim().toLocaleLowerCase()
		return needle?models.filter(model=>model.toLocaleLowerCase().includes(needle)):models
	},[models,showAll,value])

	useEffect(()=>{
		if(!open)return
		const close=(event:PointerEvent)=>{if(!rootRef.current?.contains(event.target as Node))setOpen(false)}
		document.addEventListener('pointerdown',close)
		return()=>document.removeEventListener('pointerdown',close)
	},[open])

	const reveal=(all:boolean)=>{
		if(!models.length)return
		setShowAll(all)
		setActiveIndex(Math.max(0,models.indexOf(value)))
		setOpen(true)
	}
	const choose=(model:string)=>{onChange(model);setOpen(false);setShowAll(false);inputRef.current?.focus()}
	const move=(direction:1|-1)=>{
		if(!filtered.length)return
		setActiveIndex(current=>(current+direction+filtered.length)%filtered.length)
	}

	return <div className={`model-combobox ${open?'open ':''}${invalid?'invalid':''}`} ref={rootRef}>
		<div className="model-combobox-control">
			<input ref={inputRef} value={value} placeholder={placeholder} role="combobox" aria-label={ariaLabel} aria-autocomplete="list" aria-controls={listID} aria-expanded={open} aria-invalid={invalid} autoComplete="off" spellCheck={false} onChange={event=>{onChange(event.target.value);setShowAll(false);reveal(false)}} onFocus={()=>{if(models.length&&!value)reveal(true)}} onKeyDown={event=>{
				if(event.key==='Escape'){setOpen(false);return}
				if(event.key==='ArrowDown'||event.key==='ArrowUp'){event.preventDefault();if(!open)reveal(true);else move(event.key==='ArrowDown'?1:-1);return}
				if(event.key==='Enter'&&open&&filtered[activeIndex]){event.preventDefault();choose(filtered[activeIndex])}
			}}/>
			{models.length>0&&<button type="button" aria-label={ariaLabel} aria-expanded={open} onClick={()=>{if(open)setOpen(false);else{reveal(true);inputRef.current?.focus()}}}><ChevronDown size={14}/></button>}
		</div>
		{open&&filtered.length>0&&<div id={listID} className="model-combobox-menu" role="listbox" aria-label={ariaLabel}>{filtered.map((model,index)=>{const context=metadata?.[model]?.context_window;return <button type="button" role="option" aria-selected={model===value} className={`${model===value?'selected ':''}${index===activeIndex?'active':''}`} onPointerMove={()=>setActiveIndex(index)} onMouseDown={event=>event.preventDefault()} onClick={()=>choose(model)} key={model}><span>{model}</span>{context?<small>{formatTokens(context)}</small>:model===value?<Check size={13}/>:null}</button>})}</div>}
	</div>
}

function formatTokens(value:number){
	if(value<1000)return String(value)
	if(value<1_000_000)return `${Number((value/1000).toFixed(value<10_000?1:0))}K`
	return `${Number((value/1_000_000).toFixed(value<10_000_000?1:0))}M`
}
