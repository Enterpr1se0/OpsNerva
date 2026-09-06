import { memo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { ChevronRight, LoaderCircle, ShieldCheck } from 'lucide-react'
import { api } from '../../api/api'
import { localeFor } from '../../lib/i18n'
import { errorText, sshTunnelRoute } from '../../lib/utils'
import type { Host, Run } from '../../types'
import { numberValue, textValue, type JsonRecord } from '../tools/payload'
import { fullProgram, hostIdentity, requestFromRun } from '../tools/request'
import { compactScript, runAutoApproved } from '../tools/summary'
import { AuditRunDetail } from './AuditRunDetail'

function auditOperationSummary(req:JsonRecord,run:Run,hosts:Host[],t:TFunction){
	const mode=textValue(req.mode)
	const destinationHost=hostIdentity(hosts,run.host_id)
	const destinationName=destinationHost.name||destinationHost.id
	const workspaceID=textValue(req.workspace_id)
	const relativePath=textValue(req.relative_path)
	const remotePath=textValue(req.remote_path)
	const sourceHost=hostIdentity(hosts,textValue(req.source_host_id))
	switch(mode){
		case'program':return fullProgram(req)
		case'script':return `${t('toolNames.ssh_run_script')} · ${compactScript(textValue(req.script))}`
		case'ssh_shell_start':return `SSH Shell · ${t('sshShell.toolActions.start')}`
		case'workspace_shell_start':return `Workspace Shell · ${t('sshShell.toolActions.start')} · ${workspaceID}:${textValue(req.cwd)||'.'}`
		case'ssh_tunnel_start':return sshTunnelRoute(destinationName,textValue(req.direction)||'local',textValue(req.local_host)||'127.0.0.1',numberValue(req.local_port),textValue(req.remote_host),numberValue(req.remote_port),t('tunnels.automaticPort'))
		case'remote_read':return `${t('toolNames.ssh_file_read')} · ${remotePath}`
		case'remote_search':return `${t('toolNames.ssh_file_search_mode')} · ${remotePath} · ${textValue(req.search_pattern)}`
		case'remote_edit':return `${t('toolNames.ssh_file_edit')} · ${remotePath}`
		case'workspace_read':return `${t('toolNames.workspace_file_read')} · ${workspaceID}:${relativePath}`
		case'workspace_search':return `${t('toolNames.workspace_file_search_mode')} · ${workspaceID}:${relativePath} · ${textValue(req.search_pattern)}`
		case'workspace_edit':return `${t('toolNames.workspace_file_edit')} · ${workspaceID}:${relativePath}`
		case'workspace_delete':return `${t('toolNames.workspace_file_delete')} · ${workspaceID}:${relativePath}`
		case'workspace_directory_list':return `${t('toolNames.workspace_file_list')} · ${workspaceID}:${relativePath}`
		case'workspace_shell':return `${t('toolNames.workspace_shell')} · ${compactScript(textValue(req.script))}`
		case'workspace_upload':return `${t('toolNames.workspace_file_upload')} · ${workspaceID}:${relativePath} → ${destinationName}:${remotePath}`
		case'workspace_download':return `${t('toolNames.workspace_file_download')} · ${destinationName}:${remotePath} → ${workspaceID}:${relativePath}`
		case'ssh_file_transfer':return `${t('toolNames.ssh_file_transfer')} · ${sourceHost.name||sourceHost.id}:${textValue(req.source_path)} → ${destinationName}:${remotePath}`
		default:return mode||t('audit.unknownOperation')
	}
}

export const AuditRunRow=memo(function AuditRunRow({run,hosts}:{run:Run;hosts:Host[]}){
	const {t,i18n:instance}=useTranslation()
	const [detail,setDetail]=useState<Run|null>(null)
	const [loading,setLoading]=useState(false)
	const [error,setError]=useState('')
	const req=requestFromRun(run)||{request:run.request_json}
	const auditHost=hostIdentity(hosts,run.host_id)
	const workspaceID=textValue(req.workspace_id)
	const target=auditHost.name||(run.host_id.startsWith('workspace_')?workspaceID:run.host_id)||'—'
	const operation=auditOperationSummary(req,run,hosts,t)
	const open=async()=>{
		if(detail||loading)return
		setLoading(true);setError('')
		try{setDetail((await api.runDetail(run.id)).run)}
		catch(err){setError(errorText(err))}
		finally{setLoading(false)}
	}
	const resolved=detail||run
	const resolvedRequest=requestFromRun(resolved)||req
	return <details onToggle={event=>{if(event.currentTarget.open)void open()}}>
		<summary className="audit-row">
			<span>{new Date(run.started_at).toLocaleString(localeFor(instance.language))}</span>
			<span className="command">{operation}</span>
			<span className="audit-run-status"><span className={`run-status ${run.status}`}>{t(`statusLabels.${run.status}`,{defaultValue:run.status})}</span>{runAutoApproved(run)&&<span className="auto-approved"><ShieldCheck size={11}/>{t('approval.autoApproved')}</span>}</span>
			<span title={run.host_id}>{target}</span><span>{run.exit_code}</span><ChevronRight className="audit-run-chevron" size={15}/>
		</summary>
		<div className="run-detail">
			{loading?<div className="audit-loading" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>:error?<div className="inline-error">{error}</div>:detail&&<AuditRunDetail run={resolved} req={resolvedRequest} hosts={hosts}/>}
		</div>
	</details>
})
