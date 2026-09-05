import { memo, useEffect, useMemo, useState } from 'react'
import type { HLJSApi } from 'highlight.js'
import { normalizeLanguage } from '../lib/codeLanguage'

const outputLanguages=['bash','powershell','json','yaml','xml','diff','javascript','typescript','go','rust','python','sql']
const maxHighlightedChars=128<<10
const maxAutoDetectedChars=32<<10

let loadedHighlighter:HLJSApi|undefined
let highlighterPromise:Promise<HLJSApi>|undefined

function loadHighlighter(){
	if(!highlighterPromise)highlighterPromise=Promise.all([
		import('highlight.js/lib/common'),
		import('highlight.js/lib/languages/powershell'),
	]).then(([common,powershell])=>{
		const highlighter=common.default
		if(!highlighter.getLanguage('powershell'))highlighter.registerLanguage('powershell',powershell.default)
		loadedHighlighter=highlighter
		return highlighter
	})
	return highlighterPromise
}

function highlight(highlighter:HLJSApi,code:string,language:string|undefined){
	return language&&highlighter.getLanguage(language)
		?highlighter.highlight(code,{language,ignoreIllegals:true}).value
		:highlighter.highlightAuto(code,outputLanguages).value
}

export const HighlightedCode=memo(function HighlightedCode({code,language,autoDetect=false,live=false,className=''}:{code:string;language?:string;autoDetect?:boolean;live?:boolean;className?:string}){
	const normalizedLanguage=normalizeLanguage(language)
	const highlightable=!live&&!!code&&code.length<=maxHighlightedChars&&(!!normalizedLanguage||autoDetect&&code.length<=maxAutoDetectedChars)
	const [highlighter,setHighlighter]=useState<HLJSApi|undefined>(loadedHighlighter)
	useEffect(()=>{
		if(!highlightable||highlighter)return
		let active=true
		void loadHighlighter().then(value=>{if(active)setHighlighter(value)})
		return()=>{active=false}
	},[highlightable,highlighter])
	const html=useMemo(()=>highlightable&&highlighter?highlight(highlighter,code,normalizedLanguage):undefined,[code,highlightable,highlighter,normalizedLanguage])
	return <code className={`syntax-code ${normalizedLanguage?`language-${normalizedLanguage}`:''} ${className}`.trim()}>{html!==undefined?<span dangerouslySetInnerHTML={{__html:html}}/>:code}</code>
})
