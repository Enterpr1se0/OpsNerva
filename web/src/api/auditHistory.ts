import type { AuditHistoryCursor, AuditHistoryGroup, AuditPageRequest, AuditPageResult, Run } from '../types/audit'
import { api, request } from './api'

type HistoryResponse={snapshot_at:string;has_more:boolean;next_cursor?:AuditHistoryCursor}

function historyParams(input:AuditPageRequest){
	const params=new URLSearchParams({limit:String(input.limit)})
	if(input.query)params.set('q',input.query)
	if(input.snapshotAt)params.set('snapshot_at',input.snapshotAt)
	if(input.cursor){
		params.set('cursor_started_at',input.cursor.started_at)
		params.set('cursor_id',input.cursor.id)
	}
	return params
}

export const auditHistoryApi={
	async groups(input:AuditPageRequest,signal:AbortSignal):Promise<AuditPageResult<AuditHistoryGroup>>{
		const page=await request<HistoryResponse&{groups:AuditHistoryGroup[]}>(`/api/v1/audit/groups?${historyParams(input)}`,{signal})
		return{items:page.groups,snapshotAt:page.snapshot_at,nextCursor:page.next_cursor||null}
	},
	async runs(sessionID:string,input:AuditPageRequest,signal:AbortSignal):Promise<AuditPageResult<Run>>{
		const params=historyParams(input)
		params.set('session_id',sessionID)
		const page=await request<HistoryResponse&{runs:Run[]}>(`/api/v1/audit/runs?${params}`,{signal})
		return{items:page.runs,snapshotAt:page.snapshot_at,nextCursor:page.next_cursor||null}
	},
	deleteRuns:api.deleteAuditRuns,
}

export type AuditHistoryClient=typeof auditHistoryApi
