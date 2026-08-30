import { memo, useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { invoke } from '@tauri-apps/api/core'
import { Download, FileText, FolderOpen, FolderOutput, LoaderCircle, PanelLeftClose, RefreshCw, TerminalSquare, Trash2, UploadCloud, X } from 'lucide-react'
import { api, workspaceFileEventsURL } from '../../../api/api'
import { AppSelect } from '../../../components/Controls'
import { DestructiveConfirmDialog } from '../../../components/DestructiveConfirmDialog'
import { TextFileEditor } from '../../../components/TextFileEditor'
import { FileBrowserTabs, FileTransferProgress, SFTPBrowser, useWorkspaceTransfer, type FileBrowserMode } from '../../../features/sftp'
import { sshShellActive } from '../../../features/ssh'
import { desktopRuntime, errorText, formatFileSize } from '../../../lib/utils'
import type { Host, SSHShell, WorkspaceCapability, WorkspaceFilePreview } from '../../../types'
import { workspaceChildPath } from '../utils'
import type { WorkspaceDeleteCandidate, WorkspaceNotice } from '../types'

export const ChatWorkspacePanel=memo(function ChatWorkspacePanel({active,mode,onModeChange,workspaces,workspaceID,hosts,sftpHostID,onSFTPHostChange,shells,switching,disabled,bound,onSelect,onCreateShell,onOpenShell,onCollapse}:{active:boolean;mode:FileBrowserMode;onModeChange:(mode:FileBrowserMode)=>void;workspaces:WorkspaceCapability[];workspaceID:string;hosts:Host[];sftpHostID:string;onSFTPHostChange:(id:string)=>void;shells:SSHShell[];switching:boolean;disabled:boolean;bound:boolean;onSelect:(id:string)=>void|Promise<void>;onCreateShell:(workspaceID:string)=>Promise<void>;onOpenShell:(shell:SSHShell)=>void;onCollapse:()=>void}){
	const {t}=useTranslation()
	const workspace=workspaces.find(item=>item.id===workspaceID)||workspaces[0]
	const sftpHost=hosts.find(item=>item.id===sftpHostID)||hosts[0]
	const activeWorkspaceID=workspace?.id||''
	const [path,setPath]=useState('.')
	const [entries,setEntries]=useState<{name:string;type:'file'|'directory';size?:number}[]>([])
	const [loading,setLoading]=useState(false),[error,setError]=useState('')
	const [file,setFile]=useState<File|null>(null),[target,setTarget]=useState(''),[inputKey,setInputKey]=useState(0)
	const [notice,setNotice]=useState<WorkspaceNotice|null>(null),[dragging,setDragging]=useState(false)
	const [preview,setPreview]=useState<WorkspaceFilePreview|null>(null),[previewLoading,setPreviewLoading]=useState(''),[deleting,setDeleting]=useState('')
	const [deleteCandidate,setDeleteCandidate]=useState<WorkspaceDeleteCandidate|null>(null)
	const [startingShell,setStartingShell]=useState(false)
	const {active:transfer,uploadVersion,upload:startUpload,download:startDownload,cancel}=useWorkspaceTransfer(activeWorkspaceID,active&&mode==='workspace')
	const uploading=transfer?.operation==='upload'
	const observedUploadVersion=useRef(uploadVersion)
	const loadRequestRef=useRef(0),previewPathRef=useRef('')
	const activeShells=shells.filter(shell=>shell.workspace_id===activeWorkspaceID&&sshShellActive(shell.status)).sort((left,right)=>left.started_at.localeCompare(right.started_at))

	const load=useCallback(async(showLoading=true)=>{
		if(!activeWorkspaceID)return
		const requestID=++loadRequestRef.current
		if(showLoading)setLoading(true)
		try{
			const result=await api.workspaceFiles(activeWorkspaceID,path)
			if(loadRequestRef.current!==requestID)return
			setEntries(result.entries||[]);setError('')
		}catch(err){
			if(loadRequestRef.current!==requestID)return
			setEntries([]);setError(errorText(err))
		}finally{
			if(loadRequestRef.current===requestID)setLoading(false)
		}
	},[activeWorkspaceID,path])
	const previewPath=preview?.path||''
	useEffect(()=>{previewPathRef.current=previewPath},[previewPath])
	const refreshPreview=useCallback(async()=>{
		if(!activeWorkspaceID||!previewPath)return
		try{const result=await api.previewWorkspaceFile(activeWorkspaceID,previewPath);if(previewPathRef.current===previewPath)setPreview(result)}catch{/* keep the last successful preview; the listing still reports the error */}
	},[activeWorkspaceID,previewPath])
	const synchronize=useCallback((showLoading=false)=>{void load(showLoading);void refreshPreview()},[load,refreshPreview])
	useEffect(()=>{
		if(observedUploadVersion.current===uploadVersion)return
		observedUploadVersion.current=uploadVersion
		setFile(null);setTarget('');setInputKey(value=>value+1)
		if(active&&mode==='workspace')synchronize(false)
	},[active,mode,synchronize,uploadVersion])

	useEffect(()=>{if(active&&mode==='workspace')void load()},[active,load,mode])
	useEffect(()=>{
		if(!active||mode!=='workspace'||!activeWorkspaceID)return
		const source=new EventSource(workspaceFileEventsURL(activeWorkspaceID,path))
		const changed=()=>synchronize(false)
		source.addEventListener('workspace-change',changed)
		source.onopen=changed
		return()=>{source.removeEventListener('workspace-change',changed);source.close()}
	},[active,activeWorkspaceID,mode,path,synchronize])

	const choose=(event:React.ChangeEvent<HTMLInputElement>)=>{
		const selected=event.target.files?.[0]||null
		setFile(selected);setTarget(selected?workspaceChildPath(path,selected.name):'');setNotice(null)
	}
	const upload=()=>{
		if(!workspace||!file||!target.trim()||transfer)return
		setNotice(null)
		startUpload([{file,path:target.trim()}])
	}
	const uploadDropped=(files:File[])=>{
		if(!workspace||workspace.access!=='read_write'||uploading||transfer||!files.length)return
		setNotice(null)
		startUpload(files.map(dropped=>({file:dropped,path:workspaceChildPath(path,dropped.name)})))
	}
	const acceptsFiles=(event:React.DragEvent<HTMLElement>)=>workspace?.access==='read_write'&&Array.from(event.dataTransfer.types).includes('Files')
	const dragEnter=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();setDragging(true)}
	const dragOver=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();event.dataTransfer.dropEffect=uploading||transfer?'none':'copy'}
	const dragLeave=(event:React.DragEvent<HTMLElement>)=>{if(workspace?.access!=='read_write')return;event.preventDefault();event.stopPropagation();if(event.relatedTarget instanceof Node&&event.currentTarget.contains(event.relatedTarget))return;setDragging(false)}
	const drop=(event:React.DragEvent<HTMLElement>)=>{if(!acceptsFiles(event))return;event.preventDefault();event.stopPropagation();setDragging(false);if(!uploading&&!transfer)void uploadDropped(Array.from(event.dataTransfer.files))}
	const openEntry=async(name:string,type:'file'|'directory')=>{
		const next=workspaceChildPath(path,name)
		if(type==='directory'){setPath(next);return}
		if(!workspace)return
		setPreviewLoading(next);setNotice(null)
		try{setPreview(await api.previewWorkspaceFile(workspace.id,next))}catch(err){setNotice({kind:'error',text:errorText(err)})}finally{setPreviewLoading('')}
	}
	const download=(relativePath:string,name:string,size=0)=>{
		if(!workspace||transfer)return
		setNotice(null)
		startDownload(relativePath,name,size)
	}
	const requestEntryRemoval=(name:string,type:'file'|'directory')=>{
		if(workspace)setDeleteCandidate({workspaceID:workspace.id,path:workspaceChildPath(path,name),type})
	}
	const revealDirectory=async(relativePath:string)=>{
		if(!workspace||!desktopRuntime)return
		setNotice(null)
		try{await invoke('open_workspace_directory',{workspaceId:workspace.id,relativePath})}
		catch(err){setNotice({kind:'error',text:errorText(err)})}
	}
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
			<div className="workspace-path-row"><button onClick={up} disabled={path==='.'} title={t('workspace.parent')}>‹</button><code title={path}>{path}</code>{workspace.access==='read_write'&&<label className={transfer?'disabled':''} title={t('workspace.uploadFile')}><UploadCloud size={14}/><input key={inputKey} type="file" disabled={!!transfer} onChange={choose}/></label>}<button onClick={()=>synchronize(true)} title={t('workspace.refreshFiles')}><RefreshCw size={12}/></button></div>
			{file&&<div className="chat-upload-row"><input value={target} disabled={uploading} onChange={event=>setTarget(event.target.value)} aria-label={t('workspace.relativePath')}/><button onClick={upload} disabled={uploading||!target.trim()}>{uploading?'...':t('common.upload')}</button><button onClick={()=>{if(uploading){cancel();return}setFile(null);setTarget('');setInputKey(value=>value+1)}} title={t('workspace.cancelUpload')}><X size={11}/></button></div>}
			{transfer&&<FileTransferProgress transfer={transfer} onCancel={cancel}/>}
			<div className="workspace-file-list">{loading?<span className="workspace-files-state"><LoaderCircle className="spin" size={13}/>{t('common.loading')}</span>:error?<span className="workspace-files-state error">{error}</span>:entries.length?entries.map(entry=>{const fullPath=workspaceChildPath(path,entry.name);return <div className="workspace-file-row" key={`${entry.type}:${entry.name}`}><button className="workspace-file-open" onClick={()=>void openEntry(entry.name,entry.type)} title={entry.type==='file'?t('workspace.previewFile'):t('workspace.openDirectory')}>{previewLoading===fullPath?<LoaderCircle className="spin" size={13}/>:entry.type==='directory'?<FolderOpen size={13}/>:<FileText size={13}/>}<span>{entry.name}</span>{entry.type==='file'&&<small>{formatFileSize(entry.size??0)}</small>}</button>{(entry.type==='file'||desktopRuntime&&entry.type==='directory'||workspace.access==='read_write')&&<div className="workspace-file-actions">{entry.type==='file'&&<button className="workspace-file-download" disabled={!!transfer} onClick={()=>void download(fullPath,entry.name,entry.size??0)} title={t('common.download')}><Download size={12}/></button>}{desktopRuntime&&entry.type==='directory'&&<button className="workspace-file-reveal" onClick={()=>void revealDirectory(fullPath)} title={t('workspace.revealDirectory')}><FolderOutput size={12}/></button>}{workspace.access==='read_write'&&<button className="workspace-file-delete" onClick={()=>requestEntryRemoval(entry.name,entry.type)} disabled={deleting===fullPath||!!transfer} title={t('workspace.deleteEntry',{type:t(`workspace.${entry.type}`)})}><Trash2 size={12}/></button>}</div>}</div>}):<span className="workspace-files-state">{t('workspace.emptyDirectory')}</span>}</div>
			{notice&&<div className={`chat-workspace-notice ${notice.kind}`}>{notice.text}</div>}
			{dragging&&<div className="workspace-drop-overlay"><UploadCloud size={27}/><b>{t('workspace.dropFilesHere')}</b><span>{path}</span></div>}
		</aside>
		{preview&&<TextFileEditor path={preview.path} meta={`${formatFileSize(preview.size)} · SHA-256 ${preview.sha256}${preview.truncated?` · ${t('common.truncated')}`:''}`} content={preview.content||''} binary={preview.binary} editable={workspace.access==='read_write'&&!preview.truncated} onClose={()=>setPreview(null)} onSave={savePreview} onDownload={()=>void download(preview.path,preview.path.split('/').at(-1)||'download',preview.size)}/>}
		{deleteCandidate&&<DestructiveConfirmDialog title={t('workspace.deleteTitle',{path:`${deleteCandidate.workspaceID}:${deleteCandidate.path}`})} busy={deleting===deleteCandidate.path} onCancel={()=>setDeleteCandidate(null)} onConfirm={()=>void removeEntry()}/>}
	</>
})