import { useRef, type KeyboardEventHandler, type UIEventHandler } from 'react'

import { HighlightedCode } from './HighlightedCode'

type HighlightedTextEditorProps = {
	value: string
	language?: string
	ariaLabel: string
	autoFocus?: boolean
	onChange: (value: string) => void
	onKeyDown?: KeyboardEventHandler<HTMLTextAreaElement>
}

export function HighlightedTextEditor({value,language,ariaLabel,autoFocus=false,onChange,onKeyDown}:HighlightedTextEditorProps) {
	const mirrorRef=useRef<HTMLPreElement>(null)
	const syncScroll:UIEventHandler<HTMLTextAreaElement>=event=>{
		const mirror=mirrorRef.current
		if(!mirror)return
		mirror.scrollTop=event.currentTarget.scrollTop
		mirror.scrollLeft=event.currentTarget.scrollLeft
	}
	const mirrorValue=value.endsWith('\n')?`${value} `:value||' '
	return <div className="highlighted-text-editor">
		<pre ref={mirrorRef} aria-hidden="true"><HighlightedCode code={mirrorValue} language={language}/></pre>
		<textarea className="text-file-input" aria-label={ariaLabel} value={value} wrap="off" onChange={event=>onChange(event.target.value)} onKeyDown={onKeyDown} onScroll={syncScroll} spellCheck={false} autoFocus={autoFocus}/>
	</div>
}
