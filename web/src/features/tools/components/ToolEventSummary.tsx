import { useContext, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, FolderOpen, ListChecks, LoaderCircle, Server, ShieldCheck } from 'lucide-react'
import { useDocumentVisible } from '../../../lib/hooks'
import { formatFileSize } from '../../../lib/utils'
import { ChatVisibilityContext } from '../../chat/ChatVisibilityContext'
import { previewText } from '../payload'
import type { ToolEventView } from '../toolEvent'
import { ToolSummaryIcon } from './ToolSummaryIcon'
import { ToolTransferRoute, WorkspaceTransferRoute } from './ToolTransferRoute'

export function ToolEventSummary({view,loadingDetail,startedAt,toggleExpanded}:{view:ToolEventView;loadingDetail:boolean;startedAt:number;toggleExpanded:(summary:HTMLElement)=>void}){
	const {t}=useTranslation()
	const {
		entry,sshTaskOperation,status,summaryLabel,sshTransfer,sourceHostName,sourcePath,hostName,
		remotePath,workspaceTransferRoute,targets,commandSummary,autoApproved,request,permission,purpose,
		toolLive,outputPreview,previewStream,shellAction,shellActionLabel
	}=view
	return <summary onClick={event=>{event.preventDefault();toggleExpanded(event.currentTarget)}}><div className="tool-summary-icon"><ToolSummaryIcon name={entry.tool}/></div><div className="tool-summary-copy"><div className="tool-summary-heading"><b>{summaryLabel}</b>{sshTransfer?<ToolTransferRoute sourceHost={sourceHostName} sourcePath={sourcePath} destinationHost={hostName} destinationPath={remotePath}/>:workspaceTransferRoute?<WorkspaceTransferRoute {...workspaceTransferRoute}/>:<>{targets.length>0&&<div className="tool-summary-targets">{targets.map((target,index)=><span className={`tool-target-chip tool-target-${target.kind}`} title={`${target.label}: ${[target.name,target.id].filter(Boolean).join(' · ')}`} key={`${target.kind}_${target.id||target.name}_${index}`}>{target.kind==='host'?<Server size={11}/>:target.kind==='workspace'?<FolderOpen size={11}/>:<ListChecks size={11}/>} {(targets.length>1||target.kind==='scope')&&<em>{target.label}</em>}<b>{target.name||target.id}</b></span>)}</div>}{commandSummary!==summaryLabel&&<code className={sshTaskOperation?'tool-task-id':undefined} title={previewText(commandSummary)}>{previewText(commandSummary)}</code>}</>}</div></div><div className="tool-summary-statuses">{loadingDetail&&<LoaderCircle className="spin" size={12}/>} {autoApproved&&<span className="auto-approved"><ShieldCheck size={11}/>{t('approval.autoApproved')}</span>}<span className={`tool-status ${status}`} key={status}>{t(`statusLabels.${status}`,{defaultValue:status.replaceAll('_',' ')})}</span></div><ChevronRight className="tool-summary-chevron" size={14}/>{request&&<div className="tool-summary-meta"><span className={`tool-summary-permission ${permission}`}><em>{t('tool.permission')}</em><b>{permission}</b></span>{purpose&&<span className="tool-summary-purpose" title={purpose}><em>{t('tool.reason')}</em><b>{purpose}</b></span>}</div>}{toolLive&&<ToolLiveProgress view={view} startedAt={startedAt}/>}{outputPreview&&<div className={`tool-summary-preview ${previewStream==='stderr'?'stderr':''}`}><span>{shellAction==='output'?shellActionLabel:(previewStream||'stdout').toUpperCase()}</span><pre>{outputPreview}</pre></div>}</summary>
}

function ToolLiveProgress({view,startedAt}:{view:ToolEventView;startedAt:number}){
	const {t}=useTranslation()
	const {transferTotal,transferred,transferPercent,sshTaskOperation,entry}=view
	const chatVisible=useContext(ChatVisibilityContext)
	const documentVisible=useDocumentVisible()
	const [now,setNow]=useState(Date.now)
	useEffect(()=>{
		if(!chatVisible||!documentVisible)return
		const timer=window.setInterval(()=>setNow(Date.now()),1000)
		return()=>window.clearInterval(timer)
	},[chatVisible,documentVisible])
	const elapsed=formatLiveDuration(Math.max(0,Math.floor((now-startedAt)/1000)))
	return <div className={`tool-live-progress ${transferTotal>0?'determinate':''}`} role="progressbar" aria-valuemin={transferTotal>0?0:undefined} aria-valuemax={transferTotal>0?transferTotal:undefined} aria-valuenow={transferTotal>0?transferred:undefined}><i><em style={transferTotal>0?{width:`${transferPercent}%`}:undefined}/></i><span>{transferTotal>0?`${formatFileSize(transferred)} / ${formatFileSize(transferTotal)}`:sshTaskOperation?t('tool.liveTask'):entry.liveOutputStream?.toUpperCase()||''}</span><time>{elapsed}</time></div>
}

function formatLiveDuration(seconds:number){
	if(seconds<60)return`${seconds}s`
	const minutes=Math.floor(seconds/60)
	return`${minutes}m ${String(seconds%60).padStart(2,'0')}s`
}
