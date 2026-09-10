import { emptyAuditHistoryFilters, type AuditHistoryCursor, type AuditHistoryFilters, type AuditPageRequest, type AuditPageResult } from '../../types/audit'
export type AuditPageSnapshot<T>={items:readonly T[];snapshotAt:string;nextCursor:AuditHistoryCursor|null;ready:boolean;loading:'idle'|'refresh'|'more';error:unknown;failedOperation:'refresh'|'more'|null}
type PageOperation={controller:AbortController;promise:Promise<boolean>}
type PageLoader<T>=(input:AuditPageRequest,signal:AbortSignal)=>Promise<AuditPageResult<T>>

// Match the server's nanosecond ordering; Date.parse loses sub-millisecond ties.
function timeKey(value:string){return value.replace(/(?:\.(\d+))?Z$/,(_match,digits='')=>`.${digits.padEnd(9,'0')}Z`)}
function comparePosition(a:AuditHistoryCursor,b:AuditHistoryCursor){
	const at=timeKey(a.started_at),bt=timeKey(b.started_at)
	return at===bt?(a.id===b.id?0:a.id<b.id?-1:1):at<bt?-1:1
}

export class AuditHistoryPage<T> {
	private state:AuditPageSnapshot<T>={items:[],snapshotAt:'',nextCursor:null,ready:false,loading:'idle',error:null,failedOperation:null}
	private listeners=new Set<()=>void>()
	private operation:PageOperation|null=null
	private active=false
	private filters=emptyAuditHistoryFilters()
	private boundary:AuditHistoryCursor|null=null
	private load:PageLoader<T>
	private position:(item:T)=>AuditHistoryCursor
	private limit:number
	private reportError:(error:unknown)=>void

	constructor(load:PageLoader<T>,position:(item:T)=>AuditHistoryCursor,limit:number,reportError:(error:unknown)=>void){
		this.load=load;this.position=position;this.limit=limit;this.reportError=reportError
	}
	getSnapshot=()=>this.state
	subscribe=(listener:()=>void)=>{this.listeners.add(listener);return()=>{this.listeners.delete(listener)}}
	wait=()=>this.operation?.promise||Promise.resolve(false)
	private publish(patch:Partial<AuditPageSnapshot<T>>){
		if(Object.entries(patch).every(([key,value])=>Object.is(this.state[key as keyof AuditPageSnapshot<T>],value)))return
		this.state={...this.state,...patch}
		for(const listener of this.listeners)listener()
	}
	setActive(active:boolean){this.active=active;if(!active)this.cancel()}
	cancel(){
		const operation=this.operation
		this.operation=null
		operation?.controller.abort()
		this.publish({loading:'idle'})
	}
	reset(filters:AuditHistoryFilters){
		this.cancel();this.filters={...filters};this.boundary=null
		this.publish({items:[],snapshotAt:'',nextCursor:null,ready:false,error:null,failedOperation:null})
	}
	retain(predicate:(item:T)=>boolean){
		this.cancel()
		const items=this.state.items.filter(predicate)
		if(items.length!==this.state.items.length)this.publish({items})
	}
	refresh(snapshotAt?:string){return this.start('refresh',snapshotAt)}
	loadMore(){
		if(this.operation)return this.operation.promise
		if(!this.state.nextCursor)return Promise.resolve(false)
		return this.start('more',this.state.snapshotAt)
	}
	private start(kind:'refresh'|'more',snapshotAt?:string):Promise<boolean>{
		if(!this.active)return Promise.resolve(false)
		this.cancel()
		const operation:PageOperation={controller:new AbortController(),promise:Promise.resolve(false)}
		this.operation=operation
		this.publish({loading:kind,error:null,failedOperation:null})
		operation.promise=this.read(kind,snapshotAt,operation).then(result=>{
			if(!result||this.operation!==operation)return false
			const current=new Map(this.state.items.map(item=>[this.position(item).id,item]))
			// Deleting a group's latest run can move that group into a later
			// page while a multi-page read is in progress. Keep one current row.
			const received=new Map(result.items.map(item=>[this.position(item).id,item]))
			const incoming=[...received.values()].map(item=>{
				const previous=current.get(this.position(item).id)
				return previous&&JSON.stringify(previous)===JSON.stringify(item)?previous:item
			})
			if(received.size!==result.items.length)incoming.sort((a,b)=>-comparePosition(this.position(a),this.position(b)))
			let items=incoming
			if(kind==='more'){
				const overlap=incoming.some(item=>current.has(this.position(item).id))
				for(const item of incoming)current.set(this.position(item).id,item)
				items=[...current.values()]
				if(overlap)items.sort((a,b)=>-comparePosition(this.position(a),this.position(b)))
			}
			if(kind==='more'||!this.boundary){const last=items.at(-1);if(last)this.boundary=this.position(last)}
			const unchanged=items.length===this.state.items.length&&items.every((item,index)=>item===this.state.items[index])
			this.publish({items:unchanged?this.state.items:items,snapshotAt:result.snapshotAt,nextCursor:result.nextCursor,ready:true})
			return true
		}).catch(error=>{
			if(this.operation===operation){this.publish({error,ready:true,failedOperation:kind});this.reportError(error)}
			return false
		}).finally(()=>{
			if(this.operation===operation){this.operation=null;this.publish({loading:'idle'})}
		})
		return operation.promise
	}
	private async read(kind:'refresh'|'more',snapshotAt:string|undefined,operation:PageOperation):Promise<AuditPageResult<T>|null>{
		let cursor=kind==='more'?this.state.nextCursor:undefined
		const boundary=kind==='refresh'?this.boundary:null
		const items:T[]=[]
		while(this.operation===operation){
			const page=await this.load({...this.filters,limit:this.limit,snapshotAt,cursor:cursor||undefined},operation.controller.signal)
			if(this.operation!==operation)return null
			if(snapshotAt&&timeKey(page.snapshotAt)!==timeKey(snapshotAt))throw new Error('Audit history snapshot changed during pagination')
			snapshotAt=page.snapshotAt
			if(page.nextCursor&&(!page.items.length||cursor&&comparePosition(page.nextCursor,cursor)>=0))throw new Error('Audit history cursor did not advance')
			// Re-read through the previously loaded boundary, not merely the same
			// number of pages. Newer records may span several complete pages.
			if(boundary){
				const older=page.items.findIndex(item=>comparePosition(this.position(item),boundary)<0)
				if(older>=0){
					items.push(...page.items.slice(0,older))
					return{items,snapshotAt,nextCursor:boundary}
				}
			}
			items.push(...page.items)
			const last=page.items.at(-1)
			if(!boundary||!page.nextCursor||last&&comparePosition(this.position(last),boundary)<=0)return{items,snapshotAt,nextCursor:page.nextCursor}
			cursor=page.nextCursor
		}
		return null
	}
}
