import type { AuditHistoryClient } from '../../api/auditHistory'
import type { ApplicationEvent } from '../../api/appEvents'
import type { AuditHistoryGroup, AuditRunDeleteResult, Run } from '../../types/audit'
import { AuditHistoryPage } from './auditHistoryPage'

export type AuditHistoryEvent={id?:string;type?:string;data?:AuditRunDeleteResult}
type AuditSubscription=(listener:(event:ApplicationEvent<AuditHistoryEvent>)=>void)=>()=>void
type AuditHistoryView={query:string;expanded:ReadonlySet<string>;active:boolean}
const eventBatchDelay=250

function deletionIncludes(result:AuditRunDeleteResult,sessionID:string){
	return result.scope==='all'||(result.scope==='direct'?sessionID==='':sessionID===result.session_id)
}

// Own one instance per audit view lifetime. Page snapshots have independent
// subscriptions so loading a group's commands does not publish the group list.
export class AuditHistoryStore {
	readonly groups:AuditHistoryPage<AuditHistoryGroup>
	private runs=new Map<string,AuditHistoryPage<Run>>()
	private view:AuditHistoryView={query:'',expanded:new Set(),active:false}
	private manuallyCollapsed=new Set<string>()
	private listeners=new Set<()=>void>()
	private active=false
	private unsubscribe:(()=>void)|null=null
	private timer:ReturnType<typeof setTimeout>|undefined
	private cycle:{promise:Promise<void>}|null=null
	private queued=false
	private generation=0
	private deletions=new Set<string>()
	private api:AuditHistoryClient
	private subscribeAudit:AuditSubscription
	private reportError:(error:unknown)=>void

