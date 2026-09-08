import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { api, downloadFile, sftpDownloadURL, workspaceDownloadURL } from '../../api/api'
import { useNotifier } from '../../lib/notifications'
import { errorText } from '../../lib/utils'
import { SFTPOverwriteDialog } from './components/SFTPOverwriteDialog'
import type { SFTPFileEntry } from '../../types'
import type { ActiveFileTransfer, FileTransferManager, FileTransferRecord, SFTPOverwriteCandidate, WorkspaceTransferSource } from './types'
import { isAbortError, remoteChildPath, sftpTransferKey, workspaceTransferKey } from './utils'
import { FileTransferStore } from './transferStore'

import { FileTransferContext } from './useFileTransfer'
import { useSFTPDeletionManager } from './useSFTPDeletion'

export function FileTransferProvider({children}:{children:ReactNode}){
	const deletions=useSFTPDeletionManager()
	const {t}=useTranslation()
	const notify=useNotifier()
	const [conflicts,setConflicts]=useState<ReadonlyMap<string,FileTransferRecord>>(()=>new Map())
	const [store]=useState(()=>new FileTransferStore())
	const controllers=useRef(new Map<string,AbortController>())
	const updateRecord=useCallback((key:string,update:(current:FileTransferRecord)=>FileTransferRecord)=>{
		const current=store.record(key)
		const updated=update(current)
		store.set(key,updated)
		if(current.conflict!==updated.conflict||updated.conflict&&!!current.active!==!!updated.active)setConflicts(current=>{
			const next=new Map(current)
			if(updated.conflict)next.set(key,updated);else next.delete(key)
			return next
		})
	},[store])
	const begin=useCallback((key:string,transfer:ActiveFileTransfer)=>{
		if(controllers.current.has(key))return null
		const controller=new AbortController()
		controllers.current.set(key,controller)
		updateRecord(key,current=>({...current,active:transfer,conflict:null}))
		return controller
	},[updateRecord])
	const finish=useCallback((key:string,controller:AbortController,uploaded=false)=>{
		if(controllers.current.get(key)!==controller)return
		controllers.current.delete(key)
		updateRecord(key,current=>({...current,active:null,uploadVersion:current.uploadVersion+(uploaded?1:0)}))
	},[updateRecord])
	const runSFTPUpload=useCallback(async(hostID:string,directory:string,items:Array<{file:File;path:string}>,overwrite:boolean)=>{
		const key=sftpTransferKey(hostID)
		const total=items.reduce((sum,item)=>sum+item.file.size,0)
		const controller=begin(key,{operation:'upload',name:items[0]?.file.name||'',loaded:0,total,index:1,count:items.length})
		if(!controller)return
		let completedBytes=0
		let uploaded=0
		let failure=''
		let conflict:SFTPOverwriteCandidate|null=null
		for(let index=0;index<items.length;index++){
			const {file,path}=items[index]
			updateRecord(key,current=>({...current,active:{operation:'upload',name:file.name,loaded:completedBytes,total,index:index+1,count:items.length}}))
			try{
				await api.uploadSFTPFile(hostID,path,file,overwrite,{signal:controller.signal,onProgress:progress=>updateRecord(key,current=>({...current,active:current.active?{...current.active,loaded:completedBytes+progress.loaded,total}:null}))})
				uploaded+=1;completedBytes+=file.size
			}catch(err){
				if(!isAbortError(err)){
					const message=errorText(err)
					if(items.length===1&&!overwrite&&message.includes('already exists'))conflict={file,path,directory}
					else failure=message
				}
				break
			}
		}
		if(controllers.current.get(key)===controller&&conflict)updateRecord(key,current=>({...current,conflict}))
		finish(key,controller,uploaded>0)
		if(failure)notify(failure,'error')
		else if(!controller.signal.aborted&&!conflict&&uploaded===items.length)notify(t('sshWorkspace.uploaded',{count:uploaded}))
	},[begin,finish,notify,t,updateRecord])
	const uploadSFTP=useCallback((hostID:string,directory:string,files:File[])=>{
		if(!files.length)return
		void runSFTPUpload(hostID,directory,files.map(file=>({file,path:remoteChildPath(directory,file.name)})),false)
	},[runSFTPUpload])
	const downloadSFTP=useCallback(async(hostID:string,entry:SFTPFileEntry)=>{
		const key=sftpTransferKey(hostID)
		const controller=begin(key,{operation:'download',name:entry.name,loaded:0,total:entry.size||0})
		if(!controller)return
		try{
			await downloadFile(sftpDownloadURL(hostID,entry.path),entry.name,{signal:controller.signal,totalBytes:entry.size||0,onProgress:progress=>updateRecord(key,current=>({...current,active:current.active?{...current.active,...progress}:null}))})
		}catch(err){if(!isAbortError(err))notify(errorText(err),'error')}
		finally{finish(key,controller)}
	},[begin,finish,notify,updateRecord])
	const overwrite=useCallback((hostID:string)=>{
		const conflict=store.record(sftpTransferKey(hostID)).conflict
		if(conflict)void runSFTPUpload(hostID,conflict.directory,[{file:conflict.file,path:conflict.path}],true)
	},[runSFTPUpload,store])
	const dismissConflict=useCallback((hostID:string)=>updateRecord(sftpTransferKey(hostID),current=>({...current,conflict:null})),[updateRecord])
	const uploadWorkspace=useCallback((workspaceID:string,source:WorkspaceTransferSource)=>{
		const key=workspaceTransferKey(workspaceID)
		if(!workspaceID||Array.isArray(source)&&!source.length||controllers.current.has(key))return false
		const controller=begin(key,{operation:'upload',name:t('workspace.readingUpload'),loaded:0,total:0})
		if(!controller)return false
		void (async()=>{
			let completedBytes=0
			let uploaded=0
			let failed=0
			let firstFailure=''
			const failedDirectories=new Map<string,string>()
			try{
				const items=typeof source==='function'?await source(controller.signal):source
				const total=items.reduce((sum,item)=>sum+(item.type==='file'?item.file.size:0),0)
				for(let index=0;index<items.length;index++){
					controller.signal.throwIfAborted()
					const item=items[index]
					try{
						const parent=item.path.split('/').slice(0,-1).join('/')
						const parentError=failedDirectories.get(parent)
						if(parentError)throw new Error(parentError)
						updateRecord(key,current=>({...current,active:{operation:'upload',name:item.path,loaded:completedBytes,total,index:index+1,count:items.length}}))
						if(item.type==='directory'){
							await api.createWorkspaceDirectory(workspaceID,item.path,controller.signal)
						}else{
							await api.uploadWorkspaceFile(workspaceID,item.file,item.path,{signal:controller.signal,onProgress:progress=>updateRecord(key,current=>({...current,active:current.active?{...current.active,loaded:completedBytes+progress.loaded,total}:null}))})
							completedBytes+=item.file.size
						}
						uploaded+=1
					}catch(err){
						if(isAbortError(err))throw err
						const message=errorText(err)
						if(item.type==='directory')failedDirectories.set(item.path,message)
						failed+=1
						if(!firstFailure)firstFailure=`${item.path}: ${message}`
					}
				}
				if(controller.signal.aborted)return
				if(failed)notify(t('workspace.uploadPartial',{uploaded,failed,message:firstFailure}),'error')
				else if(items.length===1)notify(t('workspace.uploaded',{path:items[0].path}))
				else if(items.length)notify(t('workspace.uploadedEntries',{count:uploaded}))
			}catch(err){if(!isAbortError(err))notify(errorText(err),'error')}
			finally{finish(key,controller,uploaded>0)}
		})()
		return true
	},[begin,finish,notify,t,updateRecord])
	const downloadWorkspace=useCallback(async(workspaceID:string,path:string,name:string,size:number)=>{
		const key=workspaceTransferKey(workspaceID)
		const controller=begin(key,{operation:'download',name,loaded:0,total:size})
		if(!controller)return
		try{
			await downloadFile(workspaceDownloadURL(workspaceID,path),name,{signal:controller.signal,totalBytes:size,onProgress:progress=>updateRecord(key,current=>({...current,active:current.active?{...current.active,...progress}:null}))})
		}catch(err){if(!isAbortError(err))notify(errorText(err),'error')}
		finally{finish(key,controller)}
	},[begin,finish,notify,updateRecord])
	const cancel=useCallback((key:string)=>controllers.current.get(key)?.abort(),[])
	useEffect(()=>()=>{for(const controller of controllers.current.values())controller.abort();controllers.current.clear();store.dispose()},[store])
	const value=useMemo<FileTransferManager>(()=>({deletions,store,uploadSFTP,downloadSFTP,uploadWorkspace,downloadWorkspace,cancel}),[deletions,store,cancel,downloadSFTP,downloadWorkspace,uploadSFTP,uploadWorkspace])
	return <FileTransferContext.Provider value={value}><>{children}{[...conflicts].map(([key,record])=>record.conflict&&<SFTPOverwriteDialog key={key} path={record.conflict.path} busy={!!record.active} onCancel={()=>dismissConflict(key.slice(5))} onConfirm={()=>overwrite(key.slice(5))}/>)}</></FileTransferContext.Provider>
}
