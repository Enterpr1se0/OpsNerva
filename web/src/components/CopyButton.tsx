import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Check, Copy } from 'lucide-react'
import { useTranslation } from 'react-i18next'

type CopyValue = string | (() => string)

export async function writeClipboard(value:string){
	try{
		if(navigator.clipboard?.writeText){await navigator.clipboard.writeText(value);return}
	}catch{/* use the document fallback below */}
	const textarea=document.createElement('textarea')
	textarea.value=value;textarea.readOnly=true;textarea.style.position='fixed';textarea.style.opacity='0'
	document.body.appendChild(textarea);textarea.select()
	const copied=document.execCommand('copy')
	textarea.remove()
	if(!copied)throw new Error('clipboard write failed')
}

export function CopyButton({value,className=''}:{value:CopyValue;className?:string}){
	const {t}=useTranslation()
	const [copied,setCopied]=useState(false)
	const timer=useRef<number|undefined>(undefined)
	useEffect(()=>()=>{if(timer.current!==undefined)window.clearTimeout(timer.current)},[])
	const copy=async()=>{
		const text=typeof value==='function'?value():value
		if(!text)return
		try{
			await writeClipboard(text)
			setCopied(true)
			if(timer.current!==undefined)window.clearTimeout(timer.current)
			timer.current=window.setTimeout(()=>setCopied(false),1600)
		}catch{setCopied(false)}
	}
	return <button type="button" className={`code-copy-button ${copied?'copied':''} ${className}`.trim()} title={t(copied?'common.copied':'common.copy')} aria-label={t(copied?'common.copied':'common.copy')} onClick={event=>{event.preventDefault();event.stopPropagation();void copy()}}>{copied?<Check size={13}/>:<Copy size={13}/>}</button>
}

export function CopyablePre({children,value,className='',preClassName=''}:{children:ReactNode;value?:CopyValue;className?:string;preClassName?:string}){
	const preRef=useRef<HTMLPreElement>(null)
	return <div className={`copyable-pre ${className}`.trim()}><CopyButton value={value||(()=>preRef.current?.textContent||'')}/><pre ref={preRef} className={preClassName||undefined}>{children}</pre></div>
}
