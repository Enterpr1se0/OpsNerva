import { useEffect, useMemo, useRef } from 'react'
import { useTranslation } from 'react-i18next'
import { FileText, FolderOpen } from 'lucide-react'
import { CopyButton } from '../../../components/CopyButton'
import { HighlightedCode } from '../../../components/HighlightedCode'
import { formatFileSize } from '../../../lib/utils'
import { jsonRecord, previewText, textValue, toolCollectionPreviewItems, toolOutputPreviewChars } from '../payload'

type ToolWorkspaceDirectoryEntry={name:string;type:'file'|'directory';size?:number}
function parseWorkspaceDirectoryOutput(value:string):ToolWorkspaceDirectoryEntry[]|undefined{
	if(!value)return
	try{
		const result=jsonRecord(JSON.parse(value))
		if(!result||!Array.isArray(result.entries))return
		const entries:ToolWorkspaceDirectoryEntry[]=[]
		for(const value of result.entries){
			const entry=jsonRecord(value),name=textValue(entry?.name),type=textValue(entry?.type)
			if(!entry||!name||(type!=='file'&&type!=='directory')||(entry.size!==undefined&&(typeof entry.size!=='number'||!Number.isFinite(entry.size))))return
			entries.push({name,type,size:typeof entry.size==='number'?entry.size:undefined})
		}
		return entries
	}catch{return}
}

export function ToolOutputPanel({kind,label,content,live,language}:{kind:'stdout'|'stderr';label:string;content:string;live:boolean;language?:string}){
	const outputRef=useRef<HTMLPreElement>(null)
	const stickToBottom=useRef(true)
	const preview=previewText(content,toolOutputPreviewChars)
	useEffect(()=>{
		const output=outputRef.current
		if(live&&output&&stickToBottom.current)output.scrollTop=output.scrollHeight
	},[preview,live])
	return <div className={`tool-output ${kind} ${live?'live':''}`}><span>{label}</span><div className="tool-output-frame"><CopyButton value={content}/><pre ref={outputRef} onScroll={event=>{const output=event.currentTarget;stickToBottom.current=output.scrollHeight-output.scrollTop-output.clientHeight<32}}><HighlightedCode code={preview} language={language} autoDetect live={live}/></pre></div></div>
}

export function WorkspaceDirectoryOutput({content,label,live}:{content:string;label:string;live:boolean}){
	const {t}=useTranslation()
	const entries=useMemo(()=>parseWorkspaceDirectoryOutput(content),[content])
	if(entries===undefined)return <ToolOutputPanel kind="stdout" label={label} content={content} live={live}/>
	const visible=entries.slice(0,toolCollectionPreviewItems),omitted=entries.length-visible.length
	return <div className="tool-output workspace-directory-output"><span>STDOUT</span><div className="tool-output-frame"><CopyButton value={content}/><div className="tool-directory-list">{visible.length?visible.map(entry=><div className={`tool-directory-entry ${entry.type}`} key={`${entry.type}:${entry.name}`}>{entry.type==='directory'?<FolderOpen size={14}/>:<FileText size={14}/>}<b title={entry.name}>{entry.name}</b><small>{entry.type==='directory'?t('workspace.directory'):formatFileSize(entry.size||0)}</small></div>):<div className="tool-directory-empty">{t('workspace.emptyDirectory')}</div>}{omitted>0&&<div className="tool-directory-omitted">{t('tool.previewItemsOmitted',{count:omitted})}</div>}</div></div></div>
}
