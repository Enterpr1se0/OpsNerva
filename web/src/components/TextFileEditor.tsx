import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Download, Edit3, FileText, LoaderCircle, Save, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { HighlightedCode, languageFromPath } from './HighlightedCode'
import type { CodeTextEditorHandle } from './CodeTextEditor'
import { CopyButton } from './CopyButton'

const CodeTextEditor=lazy(()=>import('./CodeTextEditor'))

type TextFileEditorProps = {
	path: string
	meta: string
	content: string
	binary?: boolean
	editable?: boolean
	onClose: () => void
	onSave?: (content: string) => Promise<void>
	onDownload?: () => void
}

export function TextFileEditor({path,meta,content,binary=false,editable=false,onClose,onSave,onDownload}:TextFileEditorProps){
	const {t}=useTranslation()
	const language=languageFromPath(path)
	const editorRef=useRef<CodeTextEditorHandle>(null)
	const [editing,setEditing]=useState(false)
	const [dirty,setDirty]=useState(false)
	const [saving,setSaving]=useState(false)
	const [error,setError]=useState('')
	useEffect(()=>{setEditing(false);setDirty(false);setError('')},[path])
	useEffect(()=>{if(!editing)setDirty(false)},[content,editing])
	const save=async()=>{
		const editor=editorRef.current
		if(!onSave||saving||!editor)return
		const draft=editor.getValue()
		if(draft===content){setDirty(false);return}
		setSaving(true);setError('')
		try{await onSave(draft);setDirty(false);setEditing(false)}
		catch(err){setError(err instanceof Error?err.message:String(err))}
		finally{setSaving(false)}
	}
	const close=()=>{if(!saving)onClose()}
	return createPortal(<div className="workspace-preview-backdrop" role="presentation" onMouseDown={event=>{if(event.target===event.currentTarget)close()}}>
		<section className="workspace-preview-dialog" role="dialog" aria-modal="true" aria-label={path}>
			<header>
				<div><FileText size={18}/><span><b>{path}</b><small>{meta}</small></span></div>
				<section className="text-file-actions">
					{!binary&&!editing&&<CopyButton value={content}/>}
					{onDownload&&<button type="button" disabled={saving} onClick={onDownload} title={t('common.download')}><Download size={15}/></button>}
					{editable&&!binary&&!editing&&<button type="button" onClick={()=>{setDirty(false);setError('');setEditing(true)}} title={t('common.edit')}><Edit3 size={15}/></button>}
					<button type="button" disabled={saving} onClick={close} title={t('common.close')}><X size={16}/></button>
				</section>
			</header>
			{binary?<div className="workspace-binary-preview"><FileText size={30}/><b>{t('workspace.binary')}</b></div>:editing?<Suspense fallback={<div className="code-text-editor-loading"><LoaderCircle className="spin" size={18}/></div>}><CodeTextEditor ref={editorRef} initialValue={content} language={language} ariaLabel={path} onDirtyChange={setDirty} autoFocus/></Suspense>:<pre><HighlightedCode code={content} language={language} autoDetect/></pre>}
			{editing&&<footer className="text-file-footer">
				{error&&<span>{error}</span>}
				<div><button type="button" disabled={saving} onClick={()=>{setDirty(false);setError('');setEditing(false)}}>{t('common.cancel')}</button><button type="button" className="primary" disabled={saving||!dirty} onClick={()=>void save()}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('common.save')}</button></div>
			</footer>}
		</section>
	</div>,document.body)
}
