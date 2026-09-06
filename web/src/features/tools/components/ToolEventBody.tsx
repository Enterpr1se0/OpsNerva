import { memo } from 'react'
import { useTranslation } from 'react-i18next'
import { Search, ShieldAlert, TerminalSquare } from 'lucide-react'
import { CopyButton } from '../../../components/CopyButton'
import { HighlightedCode } from '../../../components/HighlightedCode'
import { inferScriptLanguage, languageFromPath } from '../../../lib/codeLanguage'
import { formatFileSize } from '../../../lib/utils'
import { hasRecordEntries, limitedRecordEntries, numberValue, previewText, recordTableRows, textValue, toolOutputPreviewChars } from '../payload'
import { fullProgram } from '../request'
import { safeToolArgument } from '../summary'
import type { ToolEventView } from '../toolEvent'
import { CompactTable } from './CompactTable'
import { DiffViewer } from './DiffViewer'
import { GenericToolResult } from './StructuredToolResult'
import { LazyJSONDetails, ShellOutputChunks, ToolOutputPanel, WorkspaceDirectoryOutput } from './ToolOutput'
import { WebToolResult } from './WebToolResult'

export const ToolEventBody=memo(function ToolEventBody({view,detailError}:{view:ToolEventView;detailError:string}){
	const {t}=useTranslation()
	const {
		shellPrimaryAction,shellPrimaryContent,shellOutputAction,shellChunks,shellOutput,executionTool,toolArguments,request,
		shellOperation,tunnelOperation,structuredFileOperation,fileSearchMode,filePath,script,workspaceShellBackend,shellSummary,
		tunnelRoute,requestMode,workspaceUpload,workspaceDownload,workspaceID,relativePath,hostName,remotePath,
		sshTransfer,sourceHostName,sourcePath,fileTarget,program,commandSummary,change,env,
		sshTaskOperation,webTool,entry,payload,searchResult,searchFound,searchMatchModeLabel,searchPattern,
		status,instruction,fileTransfer,transferTotal,transferred,transferPercent,workspaceDirectory,stdout,
		outputLabel,stdoutOmitted,stderr,stderrOmitted,toolLive,fileReadMode,exitCode,duration,
		waitDeadlineReached,shellHasMore,runID,rawPayload,shellActionLabel
	}=view
	return <div className="tool-event-body">
		  {detailError&&<div className="tool-detail-error">{detailError}</div>}
		  {shellPrimaryAction&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellPrimaryContent||'—'}/><pre><HighlightedCode code={previewText(shellPrimaryContent||'—',toolOutputPreviewChars)} language={inferScriptLanguage(shellPrimaryContent||'')}/></pre></div></section>}
		  {shellOutputAction&&!shellChunks.length&&<section className="tool-command-pane"><div className="tool-command-head"><span>{shellActionLabel}</span></div><div className="tool-command-block"><CopyButton value={shellOutput||'—'}/><pre><HighlightedCode code={previewText(shellOutput||'—',toolOutputPreviewChars)} autoDetect live={toolLive}/></pre></div></section>}
		  {!executionTool&&!sshTaskOperation&&toolArguments&&hasRecordEntries(toolArguments)&&<CompactTable title={t('tool.actualParameters')} columns={[t('tool.parameter'),t('tool.value')]} rows={limitedRecordEntries(toolArguments).entries.map(([key,value])=>[key,safeToolArgument(value,key)])}/>}
      {request?<section className="tool-command-pane">
		  <div className="tool-command-head"><span>{shellOperation?t('sshShell.title'):tunnelOperation?t('tunnels.forwarding'):structuredFileOperation?t(fileSearchMode?'tool.searchOperation':'tool.readOperation'):filePath?t('tool.fileOperation'):script?t('tool.fullScript'):t('tool.fullCommand')}</span>{workspaceShellBackend&&<em><TerminalSquare size={12}/>{workspaceShellBackend==='host'?t('approval.hostShell'):'Bubblewrap'}</em>}</div>
			  <div className="tool-command-block"><CopyButton value={script||(program?()=>fullProgram(request,true):commandSummary)}/>{shellOperation?<pre>{shellSummary}</pre>:tunnelOperation?<pre>{tunnelRoute||requestMode}</pre>:workspaceUpload?<pre>workspace_upload {workspaceID}:{relativePath} → {hostName}:{remotePath}</pre>:workspaceDownload?<pre>workspace_download {hostName}:{remotePath} → {workspaceID}:{relativePath}</pre>:sshTransfer?<pre>{sourceHostName}:{sourcePath} → {hostName}:{remotePath}</pre>:structuredFileOperation?<pre>{fileSearchMode?'search':'read'} {fileTarget}</pre>:filePath?<pre>{requestMode} {workspaceID?`${workspaceID}:`:''}{filePath}</pre>:script?<pre><HighlightedCode code={previewText(script,toolOutputPreviewChars)} language={inferScriptLanguage(script)}/></pre>:program?<pre><span className="prompt-sign">$</span> <HighlightedCode code={previewText(program)} language="bash"/></pre>:<pre>{requestMode} {remotePath}</pre>}</div>
		  {change&&textValue(change.diff)&&<DiffViewer change={change}/>}
		  {env&&hasRecordEntries(env)&&<CompactTable title={t('tool.environment')} columns={[t('tool.key'),t('tool.value')]} rows={recordTableRows(env)}/>}
      </section>:!sshTaskOperation&&!shellPrimaryAction&&!shellOutputAction&&(webTool?<WebToolResult tool={entry.tool!} payload={payload}/>:<GenericToolResult payload={payload}/>)}
	  {fileSearchMode&&searchResult&&<div className={`file-search-result ${searchFound?'found':'empty'}`}><Search size={15}/><div><b>{t(searchFound?'tool.searchMatched':'tool.searchNoMatches')}</b><span>{searchMatchModeLabel} · {searchPattern}</span></div></div>}
	  {(textValue(payload.message)||textValue(payload.next_action))&&<div className={`tool-guidance ${payload.ok===false||['failed','denied','interrupted'].includes(status)?'error':''}`}><ShieldAlert size={15}/><div><b>{textValue(payload.code)||t('tool.result')}</b>{textValue(payload.message)&&<p>{textValue(payload.message)}</p>}{textValue(payload.next_action)&&<small>{t('common.next')} · {textValue(payload.next_action)}</small>}</div></div>}
	  {instruction&&<div className="tool-instruction"><ShieldAlert size={15}/><div><b>{t('tool.operatorInstruction')}</b><p>{instruction}</p></div></div>}
	  {fileTransfer&&transferTotal>0&&<div className="file-transfer-progress" role="progressbar" aria-valuemin={0} aria-valuemax={transferTotal} aria-valuenow={transferred}><div><span>{t('tool.transferProgress')}</span><b>{formatFileSize(transferred)} / {formatFileSize(transferTotal)}</b></div><i><em style={{width:`${transferPercent}%`}}/></i></div>}
	  {workspaceDirectory&&stdout?<div className="tool-output-grid"><WorkspaceDirectoryOutput content={stdout} label={outputLabel('STDOUT',stdoutOmitted)} live={toolLive}/>{stderr&&<ToolOutputPanel kind="stderr" label={outputLabel(t('tool.stderrResult'),stderrOmitted)} content={stderr} live={toolLive}/>}</div>:shellOperation&&shellChunks.length>0?<ShellOutputChunks chunks={shellChunks} live={toolLive}/>:((stdout&&(!shellOutputAction||shellChunks.length>0))||stderr)&&<div className="tool-output-grid">{stdout&&(!shellOutputAction||shellChunks.length>0)&&<ToolOutputPanel kind="stdout" label={outputLabel('STDOUT',stdoutOmitted)} content={stdout} live={toolLive} language={fileReadMode?languageFromPath(filePath):undefined}/>} {stderr&&<ToolOutputPanel kind="stderr" label={outputLabel(t('tool.stderrResult'),stderrOmitted)} content={stderr} live={toolLive}/>}</div>}
	  {(request||sshTaskOperation)&&(exitCode!=='—'||duration!=='—'||waitDeadlineReached||shellHasMore||sshTaskOperation&&(!!runID||numberValue(payload.stdout_total_bytes)>0||numberValue(payload.stderr_total_bytes)>0))&&<aside className="tool-context-pane"><dl className="tool-context-grid">{sshTaskOperation&&runID&&<div><dt>{t('tool.runId')}</dt><dd>{runID}</dd></div>}{exitCode!=='—'&&<div><dt>{t('tool.exitCode')}</dt><dd>{exitCode}</dd></div>}{duration!=='—'&&<div><dt>{t('tool.duration')}</dt><dd>{duration}</dd></div>}{sshTaskOperation&&numberValue(payload.stdout_total_bytes)>0&&<div><dt>STDOUT</dt><dd>{formatFileSize(numberValue(payload.stdout_total_bytes))}</dd></div>}{sshTaskOperation&&numberValue(payload.stderr_total_bytes)>0&&<div><dt>STDERR</dt><dd>{formatFileSize(numberValue(payload.stderr_total_bytes))}</dd></div>}{(waitDeadlineReached||shellHasMore)&&<div><dt>{t('common.status')}</dt><dd>{waitDeadlineReached?t('tool.waitDeadline'):t('tool.moreOutput')}</dd></div>}</dl></aside>}
	  <LazyJSONDetails value={rawPayload}/>
    </div>
})
