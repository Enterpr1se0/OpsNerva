import type { SFTPDeletion } from '../../types'

export const isSFTPDeletionActive=(job:SFTPDeletion|undefined)=>job?.status==='running'||job?.status==='stopping'

// Progress subscribers are local to a host. Completion listeners reconcile the
// visible directory only on transitions, never by replaying old results on mount.
export class SFTPDeletionStore {
	private jobs=new Map<string,SFTPDeletion>()
	private listeners=new Map<string,Set<()=>void>>()
	private completions=new Map<string,Set<(path:string,completed:boolean)=>void>>()
	private snapshotRevision=0
	readonly starting=new Set<string>()

	record=(hostID:string)=>this.jobs.get(hostID)
	subscribe=(hostID:string,listener:()=>void)=>{
		const listeners=this.listeners.get(hostID)||new Set()
		this.listeners.set(hostID,listeners);listeners.add(listener)
		return()=>{listeners.delete(listener);if(!listeners.size)this.listeners.delete(hostID)}
	}
	onComplete=(hostID:string,listener:(path:string,completed:boolean)=>void)=>{
		const listeners=this.completions.get(hostID)||new Set()
		this.completions.set(hostID,listeners);listeners.add(listener)
		return()=>{listeners.delete(listener);if(!listeners.size)this.completions.delete(hostID)}
	}
	private changed(hostID:string){for(const listener of this.listeners.get(hostID)||[])listener()}
	private completed(job:SFTPDeletion){for(const listener of this.completions.get(job.host_id)||[])listener(job.path,job.status==='completed')}

	update(job:SFTPDeletion){
		const previous=this.jobs.get(job.host_id)
		if((job.revision<=this.snapshotRevision&&!previous)||(previous&&job.revision<=previous.revision))return
		this.jobs.set(job.host_id,job)
		this.changed(job.host_id)
		if(!isSFTPDeletionActive(job)&&(previous?.id!==job.id||isSFTPDeletionActive(previous)))this.completed(job)
		// Match the bounded server history while disconnected snapshots are absent.
		if(this.jobs.size>64){
			const oldest=[...this.jobs.values()].filter(item=>!isSFTPDeletionActive(item)).sort((a,b)=>a.revision-b.revision)[0]
			if(oldest){this.jobs.delete(oldest.host_id);this.changed(oldest.host_id)}
		}
	}
	snapshot(jobs:SFTPDeletion[]){
		const previous=this.jobs
		this.snapshotRevision=Math.max(0,...jobs.map(job=>job.revision))
		this.jobs=new Map(jobs.map(job=>{
			const old=previous.get(job.host_id)
			return [job.host_id,old?.id===job.id&&old.revision>job.revision?old:job]
		}))
		for(const hostID of new Set([...previous.keys(),...this.jobs.keys()])){
			const job=this.jobs.get(hostID),old=previous.get(hostID)
			if(job===old)continue
			this.changed(hostID)
			// A service restart may discard an active, partially completed task.
			if(!job&&old&&isSFTPDeletionActive(old))this.completed(old)
			if(job&&!isSFTPDeletionActive(job)&&(isSFTPDeletionActive(old)||this.starting.has(hostID)&&old?.id!==job.id))this.completed(job)
		}
	}
}
