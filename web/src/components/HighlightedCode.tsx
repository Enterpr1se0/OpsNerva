import { memo, useEffect, useMemo, useState } from 'react'
import type { HLJSApi } from 'highlight.js'

const outputLanguages=['bash','powershell','json','yaml','xml','diff','javascript','typescript','go','rust','python','sql']
const maxHighlightedChars=128<<10
const maxAutoDetectedChars=32<<10

const languageAliases:Record<string,string>={
	'c++':'cpp',
	'c#':'csharp',
	cs:'csharp',
	html:'xml',
	jsx:'javascript',
	js:'javascript',
	md:'markdown',
	ps1:'powershell',
	pwsh:'powershell',
	sh:'bash',
	shell:'bash',
	ts:'typescript',
	tsx:'typescript',
	yml:'yaml',
	zsh:'bash',
}

const extensionLanguages:Record<string,string>={
	bash:'bash',c:'c',cc:'cpp',conf:'ini',cpp:'cpp',cs:'csharp',css:'css',diff:'diff',env:'bash',go:'go',
	h:'c',hpp:'cpp',htm:'xml',html:'xml',ini:'ini',java:'java',js:'javascript',json:'json',jsonl:'json',
	jsx:'javascript',kt:'kotlin',kts:'kotlin',less:'less',lua:'lua',md:'markdown',php:'php',pl:'perl',
	ps1:'powershell',py:'python',rb:'ruby',rs:'rust',scss:'scss',sh:'bash',sql:'sql',swift:'swift',
	toml:'ini',ts:'typescript',tsx:'typescript',xml:'xml',yaml:'yaml',yml:'yaml',zsh:'bash',
}

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

function normalizeLanguage(language?:string){
	const normalized=language?.trim().toLowerCase()
	return normalized?(languageAliases[normalized]||normalized):undefined
}

export function languageFromCodeClass(className?:string){
	const language=className?.split(/\s+/).find(value=>value.startsWith('language-'))?.slice(9)
	return normalizeLanguage(language)
}

export function languageFromPath(path?:string){
	if(!path)return undefined
	const name=path.split(/[\\/]/).at(-1)?.toLowerCase().split(/[?#]/,1)[0]||''
	if(name==='dockerfile')return'bash'
	if(name==='makefile')return'makefile'
	const extension=name.includes('.')?name.slice(name.lastIndexOf('.')+1):''
	return extensionLanguages[extension]
}

export function inferScriptLanguage(script:string){
	if(/(?:^|\n)\s*(?:#requires\b|param\s*\()|\$(?:ErrorActionPreference|PSVersionTable)\b|\b(?:Get|Set|New|Remove|Write|Start|Stop)-[A-Z][\w-]*/m.test(script))return'powershell'
	return'bash'
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
