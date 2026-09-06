import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Braces, ChevronRight, ShieldAlert, TerminalSquare } from 'lucide-react'
import { CopyButton, CopyablePre } from '../../components/CopyButton'
import { HighlightedCode } from '../../components/HighlightedCode'
import { inferScriptLanguage, languageFromPath } from '../../lib/codeLanguage'
import { sshTunnelRoute } from '../../lib/utils'
import type { Host, Run } from '../../types'
import { CompactTable } from '../tools/components/CompactTable'
import { DiffViewer } from '../tools/components/DiffViewer'
import { ToolOutputPanel } from '../tools/components/ToolOutput'
import { hasRecordEntries, jsonRecord, numberValue, previewStructuredValue, previewText, recordTableRows, textValue, toolOutputPreviewChars, type JsonRecord } from '../tools/payload'
import { executionPermission, fullProgram, hostIdentity } from '../tools/request'
import { formatDuration, toolLabel } from '../tools/summary'

export function AuditRunDetail({run,req,hosts}:{run:Run;req:JsonRecord;hosts:Host[]}){
	const {t}=useTranslation()
	const [requestExpanded,setRequestExpanded]=useState(false)
	const script=textValue(req.script)
	const program=textValue(req.program)?fullProgram(req):''
	const mode=textValue(req.mode)
	const workspaceID=textValue(req.workspace_id)
	const remotePath=textValue(req.remote_path)
	const relativePath=textValue(req.relative_path)
	const filePath=remotePath||relativePath
	const destinationHost=hostIdentity(hosts,run.host_id)
	const sourceHost=hostIdentity(hosts,textValue(req.source_host_id))
	const sourcePath=textValue(req.source_path)
	const change=jsonRecord(req.change)
	const env=jsonRecord(req.env)
	const workspaceShellBackend=textValue(req.workspace_shell_backend)
	const searchMode=mode==='remote_search'||mode==='workspace_search'
	const readMode=mode==='remote_read'||mode==='workspace_read'
	const workspaceUpload=mode==='workspace_upload'
	const workspaceDownload=mode==='workspace_download'
	const workspaceTransfer=workspaceUpload||workspaceDownload
	const sshTransfer=mode==='ssh_file_transfer'
	const permission=executionPermission(req,hosts,run.host_id,...(sshTransfer?[textValue(req.source_host_id)]:[]))
	const rootExecution=permission==='root'
	const tunnelMode=mode==='ssh_tunnel_start'
	const shellMode=mode==='ssh_shell_start'||mode==='workspace_shell_start'
	const shellModeLabel=mode==='ssh_shell_start'?`SSH Shell · ${t('sshShell.toolActions.start')}`:`Workspace Shell · ${t('sshShell.toolActions.start')}`
	const tunnelRoute=tunnelMode?sshTunnelRoute(destinationHost.name||destinationHost.id,textValue(req.direction)||'local',textValue(req.local_host)||'127.0.0.1',numberValue(req.local_port),textValue(req.remote_host),numberValue(req.remote_port),t('tunnels.automaticPort')):''
	const shellTarget=`${mode==='workspace_shell_start'?`${workspaceID}:${textValue(req.cwd)||'.'}`:destinationHost.name||destinationHost.id} · PTY`
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const commandText=shellMode?shellTarget:tunnelMode?tunnelRoute:workspaceUpload?`workspace_upload ${workspaceID}:${relativePath} → ${destinationHost.name||destinationHost.id}:${remotePath}`:workspaceDownload?`workspace_download ${destinationHost.name||destinationHost.id}:${remotePath} → ${workspaceID}:${relativePath}`:sshTransfer?`${[sourceHost.name||sourceHost.id,sourcePath].filter(Boolean).join(':')} → ${destinationHost.name||destinationHost.id}:${remotePath}`:searchMode||readMode?`${searchMode?'search':'read'} ${fileTarget}`:script?script:program?program:filePath?`${mode} ${fileTarget}`:toolLabel(run.tool_name||'')
	return <div className="audit-run-detail">
		<div className="audit-run-primary">
			<section className="audit-operation-pane">
				<div className="tool-command-head"><span>{shellMode?shellModeLabel:tunnelMode?t('tunnels.forwarding'):searchMode?t('tool.searchOperation'):readMode?t('tool.readOperation'):workspaceTransfer||sshTransfer||filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{(workspaceShellBackend||rootExecution)&&<div className="audit-operation-badges">{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}{rootExecution&&<em><ShieldAlert size={12}/>root</em>}</div>}</div>
				<div className="tool-command-block"><CopyButton value={program&&commandText===program?()=>fullProgram(req,true):commandText}/><pre>{program&&commandText===program?<><span className="prompt-sign">$</span> <HighlightedCode code={previewText(program)} language="bash"/></>:<HighlightedCode code={previewText(commandText,toolOutputPreviewChars)} language={script?inferScriptLanguage(script):undefined} autoDetect/>}</pre></div>
				{change&&textValue(change.diff)&&<DiffViewer change={change}/>}
			</section>
			<aside className="audit-run-context">
				<dl className="audit-run-facts">
					<div><dt>{workspaceID&&!sshTransfer?t('common.workspace'):t('tool.targetHost')}</dt><dd>{workspaceID&&!sshTransfer?workspaceID:[destinationHost.name,destinationHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>
					{sshTransfer&&<div><dt>{t('tool.sourceHost')}</dt><dd>{[sourceHost.name,sourceHost.id].filter(Boolean).join(' · ')||'—'}</dd></div>}
					<div><dt>{tunnelMode?t('tunnels.remoteEndpoint'):filePath?t('tool.filePath'):t('tool.workingDirectory')}</dt><dd>{tunnelMode?`${textValue(req.remote_host)}:${numberValue(req.remote_port)}`:filePath||textValue(req.cwd)||t('tool.defaultDirectory')}</dd></div>
					<div><dt>{t('tool.permission')}</dt><dd>{permission}</dd></div>
					<div><dt>{t('tool.duration')}</dt><dd>{formatDuration(undefined,run)}</dd></div>
				</dl>
				{textValue(req.reason)&&<div className="audit-run-purpose"><span>{t('tool.reason')}</span><p>{textValue(req.reason)}</p></div>}
			</aside>
		</div>
		{(run.stdout_redacted||run.stderr_redacted||run.error)&&<div className="tool-output-grid">{run.stdout_redacted&&<ToolOutputPanel kind="stdout" label="STDOUT · REDACTED" content={run.stdout_redacted} live={false} language={readMode?languageFromPath(filePath):undefined}/>} {run.stderr_redacted&&<ToolOutputPanel kind="stderr" label="STDERR · REDACTED" content={run.stderr_redacted} live={false}/>} {run.error&&!run.stderr_redacted&&<ToolOutputPanel kind="stderr" label={t('common.error')} content={run.error} live={false}/>}</div>}
		<details className="audit-request-detail" open={requestExpanded} onToggle={event=>setRequestExpanded(event.currentTarget.open)}>
			<summary><Braces size={14}/><span>{t('tool.normalizedRequest')}</span><ChevronRight size={14}/></summary>
			{requestExpanded&&<div className="audit-request-detail-body">
				<dl className="audit-request-meta"><div><dt>{t('common.operation')}</dt><dd>{toolLabel(run.tool_name||'')}</dd></div><div><dt>{t('tool.runId')}</dt><dd>{run.id}</dd></div></dl>
				{env&&hasRecordEntries(env)&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={recordTableRows(env)}/>}
				<CopyablePre value={()=>JSON.stringify(req,null,2)}><HighlightedCode code={JSON.stringify(previewStructuredValue(req),null,2)} language="json"/></CopyablePre>
			</div>}
		</details>
	</div>
}
