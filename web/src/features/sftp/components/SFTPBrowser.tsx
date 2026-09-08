import { useCallback, useEffect, useRef, useState, type DragEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, FolderOpen, PanelLeftClose, Plus, RefreshCw, UploadCloud, X } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { TextFileEditor } from '../../../components/TextFileEditor'
import { localeFor } from '../../../lib/i18n'
import { errorText, formatFileSize } from '../../../lib/utils'
import { useSFTPTransfer } from '../useFileTransfer'
import { useSFTPDirectory } from '../useSFTPDirectory'
import { useSFTPDeletion } from '../useSFTPDeletion'
import { useDocumentVisible } from '../../../lib/hooks'
import type { SFTPFileEntry, Host } from '../../../types'
import type { SFTPDeleteCandidate, SFTPNameEditor, SFTPTextFile } from '../types'
import { decodeTextFile, maxFilePreviewBytes, remoteChildPath, remoteParentPath } from '../utils'
import { FileBrowserTabs } from './FileBrowserTabs'
import { FileTransferProgress } from './FileTransferProgress'
import { SFTPNameDialog } from './SFTPNameDialog'
import { SFTPDeletionStatus } from './SFTPDeletionStatus'
import { SFTPFileList } from './SFTPFileList'

export function SFTPBrowser({host,active=true,embedded=false,hosts=[],onHostSelect,onWorkspaceMode,onCollapse}:{host?:Host;active?:boolean;embedded?:boolean;hosts?:Host[];onHostSelect?:(id:string)=>void;onWorkspaceMode?:()=>void;onCollapse?:()=>void}){
	const {t,i18n:instance}=useTranslation()
	const hostID=host?.id||''
	const [busy,setBusy]=useState(false)
	const deleting=useRef(false)
	const [notice,setNotice]=useState('')
	const [noticeError,setNoticeError]=useState(false)
	const [dragging,setDragging]=useState(false)
	const [inputKey,setInputKey]=useState(0)
	const [nameEditor,setNameEditor]=useState<SFTPNameEditor|null>(null)
	const [deleteCandidate,setDeleteCandidate]=useState<SFTPDeleteCandidate|null>(null)
	const [textFile,setTextFile]=useState<SFTPTextFile|null>(null)
	const [openingFile,setOpeningFile]=useState('')
	const previewRequest=useRef<AbortController|null>(null)
	const {operation:transfer,key:transferKey,uploadVersion,upload:uploadTransfer,download}=useSFTPTransfer(hostID,active)
	const visible=useDocumentVisible()
	const {manager:deletions,running:deletionRunning}=useSFTPDeletion(hostID,active&&visible)
	const mutating=busy||deletionRunning
	const {path,pathInput,setPathInput,entries,loading,listError,load,reconcileDeletion}=useSFTPDirectory(hostID,active,uploadVersion)
	useEffect(()=>deletions.store.onComplete(hostID,reconcileDeletion),[deletions,hostID,reconcileDeletion])
	useEffect(()=>()=>previewRequest.current?.abort(),[hostID,path,active,visible])
	const openEntry=useCallback(async(entry:SFTPFileEntry)=>{
		previewRequest.current?.abort()
		if(entry.type==='directory'){void load(entry.path);return}
		const controller=new AbortController()
		previewRequest.current=controller
		setOpeningFile(entry.path);setNotice('');setNoticeError(false)
		try{
			if((entry.size||0)>maxFilePreviewBytes)throw new Error(t('workspace.previewTooLarge'))
			const content=await api.sftpFile(hostID,entry.path,controller.signal)
			if(controller.signal.aborted)return
			const decoded=decodeTextFile(content,entry.name)
			setTextFile({entry,...decoded})
		}catch(err){if(!controller.signal.aborted){setNotice(errorText(err));setNoticeError(true)}}
		finally{if(previewRequest.current===controller){previewRequest.current=null;setOpeningFile('')}}
	},[hostID,load,t])
	const renameEntry=useCallback((entry:SFTPFileEntry)=>setNameEditor({mode:'rename',entry}),[])
	const deleteEntry=useCallback((entry:SFTPFileEntry)=>setDeleteCandidate({entry}),[])
	const uploadFiles=(files:File[])=>{
		if(!files.length||mutating||transfer)return
		setNotice('');setNoticeError(false);setInputKey(value=>value+1)
		uploadTransfer(path,files)
	}
	const saveName=async(name:string)=>{
		if(busy||!name.trim()||name==='.'||name==='..'||name.includes('/'))return
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			if(deletionRunning)throw new Error(t('sshWorkspace.deletionBusy'))
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
		if(!deleteCandidate||busy||deleting.current)return
		deleting.current=true
		setBusy(true);setNotice('');setNoticeError(false)
		try{
			await deletions.start(hostID,deleteCandidate.entry)
		}catch(err){
			setNotice(errorText(err));setNoticeError(true)
		}finally{deleting.current=false;setDeleteCandidate(null);setBusy(false)}
	}
	const saveTextFile=async(content:string)=>{
		if(!textFile)return
		if(deletionRunning)throw new Error(t('sshWorkspace.deletionBusy'))
		const result=await api.uploadSFTPTextFile(hostID,textFile.entry.path,content,textFile.encoding)
		setTextFile({entry:result.entry,content,binary:false,encoding:textFile.encoding})
		setNotice(t('sshWorkspace.saved',{path:textFile.entry.path}));setNoticeError(false)
		await load(path)
	}
	const acceptsFiles=(event:DragEvent<HTMLElement>)=>Array.from(event.dataTransfer.types).includes('Files')
	return <>
		<aside className={`sftp-browser panel ${embedded?'workspace-browser-panel chat-sftp-browser ':''}${dragging?'dragging':''}`} onDragEnter={event=>{if(hostID&&acceptsFiles(event)){event.preventDefault();setDragging(true)}}} onDragOver={event=>{if(hostID&&acceptsFiles(event)){event.preventDefault();event.dataTransfer.dropEffect=mutating||transfer?'none':'copy'}}} onDragLeave={event=>{event.preventDefault();if(!(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget)))setDragging(false)}} onDrop={event=>{if(!hostID||!acceptsFiles(event))return;event.preventDefault();setDragging(false);if(!mutating&&!transfer)void uploadFiles(Array.from(event.dataTransfer.files))}}>
			{embedded?<><header className="panel-header"><FileBrowserTabs mode="sftp" onChange={mode=>{if(mode==='workspace')onWorkspaceMode?.()}}/><div className="workspace-panel-actions"><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></header><div className="workspace-summary"><div className="chat-workspace-head sftp-workspace-head">{hosts.length>0?<AppSelect className="workspace-switch-select" value={host?.id||''} disabled={hosts.length<2} ariaLabel={t('config.tabs.hosts')} onChange={value=>onHostSelect?.(value)} options={hosts.map(item=>({value:item.id,label:item.name}))}/>:<span><b>{t('connections.noHosts')}</b></span>}{host&&<em>{host.auth_type.toUpperCase()}</em>}</div></div></>:<header><div><FolderOpen size={17}/><b>SFTP</b></div><span className="sftp-host">{host?`${host.name} · ${host.user}@${host.address}`:'—'}</span></header>}
			<form className="sftp-path" onSubmit={event=>{event.preventDefault();if(!busy)void load(pathInput)}}><button type="button" disabled={busy||!path||path==='/'} onClick={()=>void load(remoteParentPath(path))} title={t('workspace.parent')}>‹</button><input value={pathInput} disabled={busy||!hostID} onChange={event=>setPathInput(event.target.value)} aria-label={t('sshWorkspace.remotePath')}/><button type="submit" disabled={busy||!hostID||loading}><ChevronRight size={13}/></button><button type="button" disabled={busy||!hostID||loading} onClick={()=>void load(path)} title={t('common.refresh')}><RefreshCw className={loading?'spin':''} size={13}/></button></form>
			<div className="sftp-actions"><button type="button" disabled={mutating||!!transfer||!path} onClick={()=>setNameEditor({mode:'create'})}><Plus size={13}/>{t('sshWorkspace.newDirectory')}</button><label className={mutating||transfer||!path?'disabled':''}><UploadCloud size={13}/>{t('common.upload')}<input key={inputKey} type="file" multiple disabled={mutating||!!transfer||!path} onChange={event=>void uploadFiles(Array.from(event.target.files||[]))}/></label></div>
			<SFTPFileList entries={entries} active={active&&visible} loading={loading} error={listError} hostID={hostID} path={path} busy={busy} mutating={mutating} transferring={!!transfer} openingFile={openingFile} onOpen={openEntry} onDownload={download} onRename={renameEntry} onDelete={deleteEntry}/>
			<div className="sftp-feedback">
				<FileTransferProgress transferKey={transferKey} active={active}/>
				<SFTPDeletionStatus hostID={hostID} manager={deletions} active={active&&visible}/>
				{notice&&<div className={`sftp-notice ${noticeError?'error':''}`}>{notice}<button onClick={()=>setNotice('')}><X size={11}/></button></div>}
			</div>
			{dragging&&<div className="sftp-drop"><UploadCloud size={28}/><b>{t('workspace.dropFilesHere')}</b></div>}
		</aside>
		{nameEditor&&<SFTPNameDialog mode={nameEditor.mode} initialName={nameEditor.mode==='rename'?nameEditor.entry.name:''} busy={busy} onCancel={()=>setNameEditor(null)} onConfirm={name=>void saveName(name)}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('sshWorkspace.deleteTitle',{name:deleteCandidate.entry.name})} busy={busy} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void remove()}/>}
		{textFile&&<TextFileEditor path={textFile.entry.path} meta={`${textFile.entry.mode} · ${formatFileSize(textFile.entry.size||0)} · ${textFile.encoding.toUpperCase()} · ${new Date(textFile.entry.modified_at).toLocaleString(localeFor(instance.language))}`} content={textFile.content} binary={textFile.binary} editable={!mutating} onClose={()=>setTextFile(null)} onSave={saveTextFile} onDownload={()=>download(textFile.entry)}/>}
	</>
}
