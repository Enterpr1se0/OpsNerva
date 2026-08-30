import { useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { FolderOpen, LoaderCircle, Save, X } from 'lucide-react'

export function SFTPNameDialog({mode,initialName,busy,onCancel,onConfirm}:{mode:'create'|'rename';initialName:string;busy:boolean;onCancel:()=>void;onConfirm:(name:string)=>void}){
	const {t}=useTranslation()
	const [name,setName]=useState(initialName)
	const valid=!!name.trim()&&name!=='.'&&name!=='..'&&!name.includes('/')
	return createPortal(<div className="connection-dialog-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget&&!busy)onCancel()}}><form className="connection-dialog compact panel" noValidate onSubmit={event=>{event.preventDefault();if(valid)onConfirm(name)}}><header><span><FolderOpen size={19}/><span><small>SFTP</small><h2>{t(mode==='create'?'sshWorkspace.newDirectory':'sshWorkspace.rename')}</h2></span></span><button type="button" disabled={busy} onClick={onCancel}><X size={15}/></button></header><div className="connection-dialog-fields single"><label><span>{t('sshWorkspace.name')}</span><input value={name} onChange={event=>setName(event.target.value)} autoFocus/></label></div><footer><button type="button" disabled={busy} onClick={onCancel}>{t('common.cancel')}</button><button className="primary" disabled={busy||!valid}>{busy?<LoaderCircle className="spin" size={13}/>:<Save size={13}/>} {t('common.save')}</button></footer></form></div>,document.body)
}
