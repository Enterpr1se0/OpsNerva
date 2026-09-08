import { useCallback, useState, useSyncExternalStore } from 'react'
import { useTranslation } from 'react-i18next'
import { Trash2, X } from 'lucide-react'
import { useNotifier } from '../../../lib/notifications'
import { errorText } from '../../../lib/utils'
import { isSFTPDeletionActive } from '../deletionStore'
import type { SFTPDeletionManager } from '../useSFTPDeletion'

export function SFTPDeletionStatus({hostID,manager,active}:{hostID:string;manager:SFTPDeletionManager;active:boolean}){
	const {t}=useTranslation()
	const notify=useNotifier()
	const [dismissed,setDismissed]=useState('')
	const [cancelling,setCancelling]=useState(false)
	const subscribe=useCallback((listener:()=>void)=>active?manager.store.subscribe(hostID,listener):()=>{},[active,hostID,manager])
	const snapshot=useCallback(()=>active?manager.store.record(hostID):undefined,[active,hostID,manager])
	const job=useSyncExternalStore(subscribe,snapshot,snapshot)
	if(!job||job.id===dismissed)return null
	const running=isSFTPDeletionActive(job)
	const cancel=async()=>{
		setCancelling(true)
		try{await manager.cancel(job.id)}catch(err){notify(errorText(err),'error')}
		finally{setCancelling(false)}
	}
	return <section className={`sftp-deletion-status ${job.status}`} aria-label={t('common.delete')}>
		<div><Trash2 size={13}/><b title={job.path}>{job.path}</b><span>{t(`statusLabels.${job.status}`)}</span>{running?<button type="button" disabled={cancelling||job.status==='stopping'} onClick={()=>void cancel()}>{t('common.cancel')}</button>:<button type="button" onClick={()=>setDismissed(job.id)} aria-label={t('common.close')}><X size={12}/></button>}</div>
		<span>{t('sshWorkspace.deleteProgress',job.progress)}</span>
		{running&&job.progress.current_path&&<small title={job.progress.current_path}>{job.progress.current_path}</small>}
		{(job.error||job.progress.first_error)&&<p>{job.error||job.progress.first_error}</p>}
	</section>
}
