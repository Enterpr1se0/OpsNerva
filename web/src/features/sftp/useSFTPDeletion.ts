import { useCallback, useContext, useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { api } from '../../api/api'
import { subscribeApplicationEvents } from '../../api/appEvents'
import type { SFTPDeletion, SFTPFileEntry } from '../../types'
import { SFTPDeletionStore, isSFTPDeletionActive } from './deletionStore'
import { FileTransferContext } from './useFileTransfer'
import { useTranslation } from 'react-i18next'

export function useSFTPDeletionManager(){
	const {t}=useTranslation()
	const [store]=useState(()=>new SFTPDeletionStore())
	useEffect(()=>subscribeApplicationEvents<SFTPDeletion|SFTPDeletion[]>('sftp_deletions',event=>{
		if(event.type!=='event'||!event.data)return
		if(event.mode==='snapshot')store.snapshot(event.data as SFTPDeletion[])
		else store.update(event.data as SFTPDeletion)
	}),[store])
	return useMemo(()=>({
		store,
		async start(hostID:string,entry:SFTPFileEntry){
			if(store.starting.has(hostID)||isSFTPDeletionActive(store.record(hostID)))throw new Error(t('sshWorkspace.deletionBusy'))
			store.starting.add(hostID)
			try{store.update(await api.deleteSFTPEntry(hostID,entry.path,entry.type==='directory'))}
			finally{store.starting.delete(hostID)}
		},
		async cancel(id:string){store.update(await api.cancelSFTPDeletion(id))},
	}),[store,t])
}

export type SFTPDeletionManager=ReturnType<typeof useSFTPDeletionManager>

export function useSFTPDeletion(hostID:string,enabled:boolean){
	const transfers=useContext(FileTransferContext)
	if(!transfers)throw new Error('FileTransferProvider is missing')
	const manager=transfers.deletions
	const subscribe=useCallback((listener:()=>void)=>enabled?manager.store.subscribe(hostID,listener):()=>{},[enabled,hostID,manager])
	const snapshot=useCallback(()=>enabled&&isSFTPDeletionActive(manager.store.record(hostID)),[enabled,hostID,manager])
	// Only active/terminal transitions rerender the file list, not every progress update.
	const running=useSyncExternalStore(subscribe,snapshot,snapshot)
	return {manager,running}
}
