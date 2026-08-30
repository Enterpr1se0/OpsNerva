import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { FileText, LoaderCircle, UploadCloud } from 'lucide-react'

export function SFTPOverwriteDialog({path,busy,onCancel,onConfirm}:{path:string;busy:boolean;onCancel:()=>void;onConfirm:()=>void}){
	const {t}=useTranslation()
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><section className="sftp-overwrite-dialog panel"><header><FileText size={19}/><h2>{t('sshWorkspace.overwriteTitle')}</h2></header><code>{path}</code><footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button type="button" className="primary" disabled={busy} onClick={onConfirm}>{busy?<LoaderCircle className="spin" size={13}/>:<UploadCloud size={13}/>} {t('sshWorkspace.overwrite')}</button></footer></section></div>,document.body)
}
