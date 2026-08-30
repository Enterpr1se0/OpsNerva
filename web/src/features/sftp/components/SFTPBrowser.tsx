import { useCallback, useEffect, useRef, useState, type DragEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, Download, Edit3, FileText, FolderOpen, LoaderCircle, PanelLeftClose, Plus, RefreshCw, Trash2, UploadCloud, X } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { TextFileEditor } from '../../../components/TextFileEditor'
import { localeFor } from '../../../lib/i18n'
import { errorText, formatFileSize } from '../../../lib/utils'
import { useSFTPTransfer } from '../transfer'
import type { SFTPFileEntry, Host } from '../../../types'
import type { SFTPDeleteCandidate, SFTPNameEditor, SFTPTextFile } from '../types'
import { decodeTextFile, maxFilePreviewBytes, remoteChildPath, remoteParentPath } from '../utils'
import { FileBrowserTabs } from './FileBrowserTabs'
import { FileTransferProgress } from './FileTransferProgress'
import { SFTPNameDialog } from './SFTPNameDialog'

export function SFTPBrowser({host,active=true,embedded=false,hosts=[],onHostSelect,onWorkspaceMode,onCollapse}:{host?:Host;active?:boolean;embedded?:boolean;hosts?:Host[];onHostSelect?:(id:string)=>void;onWorkspaceMode?:()=>void;onCollapse?:()=>void}){
	const {t,i18n:instance}=useTranslation()
	const hostID=host?.id||''
	const [path,setPath]=useState('')
	const [pathInput,setPathInput]=useState('')
	const [entries,setEntries]=useState<SFTPFileEntry[]>([])
	const [loading,setLoading]=useState(false)
	const [busy,setBusy]=useState(false)
	const [listError,setListError]=useState('')
	const [notice,setNotice]=useState('')
	const [noticeError,setNoticeError]=useState(false)
	const [dragging,setDragging]=useState(false)
	const [inputKey,setInputKey]=useState(0)
	const [nameEditor,setNameEditor]=useState<SFTPNameEditor|null>(null)
	const [deleteCandidate,setDeleteCandidate]=useState<SFTPDeleteCandidate|null>(null)
	const [textFile,setTextFile]=useState<SFTPTextFile|null>(null)
	const [openingFile,setOpeningFile]=useState('')
	const {active:transfer,uploadVersion,upload:uploadTransfer,download,cancel}=useSFTPTransfer(hostID,active)
	const loadRequest=useRef(0)
	const observedUploadVersion=useRef(uploadVersion)
	const load=useCallback(async(target='')=>{
		if(!hostID)return
		const request=++loadRequest.current
		setLoading(true);setListError('')
		try{
			const result=await api.sftpEntries(hostID,target)
			if(request!==loadRequest.current)return
			setPath(result.path);setPathInput(result.path);setEntries(result.entries||[])
		}catch(err){
			if(request!==loadRequest.current)return
			setEntries([]);setListError(errorText(err))
		}finally{if(request===loadRequest.current)setLoading(false)}
	},[hostID])
	useEffect(()=>{if(!active)return;observedUploadVersion.current=uploadVersion;void load('')},[active,load])
	useEffect(()=>{
		if(!active)return
		if(observedUploadVersion.current===uploadVersion)return
		observedUploadVersion.current=uploadVersion
		void load(path)
	},[active,load,path,uploadVersion])
	const openTextFile=async(entry:SFTPFileEntry)=>{
		if(openingFile)return
		setOpeningFile(entry.path);setNotice('');setNoticeError(false)
		try{
			if((entry.size||0)>maxFilePreviewBytes)throw new Error(t('workspace.previewTooLarge'))
			const decoded=decodeTextFile(await api.sftpFile(hostID,entry.path),entry.name)
			setTextFile({entry,...decoded})
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setOpeningFile('')}
	}
	const uploadFiles=(files:File[])=>{
		if(!files.length||busy||transfer)return
		setNotice('');setNoticeError(false);setInputKey(value=>value+1)
		uploadTransfer(path,files)
	}
	const saveName=async(name:string)=>{
		if(!name.trim()||name==='.'||name==='..'||name.includes('/'))return
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			if(nameEditor?.mode==='create'){
				await api.createSFTPDirectory(hostID,remoteChildPath(path,name))
				setNotice(t('sshWorkspace.directoryCreated'))
			}else if(nameEditor?.mode==='rename'){
				await api.renameSFTPEntry(hostID,nameEditor.entry.path,remoteChildPath(path,name))
				setNotice(t('sshWorkspace.renamed'))
			}
			setNameEditor(null);await load(path)
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setBusy(false)}
	}
	const remove=async()=>{
		if(!deleteCandidate)return
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			await api.deleteSFTPEntry(hostID,deleteCandidate.entry.path,deleteCandidate.entry.type==='directory')
			setNotice(t('sshWorkspace.deleted'));setDeleteCandidate(null);await load(path)
		}catch(err){setNotice(errorText(err));setNoticeError(true)}
		finally{setBusy(false)}
	}
	const saveTextFile=async(content:string)=>{
		if(!textFile)return
		const result=await api.uploadSFTPTextFile(hostID,textFile.entry.path,content,textFile.encoding)
		setTextFile({entry:result.entry,content,binary:false,encoding:textFile.encoding})
		setNotice(t('sshWorkspace.saved',{path:textFile.entry.path}));setNoticeError(false)
		await load(path)
	}
	const acceptsFiles=(event:DragEvent<HTMLElement>)=>Array.from(event.dataTransfer.types).includes('Files')
	return <>
		<aside className={`sftp-browser panel ${embedded?'workspace-browser-panel chat-sftp-browser ':''}${dragging?'dragging':''}`} onDragEnter={event=>{if(hostID&&acceptsFiles(event)){event.preventDefault();setDragging(true)}}} onDragOver={event=>{if(hostID&&acceptsFiles(event)){event.preventDefault();event.dataTransfer.dropEffect=busy||transfer?'none':'copy'}}} onDragLeave={event=>{event.preventDefault();if(!(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget)))setDragging(false)}} onDrop={event=>{if(!hostID||!acceptsFiles(event))return;event.preventDefault();setDragging(false);if(!busy&&!transfer)void uploadFiles(Array.from(event.dataTransfer.files))}}>
			{embedded?<><header className="panel-header"><FileBrowserTabs mode="sftp" onChange={mode=>{if(mode==='workspace')onWorkspaceMode?.()}}/><div className="workspace-panel-actions"><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></header><div className="workspace-summary"><div className="chat-workspace-head sftp-workspace-head">{hosts.length>0?<AppSelect className="workspace-switch-select" value={host?.id||''} disabled={hosts.length<2} ariaLabel={t('config.tabs.hosts')} onChange={value=>onHostSelect?.(value)} options={hosts.map(item=>({value:item.id,label:item.name}))}/>:<span><b>{t('connections.noHosts')}</b></span>}{host&&<em>{host.auth_type.toUpperCase()}</em>}</div></div></>:<header><div><FolderOpen size={17}/><b>SFTP</b></div><span className="sftp-host">{host?`${host.name} · ${host.user}@${host.address}`:'—'}</span></header>}
			<form className="sftp-path" onSubmit={event=>{event.preventDefault();void load(pathInput)}}><button type="button" disabled={!path||path==='/'} onClick={()=>void load(remoteParentPath(path))} title={t('workspace.parent')}>‹</button><input value={pathInput} disabled={!hostID} onChange={event=>setPathInput(event.target.value)} aria-label={t('sshWorkspace.remotePath')}/><button type="submit" disabled={!hostID||loading}><ChevronRight size={13}/></button><button type="button" disabled={!hostID||loading} onClick={()=>void load(path)} title={t('common.refresh')}><RefreshCw className={loading?'spin':''} size={13}/></button></form>
			<div className="sftp-actions"><button type="button" disabled={busy||!!transfer||!path} onClick={()=>setNameEditor({mode:'create'})}><Plus size={13}/>{t('sshWorkspace.newDirectory')}</button><label className={busy||transfer||!path?'disabled':''}><UploadCloud size={13}/>{t('common.upload')}<input key={inputKey} type="file" multiple disabled={busy||!!transfer||!path} onChange={event=>void uploadFiles(Array.from(event.target.files||[]))}/></label></div>
			<div className="sftp-list">{!hostID?<span className="sftp-state">{t('connections.noHosts')}</span>:loading?<span className="sftp-state"><LoaderCircle className="spin" size={14}/>{t('common.loading')}</span>:listError?<span className="sftp-state error">{listError}</span>:entries.length?entries.map(entry=><div className="sftp-row" key={`${entry.type}:${entry.path}`}><button type="button" className="sftp-entry" onClick={()=>entry.type==='directory'?void load(entry.path):void openTextFile(entry)} title={entry.path}>{openingFile===entry.path?<LoaderCircle className="spin" size={14}/>:entry.type==='directory'?<FolderOpen size={14}/>:<FileText size={14}/>}<span><b>{entry.name}</b><small>{entry.mode} · {entry.type==='directory'?'—':formatFileSize(entry.size||0)} · {new Date(entry.modified_at).toLocaleString(localeFor(instance.language))}</small></span></button>{entry.type!=='directory'&&<button type="button" disabled={!!transfer} onClick={()=>void download(entry)} title={t('common.download')}><Download size={12}/></button>}<button type="button" disabled={!!transfer} onClick={()=>setNameEditor({mode:'rename',entry})} title={t('sshWorkspace.rename')}><Edit3 size={12}/></button><button type="button" className="danger" disabled={!!transfer} onClick={()=>setDeleteCandidate({entry})} title={t('common.delete')}><Trash2 size={12}/></button></div>):<span className="sftp-state">{t('workspace.emptyDirectory')}</span>}</div>
			{transfer&&<FileTransferProgress transfer={transfer} onCancel={cancel}/>}
			{notice&&<div className={`sftp-notice ${noticeError?'error':''}`}>{notice}<button onClick={()=>setNotice('')}><X size={11}/></button></div>}
			{dragging&&<div className="sftp-drop"><UploadCloud size={28}/><b>{t('workspace.dropFilesHere')}</b></div>}
		</aside>
		{nameEditor&&<SFTPNameDialog mode={nameEditor.mode} initialName={nameEditor.mode==='rename'?nameEditor.entry.name:''} busy={busy} onCancel={()=>setNameEditor(null)} onConfirm={name=>void saveName(name)}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('sshWorkspace.deleteTitle',{name:deleteCandidate.entry.name})} busy={busy} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
		{textFile&&<TextFileEditor path={textFile.entry.path} meta={`${textFile.entry.mode} · ${formatFileSize(textFile.entry.size||0)} · ${textFile.encoding.toUpperCase()} · ${new Date(textFile.entry.modified_at).toLocaleString(localeFor(instance.language))}`} content={textFile.content} binary={textFile.binary} editable onClose={()=>setTextFile(null)} onSave={saveTextFile} onDownload={()=>download(textFile.entry)}/>}
	</>
}