	constructor(api:AuditHistoryClient,subscribeAudit:AuditSubscription,reportError:(error:unknown)=>void){
		this.api=api;this.subscribeAudit=subscribeAudit;this.reportError=reportError
		this.groups=new AuditHistoryPage(api.groups,group=>({started_at:group.latest_started_at,id:group.session_id}),20,reportError)
	}
	getViewSnapshot=()=>this.view
	subscribeView=(listener:()=>void)=>{this.listeners.add(listener);return()=>{this.listeners.delete(listener)}}
	private publish(view:AuditHistoryView){this.view=view;for(const listener of this.listeners)listener()}
	getRuns=(sessionID:string)=>{
		let page=this.runs.get(sessionID)
		if(!page){
			page=new AuditHistoryPage((input,signal)=>this.api.runs(sessionID,input,signal),run=>({started_at:run.started_at,id:run.id}),50,this.reportError)
			page.reset(this.view.query)
			this.runs.set(sessionID,page)
		}
		return page
	}
	setActive(active:boolean){
		if(active===this.active)return
		this.active=active
		this.publish({...this.view,active})
		this.groups.setActive(active)
		for(const [id,page] of this.runs)page.setActive(active&&this.view.expanded.has(id))
		if(active){this.unsubscribe=this.subscribeAudit(this.onEvent);void this.refresh()}
		else{this.unsubscribe?.();this.unsubscribe=null;this.stopCycle()}
	}
	setQuery(query:string){
		if(query===this.view.query)return
		this.stopCycle()
		this.groups.reset(query)
		for(const page of this.runs.values())page.reset(query)
		this.publish({...this.view,query})
		void this.refresh()
	}
	setOpen(sessionID:string,open:boolean){
		if(open)this.manuallyCollapsed.delete(sessionID);else this.manuallyCollapsed.add(sessionID)
		if(this.view.expanded.has(sessionID)===open)return
		const expanded=new Set(this.view.expanded)
		if(open)expanded.add(sessionID);else expanded.delete(sessionID)
		this.publish({...this.view,expanded})
		const page=this.getRuns(sessionID)
		page.setActive(this.active&&open)
		const snapshot=this.groups.getSnapshot().snapshotAt
		if(open&&this.active&&snapshot)void page.refresh(snapshot)
	}
	private expandSmallHistory(){
		const {items,nextCursor}=this.groups.getSnapshot()
		if(nextCursor||items.reduce((count,group)=>count+group.run_count,0)>20)return
		const expanded=new Set(this.view.expanded)
		for(const group of items)if(!this.manuallyCollapsed.has(group.session_id))expanded.add(group.session_id)
		if(expanded.size!==this.view.expanded.size)this.publish({...this.view,expanded})
	}
	loadMoreGroups=async()=>{
		const generation=this.generation
		if(!await this.groups.loadMore()||generation!==this.generation)return
		const {items,snapshotAt}=this.groups.getSnapshot()
		const currentWindow=()=>this.active&&generation===this.generation&&this.groups.getSnapshot().snapshotAt===snapshotAt
		for(const group of items){
			if(!currentWindow())return
			if(!this.view.expanded.has(group.session_id))continue
			const page=this.getRuns(group.session_id)
			page.setActive(true)
			const state=page.getSnapshot()
			if(state.ready&&state.snapshotAt===snapshotAt)continue
			if(state.loading!=='idle'){
				await page.wait()
				if(!currentWindow())return
				if(!this.view.expanded.has(group.session_id))continue
				const loaded=page.getSnapshot()
				if(loaded.error||loaded.ready&&loaded.snapshotAt===snapshotAt)continue
			}
			await page.refresh(snapshotAt)
		}
	}
	refresh=():Promise<void>=>{
		if(!this.active)return Promise.resolve()
		if(this.cycle){this.queued=true;return this.cycle.promise}
		this.clearTimer()
		const cycle={promise:Promise.resolve()}
		this.cycle=cycle
		cycle.promise=(async()=>{
			await this.groups.wait()
			if(this.cycle!==cycle||!this.active||!await this.groups.refresh())return
			if(this.cycle!==cycle)return
			const {items,snapshotAt}=this.groups.getSnapshot()
			const loaded=new Set(items.map(group=>group.session_id))
			for(const [id,page] of this.runs){
				if(!loaded.has(id)){page.setActive(false);this.runs.delete(id)}
			}
			this.expandSmallHistory()
			// Refresh only open groups, sequentially to bound request concurrency.
			for(const id of this.view.expanded){
				if(this.cycle!==cycle||!this.active)return
				if(!loaded.has(id)||!this.view.expanded.has(id))continue
				const page=this.getRuns(id)
				page.setActive(true)
				const read=await page.wait()
				if(this.cycle!==cycle||!this.view.expanded.has(id))continue
				if(read&&page.getSnapshot().snapshotAt===snapshotAt)continue
				await page.refresh(snapshotAt)
			}
		})().finally(()=>{
			if(this.cycle!==cycle)return
			this.cycle=null
			if(this.queued){this.queued=false;this.scheduleRefresh()}
		})
		return cycle.promise
	}
	private clearTimer(){if(this.timer!==undefined)clearTimeout(this.timer);this.timer=undefined}
	private stopCycle(){this.clearTimer();this.cycle=null;this.queued=false;this.generation++}
	private scheduleRefresh(){
		if(!this.active)return
		if(this.cycle){this.queued=true;return}
		if(this.timer!==undefined)return
		this.timer=setTimeout(()=>{this.timer=undefined;void this.refresh()},eventBatchDelay)
	}
	private onEvent=(event:ApplicationEvent<AuditHistoryEvent>)=>{
		if(event.type==='error'){this.reportError(event.error||'Audit event stream failed');return}
		if(event.type!=='event')return
		if(event.mode==='delta'&&event.data?.type==='audit_records_deleted'&&event.data.data){
			this.applyDeletion({...event.data.data,audit_event_id:event.data.id})
			return
		}
		if(event.mode==='snapshot'){
			this.stopCycle();this.groups.cancel()
			for(const page of this.runs.values())page.cancel()
		}
		this.scheduleRefresh()
	}
	applyDeletion(result:AuditRunDeleteResult){
		if(result.deleted<=0)return
		const eventID=result.audit_event_id
		if(eventID){
			if(this.deletions.has(eventID))return
			this.deletions.add(eventID)
			if(this.deletions.size>64)this.deletions.delete(this.deletions.values().next().value!)
		}
		this.stopCycle()
		this.groups.retain(group=>!deletionIncludes(result,group.session_id)||result.retained>0)
		const retained=new Set(result.retained_run_ids||[])
		for(const [id,page] of this.runs){
			if(deletionIncludes(result,id))page.retain(run=>retained.has(run.id))
		}
		// Revalidate counts and cursors; never subtract counts twice when REST
		// and WebSocket report the same committed deletion in either order.
		this.scheduleRefresh()
	}
	deleteRuns=async(sessionID?:string|null)=>{
		try{
			const result=await this.api.deleteRuns(sessionID)
			this.applyDeletion(result)
			return result
		}catch(error){this.reportError(error);throw error}
	}
	dispose(){
		this.setActive(false)
		this.listeners.clear()
	}
}
