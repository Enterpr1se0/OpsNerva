import type { AuditRunDeleteResult, Run } from '../../types'

export const directAuditSessionID='__direct__'

export function auditSessionID(run:Pick<Run,'session_id'>){
	return run.session_id||directAuditSessionID
}

export function applyAuditRunDeletion(runs:Run[],result:AuditRunDeleteResult){
	if(result.deleted<=0)return runs
	const retained=new Set(result.retained_run_ids||[])
	return runs.filter(run=>{
		const inScope=result.scope==='all'||(result.scope==='direct'&&!run.session_id)||(result.scope==='session'&&run.session_id===result.session_id)
		return !inScope||retained.has(run.id)
	})
}
