import { useEffect } from 'react'
import { useTranslation } from 'react-i18next'
import { LoaderCircle, Trash2 } from 'lucide-react'

export function DestructiveConfirmDialog({title,busy,onCancel,onConfirm}:{title:string;busy:boolean;onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	useEffect(()=>{const close=(event:KeyboardEvent)=>{if(event.key==='Escape'&&!busy)onCancel()};window.addEventListener('keydown',close);return()=>window.removeEventListener('keydown',close)},[busy,onCancel])
	return <div className="destructive-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><section className="destructive-dialog panel" role="dialog" aria-modal="true" aria-labelledby="destructive-dialog-title"><header><Trash2 size={21}/><h2 id="destructive-dialog-title">{title}</h2></header><footer><button type="button" autoFocus disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="danger" disabled={busy} onClick={onConfirm}>{busy?<LoaderCircle className="spin" size={14}/>:<Trash2 size={14}/>} {busy?t('common.deleting'):t('common.delete')}</button></footer></section></div>
}
