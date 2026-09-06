import type { TFunction } from 'i18next'
import { activeLiveTaskStatus, type LiveSSHTaskSnapshot } from '../../lib/liveTasks'
import { sshTunnelRoute } from '../../lib/utils'
import type { Host, Run } from '../../types'
import type { ChatEntry } from '../chat/types'
import { jsonRecord, numberValue, previewText, recordArray, textValue, toolCollectionPreviewItems, toolOutputPreviewChars, type JsonRecord } from './payload'
import { executionPermission, fullProgram, hostIdentity, requestFromRun } from './request'
import { compactScript, formatDuration, runAutoApproved, toolArgumentSummary, toolLabel } from './summary'

type ToolTarget={kind:'host'|'workspace'|'scope';label:string;name:string;id?:string}
export type WorkspaceTransferEndpoint={kind:'host'|'workspace';name:string;path:string}
export type WorkspaceTransferRouteValue={source:WorkspaceTransferEndpoint;destination:WorkspaceTransferEndpoint}

function latestOutput(value:string,limit=3){const tail=value.length>toolOutputPreviewChars?value.slice(-toolOutputPreviewChars):value;return tail.trimEnd().split(/\r?\n/).filter(line=>line.trim()!=='').slice(-limit).map(line=>previewText(line,180)).join('\n')}
function cleanFileChangeOutput(value:string){const lines=value.split(/\r?\n/),result:string[]=[];for(let index=0;index<lines.length;index++){if(lines[index]==='__OPS_FILE_VALIDATION_OK__')continue;if(lines[index]==='__OPS_FILE_AFTER__'){index++;continue}result.push(lines[index])}return result.join('\n').trim()}

type ToolEventInput={
	entry:ChatEntry
	storedPayload:JsonRecord
	runs:Run[]
	hosts:Host[]
	liveSSHTaskOwner:boolean
	currentLiveSSHTask?:LiveSSHTaskSnapshot
	t:TFunction
}

