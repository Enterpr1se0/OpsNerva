import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
import { Download, Edit3, FileText, LoaderCircle, Save, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { CopyButton } from './CopyButton'

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
	const [editing,setEditing]=useState(false)
	const [draft,setDraft]=useState(content)
	const [saving,setSaving]=useState(false)
	const [error,setError]=useState('')
	useEffect(()=>{setEditing(false);setDraft(content);setError('')},[path])
	useEffect(()=>{if(!editing)setDraft(content)},[content,editing])
	const save=async()=>{
		if(!onSave||saving||draft===content)return
		setSaving(true);setError('')
		try{await onSave(draft);setEditing(false)}
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
					{editable&&!binary&&!editing&&<button type="button" onClick={()=>{setDraft(content);setError('');setEditing(true)}} title={t('common.edit')}><Edit3 size={15}/></button>}
					<button type="button" disabled={saving} onClick={close} title={t('common.close')}><X size={16}/></button>
				</section>
			</header>
			{binary?<div className="workspace-binary-preview"><FileText size={30}/><b>{t('workspace.binary')}</b></div>:editing?<textarea className="text-file-input" value={draft} onChange={event=>setDraft(event.target.value)} spellCheck={false} autoFocus/>:<pre>{content}</pre>}
			{editing&&<footer className="text-file-footer">
				{error&&<span>{error}</span>}
				<div><button type="button" disabled={saving} onClick={()=>{setDraft(content);setError('');setEditing(false)}}>{t('common.cancel')}</button><button type="button" className="primary" disabled={saving||draft===content} onClick={()=>void save()}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('common.save')}</button></div>
			</footer>}
		</section>
	</div>,document.body)
}
