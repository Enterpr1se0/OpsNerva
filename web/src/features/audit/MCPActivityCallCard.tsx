import { memo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, History, LoaderCircle } from 'lucide-react'
import { api } from '../../api/api'
import { localeFor } from '../../lib/i18n'
import { errorText, formatFileSize } from '../../lib/utils'
import type { Host, MCPToolCall, Run } from '../../types'
import { LazyJSONDetails, ToolOutputPanel } from '../tools/components/ToolOutput'
import { ToolSummaryIcon } from '../tools/components/ToolSummaryIcon'
import { parseRecord } from '../tools/payload'
import { requestFromRun } from '../tools/request'
import { toolArgumentSummary } from '../tools/summary'
import { AuditRunDetail } from './AuditRunDetail'
import type { MCPCallOutput } from './useMCPActivity'

export const MCPActivityCallCard=memo(function MCPActivityCallCard({call,output,hosts}:{call:MCPToolCall;output?:MCPCallOutput;hosts:Host[]}){
	const {t,i18n:instance}=useTranslation()
	const args=parseRecord(call.arguments_json)
	const summary=toolArgumentSummary(call.tool_name,args)
	const progress=output&&output.totalBytes>0?Math.min(100,Math.round(output.transferredBytes/output.totalBytes*100)):0
	return <details className={`mcp-call-card panel ${call.status}`}>
		<summary><span className="mcp-call-icon"><ToolSummaryIcon name={call.tool_name}/></span><span className="mcp-call-heading"><code>{call.tool_name}</code>{summary&&<small>{summary}</small>}</span><time>{new Date(call.started_at).toLocaleString(localeFor(instance.language))}</time><span className={`run-status ${call.status}`}>{t(`statusLabels.${call.status}`,{defaultValue:call.status})}</span><ChevronRight size={15}/></summary>
		<div className="mcp-call-body">
			<dl className="mcp-call-meta"><div><dt>{t('audit.callId')}</dt><dd><code>{call.id}</code></dd></div>{call.operation_status&&<div><dt>{t('common.status')}</dt><dd>{t(`statusLabels.${call.operation_status}`,{defaultValue:call.operation_status})}</dd></div>}{call.approval_id&&<div><dt>{t('audit.approvalId')}</dt><dd><code>{call.approval_id}</code></dd></div>}{call.task_id&&<div><dt>{t('audit.taskId')}</dt><dd><code>{call.task_id}</code></dd></div>}{call.shell_id&&<div><dt>Shell</dt><dd><code>{call.shell_id}</code></dd></div>}{call.tunnel_id&&<div><dt>Tunnel</dt><dd><code>{call.tunnel_id}</code></dd></div>}</dl>
			{call.error&&<div className="inline-error">{call.error}</div>}
			{progress>0&&<div className="file-transfer-progress" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={progress}><div><span>{t('tool.transferProgress')}</span><b>{formatFileSize(output!.transferredBytes)} / {formatFileSize(output!.totalBytes)}</b></div><i><em style={{width:`${progress}%`}}/></i></div>}
			{output&&(output.stdout||output.stderr)&&<div className="tool-output-grid">{output.stdout&&<ToolOutputPanel kind="stdout" label="STDOUT" content={output.stdout} live={call.status==='running'}/>} {output.stderr&&<ToolOutputPanel kind="stderr" label="STDERR" content={output.stderr} live={call.status==='running'}/>}</div>}
			<LazyJSONDetails value={args}/>
			{call.run_id&&<MCPRunEvidence runID={call.run_id} hosts={hosts}/>}
		</div>
	</details>
})

function MCPRunEvidence({runID,hosts}:{runID:string;hosts:Host[]}){
	const {t}=useTranslation()
	const [detail,setDetail]=useState<Run|null>(null)
	const [loading,setLoading]=useState(false)
	const [error,setError]=useState('')
	const load=async()=>{if(detail||loading)return;setLoading(true);try{setDetail((await api.runDetail(runID)).run)}catch(err){setError(errorText(err))}finally{setLoading(false)}}
	return <details className="mcp-run-evidence" onToggle={event=>{if(event.currentTarget.open)void load()}}><summary><History size={14}/><span>{t('audit.runEvidence')}</span><code>{runID}</code><ChevronRight size={14}/></summary><div>{loading?<div className="audit-loading" role="status"><LoaderCircle className="spin" size={14}/><span>{t('common.loading')}</span></div>:error?<div className="inline-error">{error}</div>:detail&&<AuditRunDetail run={detail} req={requestFromRun(detail)||{request:detail.request_json}} hosts={hosts}/>}</div></details>
}
