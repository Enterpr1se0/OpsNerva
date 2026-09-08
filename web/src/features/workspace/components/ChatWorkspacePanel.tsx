import { memo, useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { invoke } from '@tauri-apps/api/core'
import { FolderOpen, FolderUp, LoaderCircle, PanelLeftClose, RefreshCw, TerminalSquare, UploadCloud, X } from 'lucide-react'
import { api } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { TextFileEditor } from '../../../components/TextFileEditor'
import { FileBrowserTabs, FileTransferProgress, SFTPBrowser, useWorkspaceTransfer, type FileBrowserMode } from '../../../features/sftp'
import { sshShellActive } from '../../../features/ssh'
import { desktopRuntime, errorText, formatFileSize } from '../../../lib/utils'
import type { Host, SSHShell, WorkspaceCapability, WorkspaceFilePreview } from '../../../types'
import { workspaceChildPath } from '../utils'
import { workspaceUploadDrop, workspaceUploadFiles } from '../upload'
import type { WorkspaceDeleteCandidate, WorkspaceNotice } from '../types'
import { useDocumentVisible } from '../../../lib/hooks'
import { useWorkspaceDirectory } from '../useWorkspaceDirectory'
import { WorkspaceFileList } from './WorkspaceFileList'

export const ChatWorkspacePanel=memo(function ChatWorkspacePanel({active,mode,onModeChange,workspaces,workspaceID,hosts,sftpHostID,onSFTPHostChange,shells,switching,disabled,bound,onSelect,onCreateShell,onOpenShell,onCollapse}:{active:boolean;mode:FileBrowserMode;onModeChange:(mode:FileBrowserMode)=>void;workspaces:WorkspaceCapability[];workspaceID:string;hosts:Host[];sftpHostID:string;onSFTPHostChange:(id:string)=>void;shells:SSHShell[];switching:boolean;disabled:boolean;bound:boolean;onSelect:(id:string)=>void|Promise<void>;onCreateShell:(workspaceID:string)=>Promise<void>;onOpenShell:(shell:SSHShell)=>void;onCollapse:()=>void}){
	const {t}=useTranslation()
	const workspace=workspaces.find(item=>item.id===workspaceID)||workspaces[0]
	const sftpHost=hosts.find(item=>item.id===sftpHostID)||hosts[0]
	const activeWorkspaceID=workspace?.id||''
	const visible=useDocumentVisible()
	const viewActive=active&&visible&&mode==='workspace'
	const [path,setPath]=useState('.')
	const [file,setFile]=useState<File|null>(null),[target,setTarget]=useState(''),[inputKey,setInputKey]=useState(0)
	const [notice,setNotice]=useState<WorkspaceNotice|null>(null),[dragging,setDragging]=useState(false)
	const [preview,setPreview]=useState<WorkspaceFilePreview|null>(null),[previewLoading,setPreviewLoading]=useState(''),[deleting,setDeleting]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<WorkspaceDeleteCandidate|null>(null)
	const [startingShell,setStartingShell]=useState(false)
	const {operation:transfer,key:transferKey,uploadVersion,upload:startUpload,download:startDownload,cancel}=useWorkspaceTransfer(activeWorkspaceID,viewActive)
	const uploading=transfer==='upload'
	const observedUploadVersion=useRef(uploadVersion)
	const previewRequest=useRef<AbortController|null>(null)
	const activeShells=shells.filter(shell=>shell.workspace_id===activeWorkspaceID&&sshShellActive(shell.status)).sort((left,right)=>left.started_at.localeCompare(right.started_at))

	const previewPath=preview?.path||''
	useEffect(()=>()=>previewRequest.current?.abort(),[activeWorkspaceID,path,previewPath,viewActive])
	const refreshPreview=useCallback(async()=>{
		if(!viewActive||!activeWorkspaceID||!previewPath||previewRequest.current)return
		const controller=new AbortController()
		previewRequest.current=controller
		try{const result=await api.previewWorkspaceFile(activeWorkspaceID,previewPath,controller.signal);if(!controller.signal.aborted)setPreview(result)}catch{/* keep the last successful preview; the listing still reports the error */}
		finally{if(previewRequest.current===controller)previewRequest.current=null}
	},[activeWorkspaceID,previewPath,viewActive])
	const {entries,loading,error,load}=useWorkspaceDirectory(activeWorkspaceID,path,viewActive,uploadVersion,refreshPreview)
	const synchronize=useCallback((showLoading=false)=>{void load(showLoading);void refreshPreview()},[load,refreshPreview])
	useEffect(()=>{
		if(observedUploadVersion.current===uploadVersion)return
		observedUploadVersion.current=uploadVersion
		setFile(null);setTarget('');setInputKey(value=>value+1)
	},[uploadVersion])

	const choose=(event:React.ChangeEvent<HTMLInputElement>)=>{
		const selected=event.target.files?.[0]||null
		setFile(selected);setTarget(selected?workspaceChildPath(path,selected.name):'');setNotice(null)
	}
	const upload=()=>{
		if(!workspace||!file||!target.trim()||transfer)return
		setNotice(null)
		startUpload([{type:'file',file,path:target.trim()}])
	}
	const chooseFolder=(event:React.ChangeEvent<HTMLInputElement>)=>{
		const files=Array.from(event.currentTarget.files||[])
		event.currentTarget.value=''
		if(!workspace||workspace.access!=='read_write'||transfer||!files.length)return
		setNotice(null)
		if(startUpload(workspaceUploadFiles(files,path))){setFile(null);setTarget('')}
	}
	const acceptsFiles=(event:React.DragEvent<HTMLElement>)=>workspace?.access==='read_write'&&Array.from(event.dataTransfer.types).includes('Files')
	const dragEnter=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();setDragging(true)}
	const dragOver=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();event.dataTransfer.dropEffect=uploading||transfer?'none':'copy'}
	const dragLeave=(event:React.DragEvent<HTMLElement>)=>{if(workspace?.access!=='read_write')return;event.preventDefault();event.stopPropagation();if(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget))return;setDragging(false)}
	const drop=(event:React.DragEvent<HTMLElement>)=>{
		if(!acceptsFiles(event))return
		event.preventDefault();event.stopPropagation();setDragging(false)
		if(transfer)return
		setNotice(null)
		if(startUpload(workspaceUploadDrop(event.dataTransfer,path))){setFile(null);setTarget('')}
	}
	const openEntry=useCallback(async(name:string,type:'file'|'directory')=>{
		previewRequest.current?.abort()
		const next=workspaceChildPath(path,name)
		if(type==='directory'){setPath(next);return}
		if(!activeWorkspaceID)return
		const controller=new AbortController()
		previewRequest.current=controller
		setPreviewLoading(next);setNotice(null)
		try{const result=await api.previewWorkspaceFile(activeWorkspaceID,next,controller.signal);if(!controller.signal.aborted)setPreview(result)}catch(err){if(!controller.signal.aborted)setNotice({kind:'error',text:errorText(err)})}finally{if(previewRequest.current===controller){previewRequest.current=null;setPreviewLoading('')}}
	},[activeWorkspaceID,path])
	const download=useCallback((relativePath:string,name:string,size=0)=>{
		if(!activeWorkspaceID||transfer)return
		setNotice(null)
		startDownload(relativePath,name,size)
	},[activeWorkspaceID,transfer,startDownload])
	const requestEntryRemoval=useCallback((name:string,type:'file'|'directory')=>{
		if(activeWorkspaceID)setDeleteCandidate({workspaceID:activeWorkspaceID,path:workspaceChildPath(path,name),type})
	},[activeWorkspaceID,path])
	const revealDirectory=useCallback(async(relativePath:string)=>{
		if(!activeWorkspaceID||!desktopRuntime)return
		setNotice(null)
		try{await invoke('open_workspace_directory',{workspaceId:activeWorkspaceID,relativePath})}
		catch(err){setNotice({kind:'error',text:errorText(err)})}
	},[activeWorkspaceID])
	const removeEntry=async()=>{
		if(!deleteCandidate)return
		const candidate=deleteCandidate
		setDeleting(candidate.path);setNotice(null)
		try{
			const result=await api.deleteWorkspaceEntry(candidate.workspaceID,candidate.path)
			if(candidate.workspaceID===workspace?.id&&preview?.path===candidate.path)setPreview(null)
			setNotice({kind:'success',text:t('workspace.deleted',{type:t(`workspace.${result.type}`,{defaultValue:result.type})})})
		}catch(err){setNotice({kind:'error',text:errorText(err)})}finally{setDeleting('');setDeleteCandidate(null)}
	}
	const savePreview=async(content:string)=>{
		if(!workspace||!preview)return
		const saved=await api.saveWorkspaceTextFile(workspace.id,preview.path,content)
		setPreview({...preview,content,binary:false,size:saved.size,sha256:saved.sha256})
		setNotice({kind:'success',text:t('workspace.saved',{path:saved.path})})
	}
	const up=()=>{if(path==='.')return;const parts=path.split('/');parts.pop();setPath(parts.join('/')||'.')}
	const createShell=async()=>{
		if(!workspace||startingShell)return
		setStartingShell(true)
		try{await onCreateShell(workspace.id)}finally{setStartingShell(false)}
	}

	if(mode==='sftp')return <SFTPBrowser key={sftpHost?.id||'no-host'} host={sftpHost} active={active} embedded hosts={hosts} onHostSelect={onSFTPHostChange} onWorkspaceMode={()=>onModeChange('workspace')} onCollapse={onCollapse}/>
	if(!workspace)return <aside className="workspace-browser-panel panel empty"><div className="panel-header"><FileBrowserTabs mode={mode} onChange={onModeChange}/><div className="workspace-panel-actions"><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></div><div className="workspace-empty"><FolderOpen size={23}/><span>{t('workspace.noConfigured')}</span></div></aside>
	return <>
		<aside className={`workspace-browser-panel panel ${dragging?'dragging':''}`} onDragEnter={dragEnter} onDragOver={dragOver} onDragLeave={dragLeave} onDrop={drop}>
			<div className="panel-header"><FileBrowserTabs mode={mode} onChange={onModeChange}/><div className="workspace-panel-actions"><button type="button" onClick={onCollapse} title={t('workspace.collapsePanel')} aria-label={t('workspace.collapsePanel')}><PanelLeftClose size={14}/></button></div></div>
			<div className="workspace-summary"><div className="chat-workspace-head"><div className="chat-workspace-selector"><AppSelect className="workspace-switch-select" value={workspace.id} disabled={workspaces.length<2||disabled||switching} ariaLabel={t('workspace.switchWorkspace')} onChange={onSelect} options={workspaces.map(item=>({value:item.id,label:item.id}))}/>{(switching||bound)&&<small>{switching?t('workspace.switching'):t('workspace.boundToConversation')}</small>}</div><div className="chat-workspace-head-actions"><em className={workspace.access}>{workspace.access==='read_write'?t('workspace.readWrite'):t('workspace.readOnly')}</em><button type="button" disabled={!workspace.shell||startingShell} onClick={()=>void createShell()} title={t('workspace.newTerminal')} aria-label={t('workspace.newTerminal')}>{startingShell?<LoaderCircle className="spin" size={14}/>:<TerminalSquare size={14}/>}</button></div></div>{activeShells.length>0&&<div className="workspace-shell-sessions">{activeShells.map(shell=><button type="button" onClick={()=>onOpenShell(shell)} title={shell.id} key={shell.id}><i className={shell.status}/><b>{t(shell.surface==='workspace_agent'?'workspace.agent':'workspace.operator')}</b><code>{shell.cwd||'.'}</code></button>)}</div>}</div>
			<div className="workspace-path-row"><button onClick={up} disabled={path==='.'} title={t('workspace.parent')}>‹</button><code title={path}>{path}</code>{workspace.access==='read_write'&&<><label className={transfer?'disabled':''} title={t('workspace.uploadFile')}><UploadCloud size={14}/><input key={inputKey} type="file" disabled={!!transfer} aria-label={t('workspace.uploadFile')} onChange={choose}/></label><label className={transfer?'disabled':''} title={t('workspace.uploadFolder')}><FolderUp size={14}/><input type="file" multiple ref={element=>{element?.setAttribute('webkitdirectory','')}} disabled={!!transfer} aria-label={t('workspace.uploadFolder')} onChange={chooseFolder}/></label></>}<button onClick={()=>synchronize(true)} title={t('workspace.refreshFiles')}><RefreshCw size={12}/></button></div>
			{file&&<div className="chat-upload-row"><input value={target} disabled={uploading} onChange={event=>setTarget(event.target.value)} aria-label={t('workspace.relativePath')}/><button onClick={upload} disabled={uploading||!target.trim()}>{uploading?'...':t('common.upload')}</button><button onClick={()=>{if(uploading){cancel();return}setFile(null);setTarget('');setInputKey(value=>value+1)}} title={t('workspace.cancelUpload')}><X size={11}/></button></div>}
			<FileTransferProgress transferKey={transferKey} active={active&&mode==='workspace'}/>
			<WorkspaceFileList entries={entries} active={viewActive} loading={loading} error={error} path={path} writable={workspace.access==='read_write'} transferring={!!transfer} opening={previewLoading} deleting={deleting} onOpen={openEntry} onDownload={download} onReveal={revealDirectory} onDelete={requestEntryRemoval}/>
			{notice&&<div className={`chat-workspace-notice ${notice.kind}`}>{notice.text}</div>}
			{dragging&&<div className="workspace-drop-overlay"><UploadCloud size={27}/><b>{t('workspace.dropEntriesHere')}</b><span>{path}</span></div>}
		</aside>
		{preview&&<TextFileEditor path={preview.path} meta={`${formatFileSize(preview.size)} · SHA-256 ${preview.sha256}${preview.truncated?` · ${t('common.truncated')}`:''}`} content={preview.content||''} binary={preview.binary} editable={workspace.access==='read_write'&&!preview.truncated} onClose={()=>setPreview(null)} onSave={savePreview} onDownload={()=>void download(preview.path,preview.path.split('/').at(-1)||'download',preview.size)}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('workspace.deleteTitle',{path:`${deleteCandidate.workspaceID}:${deleteCandidate.path}`})} busy={deleting===deleteCandidate.path} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void removeEntry()}/>}
	</>
})
