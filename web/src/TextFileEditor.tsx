import { useEffect, useState, type KeyboardEvent } from 'react'
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

type IndentEdit = {position:number;remove:number;insert:string}

function applyIndentEdits(value:string,edits:IndentEdit[]){
	let result=value
	for(let index=edits.length-1;index>=0;index--){
		const edit=edits[index]
		result=result.slice(0,edit.position)+edit.insert+result.slice(edit.position+edit.remove)
	}
	return result
}

function moveSelection(position:number,edits:IndentEdit[]){
	let result=position
	for(const edit of edits){
		if(edit.remove===0){
			if(position>=edit.position)result+=edit.insert.length
			continue
		}
		if(position>=edit.position+edit.remove)result+=edit.insert.length-edit.remove
		else if(position>edit.position)result+=edit.insert.length-(position-edit.position)
	}
	return result
}

function lineStartsInSelection(value:string,start:number,end:number){
	const first=value.lastIndexOf('\n',Math.max(0,start-1))+1
	const endProbe=end>start&&value[end-1]==='\n'?end-1:end
	const last=value.lastIndexOf('\n',Math.max(0,endProbe-1))+1
	const starts=[first]
	for(let position=value.indexOf('\n',first);position>=0&&position<last;position=value.indexOf('\n',position+1))starts.push(position+1)
	return starts
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
	const indent=(event:KeyboardEvent<HTMLTextAreaElement>)=>{
		if(event.key!=='Tab'||event.ctrlKey||event.metaKey||event.altKey)return
		event.preventDefault()
		const input=event.currentTarget,value=input.value,start=input.selectionStart,end=input.selectionEnd
		if(!event.shiftKey&&start===end){
			const next=value.slice(0,start)+'\t'+value.slice(end)
			setDraft(next)
			requestAnimationFrame(()=>input.setSelectionRange(start+1,start+1))
			return
		}
		const edits=lineStartsInSelection(value,start,end).map(position=>{
			if(!event.shiftKey)return{position,remove:0,insert:'\t'}
			if(value[position]==='\t')return{position,remove:1,insert:''}
			const spaces=value.slice(position,position+4).match(/^ {1,4}/)?.[0].length||0
			return{position,remove:spaces,insert:''}
		}).filter(edit=>edit.remove>0||edit.insert)
		if(!edits.length)return
		const next=applyIndentEdits(value,edits),nextStart=moveSelection(start,edits),nextEnd=moveSelection(end,edits)
		setDraft(next)
		requestAnimationFrame(()=>input.setSelectionRange(nextStart,nextEnd))
	}
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
			{binary?<div className="workspace-binary-preview"><FileText size={30}/><b>{t('workspace.binary')}</b></div>:editing?<textarea className="text-file-input" value={draft} onChange={event=>setDraft(event.target.value)} onKeyDown={indent} spellCheck={false} autoFocus/>:<pre>{content}</pre>}
			{editing&&<footer className="text-file-footer">
				{error&&<span>{error}</span>}
				<div><button type="button" disabled={saving} onClick={()=>{setDraft(content);setError('');setEditing(false)}}>{t('common.cancel')}</button><button type="button" className="primary" disabled={saving||draft===content} onClick={()=>void save()}>{saving?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {saving?t('common.saving'):t('common.save')}</button></div>
			</footer>}
		</section>
	</div>,document.body)
}
