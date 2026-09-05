import { createContext, useCallback, useContext, useSyncExternalStore } from 'react'
import type { SFTPFileEntry } from '../../types'
import type { FileTransferManager, WorkspaceTransferItem } from './types'
import { emptyFileTransferRecord, sftpTransferKey, workspaceTransferKey } from './utils'

export const FileTransferContext=createContext<FileTransferManager|null>(null)

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
