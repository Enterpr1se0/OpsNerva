import i18n from '../../lib/i18n'

export type JsonRecord = Record<string,unknown>
const toolValuePreviewChars=8<<10
export const toolOutputPreviewChars=128<<10
export const toolDiffPreviewChars=128<<10
export const toolCollectionPreviewItems=100
const toolStructuredParseChars=512<<10
export function previewText(value:string,limit=toolValuePreviewChars){
	if(value.length<=limit)return value
	const edge=Math.max(1,Math.floor(limit/2))
	return `${value.slice(0,edge)}\n… ${i18n.t('tool.previewOmitted',{count:value.length-edge*2})} …\n${value.slice(-edge)}`
}
export function jsonRecord(value:unknown):JsonRecord|undefined{return value!==null&&typeof value==='object'&&!Array.isArray(value)?value as JsonRecord:undefined}
export function limitedRecordEntries(value:JsonRecord,limit=toolCollectionPreviewItems){
	const entries:Array<[string,unknown]>=[]
	let truncated=false
	for(const key in value){
		if(!Object.prototype.hasOwnProperty.call(value,key))continue
		if(entries.length>=limit){truncated=true;break}
		entries.push([key,value[key]])
	}
	return{entries,truncated}
}
export function hasRecordEntries(value:JsonRecord){for(const key in value)if(Object.prototype.hasOwnProperty.call(value,key))return true;return false}
export function previewStructuredValue(value:unknown,depth=0):unknown{
	if(typeof value==='string')return previewText(value)
	if(value===null||typeof value!=='object')return value
	if(depth>=4)return'…'
	if(Array.isArray(value)){
		const visible=value.slice(0,toolCollectionPreviewItems).map(item=>previewStructuredValue(item,depth+1))
		if(value.length>visible.length)visible.push(i18n.t('tool.previewItemsOmitted',{count:value.length-visible.length}))
		return visible
	}
	const {entries,truncated}=limitedRecordEntries(value as JsonRecord)
	const result=Object.fromEntries(entries.map(([key,item])=>[key,previewStructuredValue(item,depth+1)]))
	if(truncated)result['…']=i18n.t('tool.moreItemsOmitted')
	return result
}
export function parseRecord(value:string):JsonRecord{
	if(value.length>toolStructuredParseChars){
		const envelope=value.slice(0,toolValuePreviewChars)
		const runID=envelope.match(/"run_id"\s*:\s*"([^"\\]+)"/)?.[1]
		const status=envelope.match(/"status"\s*:\s*"([^"\\]+)"/)?.[1]
		return{...(runID?{run_id:runID}:{}),...(status?{status}:{}),output_limited:true,original_chars:value.length,preview:previewText(value,toolOutputPreviewChars)}
	}
	try{const parsed=JSON.parse(value);return jsonRecord(parsed)||{value:parsed}}catch{return{value:previewText(value,toolOutputPreviewChars)}}
}
export function textValue(value:unknown){return typeof value==='string'?value:''}