export function buildToolEventView({entry,storedPayload,runs,hosts,liveSSHTaskOwner,currentLiveSSHTask,t}:ToolEventInput){
	const storedDisplay=jsonRecord(storedPayload._display)
	const storedToolArguments=jsonRecord(storedDisplay?.arguments)
	const sshTaskOperation=entry.tool==='ssh_task'
	const sshTaskAction=textValue(storedToolArguments?.action)
	const sshTaskID=sshTaskOperation?(textValue(storedPayload.task_id)||textValue(jsonRecord(storedPayload.result)?.task_id)||textValue(storedToolArguments?.task_id)):''
	const storedSSHTaskStatus=textValue(storedPayload.status)||textValue(jsonRecord(storedPayload.result)?.status)
	const liveSSHTaskUnavailable=!!currentLiveSSHTask?.error&&!textValue(currentLiveSSHTask.task?.id)
	const liveSSHTaskStatus=liveSSHTaskUnavailable?'failed':textValue(currentLiveSSHTask?.task?.status)||textValue(currentLiveSSHTask?.result?.status)
	const useLiveSSHTask=liveSSHTaskOwner&&!!currentLiveSSHTask&&activeLiveTaskStatus(storedSSHTaskStatus)
	const payload=useLiveSSHTask?{
		...storedPayload,
		...currentLiveSSHTask.result,
		task_id:sshTaskID,
		run_id:textValue(currentLiveSSHTask.result?.run_id)||textValue(currentLiveSSHTask.task?.run_id)||textValue(storedPayload.run_id),
		status:liveSSHTaskStatus||storedSSHTaskStatus,
		wait_deadline_reached:false,
		...(currentLiveSSHTask.error?{message:currentLiveSSHTask.error,code:liveSSHTaskUnavailable?'task_status_unavailable':'remote_failed'}:{}),
		_display:storedDisplay,
	}:storedPayload
	const taskPayload=jsonRecord(payload.task)
	const resultPayload=jsonRecord(payload.result)
  const runID=entry.runId||textValue(payload.run_id)||textValue(taskPayload?.run_id)||textValue(resultPayload?.run_id)
	const run=runs.find(item=>item.id===runID)
	const display=jsonRecord(payload._display)
	const toolArguments=jsonRecord(display?.arguments)
	const displayRequest=jsonRecord(display?.request)||requestFromRun(run)
	const executionTool=!!entry.tool&&['ssh_exec','ssh_run_script','ssh_tunnel','ssh_shell','ssh_file_read','ssh_file_list','ssh_file_edit','ssh_file_transfer','workspace_file_list','workspace_file_read','workspace_file_edit','workspace_file_delete','workspace_file_upload','workspace_file_download','workspace_shell'].includes(entry.tool)
	const request=executionTool?displayRequest:undefined
	const shellPayload=jsonRecord(payload.shell)||jsonRecord(resultPayload?.shell)
	const destinationHostID=textValue(display?.host_id)||run?.host_id||textValue(request?.host_id)||textValue(toolArguments?.host_id)||textValue(toolArguments?.destination_host_id)||textValue(shellPayload?.host_id)||textValue(currentLiveSSHTask?.task?.host_id)||textValue(payload.host_id)||textValue(resultPayload?.host_id)
	const destinationHost=hostIdentity(hosts,destinationHostID)
  const hostID=destinationHost.id
  const hostName=destinationHost.name||hostID||'—'
  const rawPayloadStatus=textValue(payload.status)||textValue(taskPayload?.status)||textValue(resultPayload?.status)
	const payloadStatus=sshTaskOperation?(rawPayloadStatus==='running'?'in_progress':rawPayloadStatus==='waiting_for_approval'?'approval_required':rawPayloadStatus):rawPayloadStatus
  const runStatus=run?.status==='running'?'in_progress':run?.status
  const status=sshTaskOperation?payloadStatus||runStatus||'completed':payloadStatus==='approval_required'&&runStatus&&runStatus!=='approval_required'?runStatus:payloadStatus||runStatus||'completed'
	const toolLive=status==='in_progress'&&(!sshTaskOperation||liveSSHTaskOwner)
	const program=request?fullProgram(request):''
	const script=request?textValue(request.script):''
	const change=jsonRecord(request?.change)||jsonRecord(payload.change)||jsonRecord(resultPayload?.change)
  const remotePath=request?(textValue(request.remote_path)||(!entry.tool?.startsWith('workspace_')?textValue(request.path):'')):''
	const workspaceID=textValue(display?.workspace_id)||(request?textValue(request.workspace_id):'')||textValue(shellPayload?.workspace_id)||textValue(payload.workspace_id)||textValue(resultPayload?.workspace_id)
	const relativePath=request?(textValue(request.relative_path)||(entry.tool?.startsWith('workspace_')?textValue(request.path):'')):''
	const unifiedFileRead=entry.tool==='ssh_file_read'||entry.tool==='workspace_file_read'
	const requestMode=(request?textValue(request.mode):'')||(unifiedFileRead?(request&&textValue(request.pattern)?`${entry.tool==='workspace_file_read'?'workspace':'remote'}_search`:`${entry.tool==='workspace_file_read'?'workspace':'remote'}_read`):'')
	const tunnelOperation=entry.tool==='ssh_tunnel'
	const tunnelAction=textValue(toolArguments?.action)||(requestMode==='ssh_tunnel_start'?'start':'')
	const tunnel=jsonRecord(payload.tunnel)||jsonRecord(resultPayload?.tunnel)
	const tunnelDirection=(request?textValue(request.direction):'')||textValue(tunnel?.direction)||textValue(toolArguments?.direction)||'local'
	const tunnelLocalHost=(request?textValue(request.local_host):'')||textValue(tunnel?.local_host)||textValue(toolArguments?.local_host)||'127.0.0.1'
	const tunnelRemoteHost=(request?textValue(request.remote_host):'')||textValue(tunnel?.remote_host)||textValue(toolArguments?.remote_host)
	const tunnelRemotePort=(request?numberValue(request.remote_port):0)||numberValue(tunnel?.remote_port)||numberValue(toolArguments?.remote_port)
	const tunnelLocalPort=(request?numberValue(request.local_port):0)||numberValue(tunnel?.local_port)||numberValue(toolArguments?.local_port)
	const tunnelRoute=tunnelAction==='start'?sshTunnelRoute(hostName,tunnelDirection,tunnelLocalHost,tunnelLocalPort,tunnelRemoteHost,tunnelRemotePort,t('tunnels.automaticPort')):tunnelAction==='stop'?textValue(toolArguments?.tunnel_id):''
	const shellTool=entry.tool==='ssh_shell'||entry.tool==='workspace_shell'
	const shellAction=textValue(toolArguments?.action)||(requestMode==='ssh_shell_start'||requestMode==='workspace_shell_start'?'start':requestMode==='workspace_shell'?'run':'')
	const shellOperation=shellTool&&shellAction!=='run'
	const shellID=textValue(toolArguments?.shell_id)||textValue(shellPayload?.id)
	const shellEvents=[...recordArray(payload.events,true),...recordArray(resultPayload?.events,true)]
	const shellChunks=[...recordArray(payload.chunks,true),...recordArray(resultPayload?.chunks,true)]
	const recentShellChunks=shellChunks.slice(-toolCollectionPreviewItems)
	const shellChunkStdout=recentShellChunks.filter(chunk=>textValue(chunk.stream)==='stdout').map(chunk=>previewText(textValue(chunk.content),toolOutputPreviewChars)).join('')
	const shellChunkStderr=recentShellChunks.filter(chunk=>textValue(chunk.stream)==='stderr').map(chunk=>previewText(textValue(chunk.content),toolOutputPreviewChars)).join('')
	const shellChunkOutput=recentShellChunks.map(chunk=>previewText(textValue(chunk.content),toolOutputPreviewChars)).join('')
	const shellHasMore=payload.has_more===true||resultPayload?.has_more===true
	const shellOutput=shellChunkOutput||textValue(payload.output)||textValue(resultPayload?.output)||textValue(payload.recent_output)||textValue(resultPayload?.recent_output)||shellEvents
		.filter(event=>['stdout','stderr'].includes(textValue(event.stream)))
		.slice(-toolCollectionPreviewItems)
		.map(event=>previewText(textValue(event.content),toolOutputPreviewChars))
		.join('')||entry.liveStdout||''
	const shellInput=textValue(toolArguments?.input)
	const shellInputDisplay=`${shellInput}${toolArguments?.submit===true&&!/[\r\n]$/.test(shellInput)?' ↵':''}`
	const shellActionName=shellAction?t(`sshShell.toolActions.${shellAction}`,{defaultValue:t('sshShell.short')}):t('sshShell.short')
	const shellActionLabel=entry.tool==='ssh_shell'?`SSH Shell · ${shellActionName}`:shellActionName
	const shellSummary=shellOperation?(shellAction==='start'
		?`${workspaceID?`${workspaceID}:${request?textValue(request.cwd)||'.':textValue(toolArguments?.cwd)||'.'}`:`${hostName}:${request?textValue(request.cwd)||'~':textValue(toolArguments?.cwd)||'~'}`} · PTY`
		:shellAction==='input'
			?shellInputDisplay
			:shellAction==='output'
				?`${latestOutput(shellOutput,1)||shellID}${shellHasMore?` · ${t('tool.moreOutput')}`:''}`
				:shellAction==='list'
					?String(numberValue(payload.count)||numberValue(resultPayload?.count))
					:shellID):''
	const shellPrimaryContent=shellAction==='input'?shellInputDisplay:shellAction==='output'?shellOutput:shellSummary
	const shellPrimaryAction=shellOperation&&shellAction==='input'
	const shellOutputAction=shellOperation&&shellAction==='output'
	const fileSearchMode=unifiedFileRead&&(requestMode==='remote_search'||requestMode==='workspace_search')
	const fileReadMode=unifiedFileRead&&(requestMode==='remote_read'||requestMode==='workspace_read')
	const structuredFileOperation=fileReadMode||fileSearchMode
	const searchPattern=request?textValue(request.search_pattern):''
	const searchResult=jsonRecord(payload.search)||jsonRecord(resultPayload?.search)
	const searchMatchMode=(request?textValue(request.search_match_mode):'')||textValue(searchResult?.match_mode)
	const searchMatchModeLabel=searchMatchMode==='literal'?t('tool.matchModeLiteral'):searchMatchMode==='regex'?t('tool.matchModeRegex'):searchMatchMode||'—'
	const searchFound=searchResult?.found===true
	const workspaceShellBackend=request?textValue(request.workspace_shell_backend):''
	const workspaceUpload=requestMode==='workspace_upload'||entry.tool==='workspace_file_upload'
	const workspaceDownload=requestMode==='workspace_download'||entry.tool==='workspace_file_download'
	const sshTransfer=requestMode==='ssh_file_transfer'||entry.tool==='ssh_file_transfer'
	const workspaceTool=!!entry.tool?.startsWith('workspace_')
	const sourceHostID=(request?textValue(request.source_host_id):'')||textValue(toolArguments?.source_host_id)
	const sourcePath=(request?textValue(request.source_path):'')||textValue(toolArguments?.source_path)
	const sourceHost=hostIdentity(hosts,sourceHostID)
	const sourceHostName=sourceHost.name||sourceHost.id
	const file=jsonRecord(payload.file)||jsonRecord(resultPayload?.file)
	const filePath=textValue(file?.path)||remotePath||relativePath
	const fileTarget=`${workspaceID?`${workspaceID}:`:''}${filePath}`
	const eventToolLabel=sshTaskOperation?t(sshTaskAction==='cancel'?'tool.taskCancel':'tool.taskStatus'):shellOperation?shellActionLabel:structuredFileOperation?t(fileSearchMode?(workspaceID?'toolNames.workspace_file_search_mode':'toolNames.ssh_file_search_mode'):(workspaceID?'toolNames.workspace_file_read':'toolNames.ssh_file_read')):toolLabel(entry.tool||'')
	const workspaceTransferRoute:WorkspaceTransferRouteValue|undefined=workspaceUpload
		?{source:{kind:'workspace',name:workspaceID,path:relativePath},destination:{kind:'host',name:hostName,path:remotePath}}
		:workspaceDownload
			?{source:{kind:'host',name:hostName,path:remotePath},destination:{kind:'workspace',name:workspaceID,path:relativePath}}
			:undefined
	const workspaceTransfer=!!workspaceTransferRoute
	const fileTransfer=workspaceTransfer||sshTransfer
	const transferSummary=tunnelRoute||shellSummary||(workspaceTransferRoute?`${workspaceTransferRoute.source.name}:${workspaceTransferRoute.source.path} → ${workspaceTransferRoute.destination.name}:${workspaceTransferRoute.destination.path}`:sshTransfer?`${sourceHostName}:${sourcePath} → ${hostName}:${remotePath}`:'')
  const planSteps=Array.isArray(payload.steps)?payload.steps.slice(0,toolCollectionPreviewItems).map(jsonRecord).filter((step):step is JsonRecord=>!!step):[]
  const planSummary=textValue(payload.goal)||textValue(planSteps.find(step=>textValue(step.status)==='in_progress'||textValue(step.status)==='blocked')?.title)
	const genericArgumentSummary=executionTool||sshTaskOperation?'':toolArgumentSummary(entry.tool,toolArguments)
	const webTool=entry.tool==='web_search'||entry.tool==='web_extract'
	const webSummary=webTool?textValue(payload.query):''
	const operation=filePath||(script?t('tool.shellScript'):program||genericArgumentSummary||eventToolLabel||t('tool.result'))
  const env=request?jsonRecord(request.env):undefined
	const rawStdout=shellOperation&&(shellAction==='input'||shellAction==='output')?(shellChunks.length?shellChunkStdout:shellOutput):textValue(payload.stdout)||textValue(resultPayload?.stdout)||entry.liveStdout||run?.stdout_redacted||''
	const stdout=change?cleanFileChangeOutput(rawStdout):rawStdout
	const workspaceDirectory=entry.tool==='workspace_file_list'
	  const stderr=(shellOperation&&shellChunks.length?shellChunkStderr:'')||textValue(payload.stderr)||textValue(resultPayload?.stderr)||entry.liveStderr||run?.stderr_redacted||run?.error||''
	const outputView=textValue(payload.output_view)||textValue(resultPayload?.output_view)
	const stdoutOmitted=numberValue(payload.stdout_omitted_bytes)||numberValue(resultPayload?.stdout_omitted_bytes)
	const stderrOmitted=numberValue(payload.stderr_omitted_bytes)||numberValue(resultPayload?.stderr_omitted_bytes)
	const waitDeadlineReached=payload.wait_deadline_reached===true||resultPayload?.wait_deadline_reached===true
	const transferTotal=entry.transferTotalBytes||0
	const transferred=Math.min(entry.transferredBytes||0,transferTotal)
	const transferPercent=transferTotal>0?Math.min(100,Math.round(transferred/transferTotal*100)):0
	const outputLabel=(label:string,omitted:number)=>omitted>0?`${label} · ${outputView.toUpperCase()} · ${t('tool.outputOmitted',{count:omitted})}`:label
		const previewStream=status==='in_progress'&&entry.liveOutput?entry.liveOutputStream:stdout?'stdout':stderr?'stderr':undefined
		const previewContent=status==='in_progress'&&entry.liveOutput?entry.liveOutput:previewStream==='stderr'?stderr:stdout
	  const outputPreview=status==='in_progress'?latestOutput(previewContent,1):''
		const commandSummary=transferSummary||(fileSearchMode?`${fileTarget} · ${searchMatchModeLabel} pattern=${JSON.stringify(searchPattern)}`:filePath)||program||(script?compactScript(script):'')||planSummary||(sshTaskOperation?sshTaskID:'')||genericArgumentSummary||webSummary||operation
	const summaryLabel=eventToolLabel||entry.tool||t('common.functions')
	const historyRuns=[...recordArray(payload.runs),...recordArray(resultPayload?.runs)].slice(0,toolCollectionPreviewItems)
	const historyHostIDs=[...new Set(historyRuns.map(item=>textValue(item.host_id)).filter(Boolean))]
	const listedHosts=[...recordArray(payload.hosts),...recordArray(resultPayload?.hosts)].slice(0,toolCollectionPreviewItems)
	const targets:ToolTarget[]=[]
	if(!fileTransfer&&workspaceTool&&workspaceID){
		targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(!fileTransfer&&hostID){
		targets.push({kind:'host',label:t('tool.targetHost'),name:destinationHost.name,id:hostID})
	}else if(!fileTransfer&&workspaceID){
		targets.push({kind:'workspace',label:t('common.workspace'),name:workspaceID})
	}else if(entry.tool==='ssh_history'&&historyHostIDs.length>0){
		for(const historyHostID of historyHostIDs.slice(0,3)){const historyHost=hostIdentity(hosts,historyHostID);targets.push({kind:'host',label:t('tool.historyHost'),name:historyHost.name,id:historyHost.id})}
		if(historyHostIDs.length>3)targets.push({kind:'scope',label:t('tool.historyHost'),name:t('tool.moreHosts',{count:historyHostIDs.length-3})})
	}else if(entry.tool==='ssh_host_list'){
		targets.push({kind:'scope',label:t('tool.scope'),name:t('tool.allHosts',{count:listedHosts.length||hosts.length})})
	}
  const instruction=textValue(payload.operator_instruction)||textValue(taskPayload?.operator_instruction)||textValue(resultPayload?.operator_instruction)
  const rawPayload={...payload};delete rawPayload._display
		const resultExitCode=resultPayload?.exit_code
	const exitCode=typeof payload.exit_code==='number'?payload.exit_code:typeof resultExitCode==='number'?resultExitCode:run?.exit_code??'—'
	const duration=formatDuration(payload.duration??resultPayload?.duration,run)
	const autoApproved=payload.auto_approved===true||resultPayload?.auto_approved===true||runAutoApproved(run)
	const permission=executionPermission(request,hosts,destinationHostID,...(sshTransfer?[sourceHostID]:[]))
	const purpose=request?textValue(request.reason):''
	const liveTaskStartedAt=textValue(currentLiveSSHTask?.task?.started_at)
	const persistedStartedAt=run?.started_at?Date.parse(run.started_at):liveTaskStartedAt?Date.parse(liveTaskStartedAt):entry.startedAt
	return{
		entry,sshTaskOperation,status,summaryLabel,sshTransfer,sourceHostName,sourcePath,hostName,
		remotePath,workspaceTransferRoute,targets,commandSummary,autoApproved,request,permission,purpose,
		toolLive,outputPreview,previewStream,shellAction,shellActionLabel,shellPrimaryAction,shellPrimaryContent,shellOutputAction,
		shellChunks,shellOutput,executionTool,toolArguments,shellOperation,tunnelOperation,structuredFileOperation,fileSearchMode,
		filePath,script,workspaceShellBackend,shellSummary,tunnelRoute,requestMode,workspaceUpload,workspaceDownload,
		workspaceID,relativePath,fileTarget,program,change,env,webTool,payload,
		searchResult,searchFound,searchMatchModeLabel,searchPattern,instruction,fileTransfer,transferTotal,transferred,
		transferPercent,workspaceDirectory,stdout,outputLabel,stdoutOmitted,stderr,stderrOmitted,fileReadMode,
		exitCode,duration,waitDeadlineReached,shellHasMore,runID,rawPayload,
		startedAt:Number.isFinite(persistedStartedAt)?persistedStartedAt:undefined
	}
}

export type ToolEventView=ReturnType<typeof buildToolEventView>
