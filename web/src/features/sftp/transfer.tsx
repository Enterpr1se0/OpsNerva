import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, useSyncExternalStore, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { api, downloadFile, sftpDownloadURL, workspaceDownloadURL } from '../../api/api'
import { useNotifier } from '../../lib/notifications'
import { errorText } from '../../lib/utils'
import { SFTPOverwriteDialog } from './components/SFTPOverwriteDialog'
import type { SFTPFileEntry } from '../../types'
import type { ActiveFileTransfer, FileTransferManager, FileTransferRecord, SFTPOverwriteCandidate, WorkspaceTransferItem } from './types'
import { emptyFileTransferRecord, isAbortError, remoteChildPath, sftpTransferKey, workspaceTransferKey } from './utils'

export const FileTransferContext=createContext<FileTransferManager|null>(null)

export function FileTransferProvider({children}:{children:ReactNode}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [,refreshDialogs]=useState(0)
	const recordsRef=useRef<ReadonlyMap<string,FileTransferRecord>>(new Map())
	const subscribersRef=useRef(new Map<string,Set<()=>void>>())
	const record=useCallback((key:string)=>recordsRef.current.get(key)||emptyFileTransferRecord,[])
	const subscribe=useCallback((key:string,listener:()=>void)=>{
		let subscribers=subscribersRef.current.get(key)
		if(!subscribers){subscribers=new Set();subscribersRef.current.set(key,subscribers)}
		subscribers.add(listener)
		return()=>{subscribers!.delete(listener);if(!subscribers!.size)subscribersRef.current.delete(key)}
	},[])
	const controllers=useRef(new Map<string,AbortController>())
	const updateRecord=useCallback((key:string,update:(current:FileTransferRecord)=>FileTransferRecord)=>{
		const next=new Map(recordsRef.current)
		const current=next.get(key)||emptyFileTransferRecord
		const updated=update(current)
		next.set(key,updated)
		recordsRef.current=next
		if(current.conflict!==updated.conflict)refreshDialogs(version=>version+1)
		for(const listener of subscribersRef.current.get(key)||[])listener()
	},[])
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
		const conflict=recordsRef.current.get(sftpTransferKey(hostID))?.conflict
		if(conflict)void runSFTPUpload(hostID,conflict.directory,[{file:conflict.file,path:conflict.path}],true)
	},[runSFTPUpload])
	const dismissConflict=useCallback((hostID:string)=>updateRecord(sftpTransferKey(hostID),current=>({...current,conflict:null})),[updateRecord])
	const uploadWorkspace=useCallback((workspaceID:string,items:WorkspaceTransferItem[])=>{
		const key=workspaceTransferKey(workspaceID)
		if(!workspaceID||!items.length||controllers.current.has(key))return false
		void (async()=>{
			const total=items.reduce((sum,item)=>sum+item.file.size,0)
			const controller=begin(key,{operation:'upload',name:items[0].file.name,loaded:0,total,index:1,count:items.length})
			if(!controller)return
			let completedBytes=0
			let uploaded=0
			const failures:Array<{name:string;message:string}>=[]
			for(let index=0;index<items.length;index++){
				const item=items[index]
				updateRecord(key,current=>({...current,active:{operation:'upload',name:item.file.name,loaded:completedBytes,total,index:index+1,count:items.length}}))
				try{
					await api.uploadWorkspaceFile(workspaceID,item.file,item.path,{signal:controller.signal,onProgress:progress=>updateRecord(key,current=>({...current,active:current.active?{...current.active,loaded:completedBytes+progress.loaded,total}:null}))})
					uploaded+=1;completedBytes+=item.file.size
				}catch(err){
					if(isAbortError(err))break
					failures.push({name:item.file.name,message:errorText(err)})
				}
			}
			finish(key,controller,uploaded>0)
			if(controller.signal.aborted)return
			if(failures.length===1&&items.length===1)notify(failures[0].message,'error')
			else if(failures.length)notify(t('workspace.uploadPartial',{uploaded,failed:failures.length,message:`${failures[0].name}: ${failures[0].message}`}),'error')
			else if(items.length===1)notify(t('workspace.uploaded',{path:items[0].path}))
			else notify(t('workspace.uploadedFiles',{count:uploaded}))
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
	useEffect(()=>()=>{for(const controller of controllers.current.values())controller.abort();controllers.current.clear();subscribersRef.current.clear()},[])
	const value=useMemo<FileTransferManager>(()=>({record,subscribe,uploadSFTP,downloadSFTP,uploadWorkspace,downloadWorkspace,cancel}),[cancel,downloadSFTP,downloadWorkspace,record,subscribe,uploadSFTP,uploadWorkspace])
	return <FileTransferContext.Provider value={value}><>{children}{[...recordsRef.current].map(([key,record])=>record.conflict&&<SFTPOverwriteDialog key={key} path={record.conflict.path} busy={!!record.active} onCancel={()=>dismissConflict(key.slice(5))} onConfirm={()=>overwrite(key.slice(5))}/>)}</></FileTransferContext.Provider>
}

export function useFileTransferRecord(manager:FileTransferManager|null,key:string,enabled:boolean){
	const subscribe=useCallback((listener:()=>void)=>enabled&&key&&manager?manager.subscribe(key,listener):()=>{},[enabled,key,manager])
	const snapshot=useCallback(()=>enabled&&key&&manager?manager.record(key):emptyFileTransferRecord,[enabled,key,manager])
	return useSyncExternalStore(subscribe,snapshot,snapshot)
}

export function useSFTPTransfer(hostID:string,active=true){
	const manager=useContext(FileTransferContext)
	const key=sftpTransferKey(hostID)
	const record=useFileTransferRecord(manager,key,active)
	if(!manager)throw new Error('FileTransferProvider is missing')
	return{...record,upload:(directory:string,files:File[])=>manager.uploadSFTP(hostID,directory,files),download:(entry:SFTPFileEntry)=>manager.downloadSFTP(hostID,entry),cancel:()=>manager.cancel(key)}
}

export function useWorkspaceTransfer(workspaceID:string,active=true){
	const manager=useContext(FileTransferContext)
	const key=workspaceTransferKey(workspaceID)
	const record=useFileTransferRecord(manager,key,active)
	if(!manager)throw new Error('FileTransferProvider is missing')
	return{...record,upload:(items:WorkspaceTransferItem[])=>manager.uploadWorkspace(workspaceID,items),download:(path:string,name:string,size:number)=>manager.downloadWorkspace(workspaceID,path,name,size),cancel:()=>manager.cancel(key)}
}
