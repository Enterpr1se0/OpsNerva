import { FormEvent, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { FolderOutput, Download, LoaderCircle, ShieldAlert, UploadCloud } from 'lucide-react'
import { api } from '../../../api/api'
import { PasswordInput } from '../../../components/PasswordInput'
import { SettingsDisclosure } from '../../../components/SettingsDisclosure'
import { useNotifier } from '../../../lib/notifications'
import { errorText } from '../../../lib/utils'

export function ConfigurationTransferSettings({refreshModels,refreshHosts,refreshProxies,refreshCapabilities,refreshHealth}:{refreshModels:()=>Promise<void>;refreshHosts:()=>Promise<void>;refreshProxies:()=>Promise<void>;refreshCapabilities:()=>Promise<void>;refreshHealth:()=>Promise<void>}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [busy,setBusy]=useState<'export'|'import'|''>('')
	const [dialog,setDialog]=useState<'export'|'import'|null>(null)
	const [importFile,setImportFile]=useState<File|null>(null)
	const [password,setPassword]=useState('')
	const [confirmPassword,setConfirmPassword]=useState('')
	const [error,setError]=useState('')
	const openDialog=(mode:'export'|'import')=>{setDialog(mode);setImportFile(null);setPassword('');setConfirmPassword('');setError('')}
	const closeDialog=()=>{if(busy)return;setDialog(null);setImportFile(null);setPassword('');setConfirmPassword('');setError('')}
	const exportConfiguration=async(event:FormEvent)=>{event.preventDefault();if(busy)return;if(password!==confirmPassword){setError(t('config.passwordMismatch'));return}setBusy('export');setError('');try{const result=await api.exportConfiguration(password);const url=URL.createObjectURL(result.blob);const anchor=document.createElement('a');anchor.href=url;anchor.download=result.filename;document.body.appendChild(anchor);anchor.click();anchor.remove();window.setTimeout(()=>URL.revokeObjectURL(url),1000);notify(t('config.exported'));setDialog(null);setPassword('');setConfirmPassword('')}catch(err){setError(errorText(err))}finally{setBusy('')}}
	const importConfiguration=async(event:FormEvent)=>{event.preventDefault();if(!importFile||busy)return;setBusy('import');setError('');try{const result=await api.importConfiguration(importFile,password);await Promise.all([refreshModels(),refreshHosts(),refreshProxies(),refreshCapabilities(),refreshHealth()]);notify(t('config.imported',{models:result.model_providers,hosts:result.hosts,proxies:result.proxies}));setDialog(null);setImportFile(null);setPassword('')}catch(err){setError(errorText(err))}finally{setBusy('')}}
	return <>
		<SettingsDisclosure className="configuration-transfer-settings" icon={<FolderOutput size={18}/>} title={t('config.transfer')}>
			<div className="settings-action-row"><button type="button" onClick={()=>openDialog('import')}><UploadCloud size={15}/>{t('config.import')}</button><button type="button" className="primary" onClick={()=>openDialog('export')}><Download size={15}/>{t('config.export')}</button></div>
		</SettingsDisclosure>
		{dialog==='export'&&createPortal(<div className="configuration-transfer-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)closeDialog()}}><form className="configuration-transfer-dialog panel" role="dialog" aria-modal="true" aria-labelledby="configuration-export-title" onSubmit={exportConfiguration}><header><Download size={20}/><h2 id="configuration-export-title">{t('config.exportTitle')}</h2></header><label><span>{t('config.encryptionPassword')}</span><PasswordInput autoFocus autoComplete="new-password" minLength={8} maxLength={1024} value={password} onChange={event=>{setPassword(event.target.value);setError('')}}/></label><label><span>{t('config.confirmPassword')}</span><PasswordInput autoComplete="new-password" minLength={8} maxLength={1024} value={confirmPassword} onChange={event=>{setConfirmPassword(event.target.value);setError('')}}/></label>{error&&<div className="configuration-transfer-error" role="alert"><ShieldAlert size={14}/>{error}</div>}<footer><button type="button" disabled={busy==='export'} onClick={closeDialog}>{t('common.cancel')}</button><button className="primary" disabled={busy==='export'||password.length<8||confirmPassword.length<8}>{busy==='export'?<LoaderCircle className="spin" size={14}/>:<Download size={14}/>} {busy==='export'?t('config.exporting'):t('config.export')}</button></footer></form></div>,document.body)}
		{dialog==='import'&&createPortal(<div className="configuration-transfer-backdrop" onMouseDown={event=>{if(event.target===event.currentTarget)closeDialog()}}><form className="configuration-transfer-dialog panel" role="dialog" aria-modal="true" aria-labelledby="configuration-import-title" onSubmit={importConfiguration}><header><UploadCloud size={20}/><h2 id="configuration-import-title">{t('config.importTitle')}</h2></header><label className="configuration-import-file"><span>{t('config.package')}</span><input type="file" accept=".opsnerva-config,application/vnd.opsnerva.configuration" onChange={event=>{setImportFile(event.target.files?.[0]||null);setError('')}}/><b>{importFile?.name||t('config.choosePackage')}</b></label><label><span>{t('config.encryptionPassword')}</span><PasswordInput autoFocus autoComplete="off" minLength={8} maxLength={1024} value={password} onChange={event=>{setPassword(event.target.value);setError('')}}/></label>{error&&<div className="configuration-transfer-error" role="alert"><ShieldAlert size={14}/>{error}</div>}<footer><button type="button" disabled={busy==='import'} onClick={closeDialog}>{t('common.cancel')}</button><button className="primary" disabled={!importFile||busy==='import'||password.length<8}>{busy==='import'?<LoaderCircle className="spin" size={14}/>:<UploadCloud size={14}/>} {busy==='import'?t('config.importing'):t('config.import')}</button></footer></form></div>,document.body)}
	</>
}
