import type { Extension } from '@codemirror/state'
import { StreamLanguage, type StreamParser } from '@codemirror/language'

type LanguageLoader = () => Promise<Extension>

async function streamMode<T>(module:Promise<T>,select:(value:T)=>StreamParser<unknown>){
	return StreamLanguage.define(select(await module))
}

const languageLoaders:Record<string,LanguageLoader>={
	bash:()=>streamMode(import('@codemirror/legacy-modes/mode/shell'),module=>module.shell),
	c:()=>streamMode(import('@codemirror/legacy-modes/mode/clike'),module=>module.c),
	cpp:()=>streamMode(import('@codemirror/legacy-modes/mode/clike'),module=>module.cpp),
	csharp:()=>streamMode(import('@codemirror/legacy-modes/mode/clike'),module=>module.csharp),
	css:()=>streamMode(import('@codemirror/legacy-modes/mode/css'),module=>module.css),
	diff:()=>streamMode(import('@codemirror/legacy-modes/mode/diff'),module=>module.diff),
	dockerfile:()=>streamMode(import('@codemirror/legacy-modes/mode/dockerfile'),module=>module.dockerFile),
	go:()=>streamMode(import('@codemirror/legacy-modes/mode/go'),module=>module.go),
	html:()=>streamMode(import('@codemirror/legacy-modes/mode/xml'),module=>module.html),
	ini:()=>streamMode(import('@codemirror/legacy-modes/mode/properties'),module=>module.properties),
	java:()=>streamMode(import('@codemirror/legacy-modes/mode/clike'),module=>module.java),
	javascript:()=>streamMode(import('@codemirror/legacy-modes/mode/javascript'),module=>module.javascript),
	json:()=>streamMode(import('@codemirror/legacy-modes/mode/javascript'),module=>module.json),
	jsonl:()=>streamMode(import('@codemirror/legacy-modes/mode/javascript'),module=>module.json),
	jsx:()=>streamMode(import('@codemirror/legacy-modes/mode/javascript'),module=>module.javascript),
	kotlin:()=>streamMode(import('@codemirror/legacy-modes/mode/clike'),module=>module.kotlin),
	less:()=>streamMode(import('@codemirror/legacy-modes/mode/css'),module=>module.less),
	lua:()=>streamMode(import('@codemirror/legacy-modes/mode/lua'),module=>module.lua),
	markdown:()=>import('@codemirror/lang-markdown').then(module=>module.markdown()),
	perl:()=>streamMode(import('@codemirror/legacy-modes/mode/perl'),module=>module.perl),
	php:()=>import('@codemirror/lang-php').then(module=>module.php()),
	powershell:()=>streamMode(import('@codemirror/legacy-modes/mode/powershell'),module=>module.powerShell),
	python:()=>streamMode(import('@codemirror/legacy-modes/mode/python'),module=>module.python),
	ruby:()=>streamMode(import('@codemirror/legacy-modes/mode/ruby'),module=>module.ruby),
	rust:()=>streamMode(import('@codemirror/legacy-modes/mode/rust'),module=>module.rust),
	scss:()=>streamMode(import('@codemirror/legacy-modes/mode/css'),module=>module.sCSS),
	sql:()=>streamMode(import('@codemirror/legacy-modes/mode/sql'),module=>module.standardSQL),
	swift:()=>streamMode(import('@codemirror/legacy-modes/mode/swift'),module=>module.swift),
	toml:()=>streamMode(import('@codemirror/legacy-modes/mode/toml'),module=>module.toml),
	tsx:()=>streamMode(import('@codemirror/legacy-modes/mode/javascript'),module=>module.typescript),
	typescript:()=>streamMode(import('@codemirror/legacy-modes/mode/javascript'),module=>module.typescript),
	xml:()=>streamMode(import('@codemirror/legacy-modes/mode/xml'),module=>module.xml),
	yaml:()=>streamMode(import('@codemirror/legacy-modes/mode/yaml'),module=>module.yaml),
}

const loadedLanguages=new Map<string,Promise<Extension>>()

export function loadCodeEditorLanguage(language?:string):Promise<Extension>{
	if(!language)return Promise.resolve([])
	const loader=languageLoaders[language]
	if(!loader)return Promise.resolve([])
	let loaded=loadedLanguages.get(language)
	if(!loaded){loaded=loader();loadedLanguages.set(language,loaded)}
	return loaded
}
