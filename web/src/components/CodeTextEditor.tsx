import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react'
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands'
import { bracketMatching, indentOnInput, indentUnit } from '@codemirror/language'
import { Compartment, EditorState, Text } from '@codemirror/state'
import { drawSelection, dropCursor, EditorView, highlightActiveLine, highlightSpecialChars, keymap } from '@codemirror/view'

import { loadCodeEditorLanguage } from './codeEditorLanguage'
import { codeEditorTheme } from './codeEditorTheme'

export type CodeTextEditorHandle = {getValue:()=>string}

type CodeTextEditorProps = {
	initialValue: string
	language?: string
	ariaLabel: string
	autoFocus?: boolean
	onDirtyChange: (dirty:boolean) => void
}

function lineSeparator(value:string){
	if(value.includes('\r\n'))return'\r\n'
	if(value.includes('\r'))return'\r'
	return'\n'
}

const CodeTextEditor=forwardRef<CodeTextEditorHandle,CodeTextEditorProps>(function CodeTextEditor({initialValue,language,ariaLabel,autoFocus=false,onDirtyChange},ref){
	const hostRef=useRef<HTMLDivElement>(null)
	const viewRef=useRef<EditorView>(null)
	const languageCompartmentRef=useRef<Compartment>(null)
	if(!languageCompartmentRef.current)languageCompartmentRef.current=new Compartment()
	const languageCompartment=languageCompartmentRef.current
	const onDirtyChangeRef=useRef(onDirtyChange)
	onDirtyChangeRef.current=onDirtyChange

	useImperativeHandle(ref,()=>({getValue:()=>viewRef.current!.state.sliceDoc()}),[])

	useEffect(()=>{
		const host=hostRef.current
		if(!host)return
		const initialDocument=Text.of(initialValue.split(/\r\n?|\n/))
		let dirty=false
		const state=EditorState.create({doc:initialDocument,extensions:[
			EditorState.lineSeparator.of(lineSeparator(initialValue)),
			EditorState.tabSize.of(4),
			EditorState.allowMultipleSelections.of(true),
			EditorView.contentAttributes.of({'aria-label':ariaLabel,'aria-multiline':'true',spellcheck:'false'}),
			highlightSpecialChars(),history(),drawSelection(),dropCursor(),indentOnInput(),bracketMatching(),highlightActiveLine(),
			indentUnit.of('\t'),
			keymap.of([...defaultKeymap,...historyKeymap,indentWithTab]),
			languageCompartment.of([]),
			codeEditorTheme,
			EditorView.updateListener.of(update=>{
				if(!update.docChanged)return
				const nextDirty=!update.state.doc.eq(initialDocument)
				if(nextDirty===dirty)return
				dirty=nextDirty
				onDirtyChangeRef.current(dirty)
			}),
		]})
		const view=new EditorView({state,parent:host})
		viewRef.current=view
		if(autoFocus)view.focus()
		return()=>{viewRef.current=null;view.destroy()}
	},[ariaLabel,autoFocus,initialValue,languageCompartment])

	useEffect(()=>{
		let active=true
		void loadCodeEditorLanguage(language).then(extension=>{
			const view=viewRef.current
			if(active&&view)view.dispatch({effects:languageCompartment.reconfigure(extension)})
		})
		return()=>{active=false}
	},[language,languageCompartment])

	return <div className="code-text-editor" ref={hostRef}/>
})

export default CodeTextEditor
