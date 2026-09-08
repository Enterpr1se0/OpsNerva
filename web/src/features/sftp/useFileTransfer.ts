import { createContext, useCallback, useContext, useMemo, useSyncExternalStore } from 'react'
import { useDocumentVisible } from '../../lib/hooks'
import type { SFTPFileEntry } from '../../types'
import type { FileTransferManager, WorkspaceTransferSource } from './types'
import { sftpTransferKey, workspaceTransferKey } from './utils'

export const FileTransferContext=createContext<FileTransferManager|null>(null)

export function useFileTransferManager(){
	const manager=useContext(FileTransferContext)
	if(!manager)throw new Error('FileTransferProvider is missing')
	return manager
}

function useFileTransferState(manager:FileTransferManager,key:string,active:boolean){
	const visible=useDocumentVisible()
	const subscribe=useCallback((listener:()=>void)=>active&&visible?manager.store.subscribeState(key,listener):()=>{},[active,visible,key,manager])
	const snapshot=useCallback(()=>manager.store.state(key),[key,manager])
	return useSyncExternalStore(subscribe,snapshot,snapshot)
}

export function useSFTPTransfer(hostID:string,active=true){
	const manager=useFileTransferManager()
	const key=sftpTransferKey(hostID)
	const state=useFileTransferState(manager,key,active)
	const actions=useMemo(()=>({
		upload:(directory:string,files:File[])=>manager.uploadSFTP(hostID,directory,files),
		download:(entry:SFTPFileEntry)=>manager.downloadSFTP(hostID,entry),
		cancel:()=>manager.cancel(key),
	}),[manager,hostID,key])
	return {...state,...actions,key}
}

export function useWorkspaceTransfer(workspaceID:string,active=true){
	const manager=useFileTransferManager()
	const key=workspaceTransferKey(workspaceID)
	const state=useFileTransferState(manager,key,active)
	const actions=useMemo(()=>({
		upload:(source:WorkspaceTransferSource)=>manager.uploadWorkspace(workspaceID,source),
		download:(path:string,name:string,size:number)=>manager.downloadWorkspace(workspaceID,path,name,size),
		cancel:()=>manager.cancel(key),
	}),[manager,workspaceID,key])
	return {...state,...actions,key}
}
