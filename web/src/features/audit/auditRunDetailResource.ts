import type { Run, RunDetail } from '../../types/audit'

type DetailSnapshot={run:Run|null;loading:boolean;error:unknown}
type DetailLoader=(id:string,signal:AbortSignal)=>Promise<RunDetail>

// Only instantiated for a row whose detail has been opened. The summary's
// stable object identity changes when the audit page receives a changed run.
export class AuditRunDetailResource {
	private state:DetailSnapshot={run:null,loading:false,error:null}
	private listeners=new Set<()=>void>()
	private summary:Run|null=null
	private active=false
	private operation:AbortController|null=null
	private load:DetailLoader
	constructor(load:DetailLoader){this.load=load}
	getSnapshot=()=>this.state
	subscribe=(listener:()=>void)=>{this.listeners.add(listener);return()=>{this.listeners.delete(listener)}}
	private publish(patch:Partial<DetailSnapshot>){
		if(Object.entries(patch).every(([key,value])=>Object.is(this.state[key as keyof DetailSnapshot],value)))return
		this.state={...this.state,...patch}
		for(const listener of this.listeners)listener()
	}
	setInput(summary:Run,active:boolean){
		if(summary!==this.summary){
			this.cancel();this.summary=summary
			this.publish({run:null,error:null})
		}
		this.active=active
		if(!active)this.cancel()
		else if(!this.state.run&&!this.operation&&!this.state.error)void this.retry()
	}
	cancel(){
		this.operation?.abort();this.operation=null
		this.publish({loading:false})
	}
	retry=async()=>{
		if(!this.active||!this.summary||this.operation)return
		const operation=new AbortController()
		this.operation=operation;this.publish({loading:true,error:null})
		try{
			const detail=await this.load(this.summary.id,operation.signal)
			if(this.operation===operation)this.publish({run:detail.run})
		}catch(error){
			if(this.operation===operation)this.publish({error})
		}finally{
			if(this.operation===operation){this.operation=null;this.publish({loading:false})}
		}
	}
}
