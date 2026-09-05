import i18n from '../../lib/i18n'
import type { Host, Run } from '../../types'
import { jsonRecord, previewText, textValue, toolCollectionPreviewItems, toolOutputPreviewChars, type JsonRecord } from './payload'

export function requestFromRun(run?:Run):JsonRecord|undefined{if(!run)return;try{return jsonRecord(JSON.parse(run.request_json))}catch{return}}
function shellArg(value:string){return /^[A-Za-z0-9_@%+=:,./-]+$/.test(value)?value:JSON.stringify(value)}
export function fullProgram(request:JsonRecord,full=false){
	const program=full?textValue(request.program):previewText(textValue(request.program))
	const source=Array.isArray(request.args)?request.args:[]
	const selected=full?source:source.slice(0,toolCollectionPreviewItems)
	const args=selected.map(value=>full?String(value):previewText(String(value)))
	if(!full&&source.length>selected.length)args.push(i18n.t('tool.previewItemsOmitted',{count:source.length-selected.length}))
	const command=[program,...args].filter(Boolean).map(shellArg).join(' ')
	return full?command:previewText(command,toolOutputPreviewChars)
}
export function hostIdentity(hosts:Host[],hostID:string){
	const host=hosts.find(item=>item.id===hostID||item.name===hostID)
	return {name:host?.name||'',id:host?.id||hostID,user:host?.user||''}
}
export function executionPermission(request:JsonRecord|undefined,hosts:Host[],...hostIDs:string[]){
	if(request?.elevated===true)return'root'
	return hostIDs.some(hostID=>hosts.find(host=>host.id===hostID||host.name===hostID)?.user.trim().toLowerCase()==='root')?'root':'user'
}
