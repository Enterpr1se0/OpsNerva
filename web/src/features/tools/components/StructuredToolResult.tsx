import i18n from '../../../lib/i18n'
import { displayValue, jsonRecord, limitedRecordEntries, toolCollectionPreviewItems, type JsonRecord } from '../payload'
import { CompactTable } from './CompactTable'

export function GenericToolResult({payload}:{payload:JsonRecord}){
  const hidden=new Set(['_display','stdout','stderr','operator_instruction','ok','status','code','message','next_action','run_id','duration','exit_code','auto_approved','tasks'])
	const entries=limitedRecordEntries(payload).entries.filter(([key])=>!hidden.has(key))
	if(!entries.length)return null
  const scalars=entries.filter(([,value])=>value===null||typeof value==='string'||typeof value==='number'||typeof value==='boolean')
  const arrays=entries.filter(([,value])=>Array.isArray(value))
  const objects=entries.filter(([,value])=>!!jsonRecord(value))
  return <div className="tool-structured-result">
    {scalars.length>0&&<dl className="tool-generic-grid">{scalars.map(([key,value])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{displayValue(value)}</dd></div>)}</dl>}
    {arrays.map(([key,value])=><StructuredArray key={key} label={key} values={value as unknown[]}/>)}
    {objects.map(([key,value])=><StructuredObject key={key} label={key} value={value as JsonRecord}/>)}
  </div>
}

function StructuredArray({label,values}:{label:string;values:unknown[]}){
	const visible=values.slice(0,toolCollectionPreviewItems)
  const records=visible.map(jsonRecord).filter((item):item is JsonRecord=>!!item)
	if(records.length===visible.length&&records.length>0){const columns=[...new Set(records.flatMap(record=>Object.keys(record)))].slice(0,10);return <CompactTable title={`${label.replaceAll('_',' ')} · ${values.length} ITEMS`} columns={columns.map(column=>column.replaceAll('_',' '))} rows={records.map(record=>columns.map(column=>record[column]))}/>}
	return <div className="tool-array-section"><span>{label.replaceAll('_',' ')}</span><div>{visible.map((value,index)=><code key={index}>{displayValue(value)}</code>)}{values.length>visible.length&&<code>{i18n.t('tool.previewItemsOmitted',{count:values.length-visible.length})}</code>}</div></div>
}

function StructuredObject({label,value}:{label:string;value:JsonRecord}){
	const {entries,truncated}=limitedRecordEntries(value)
  return <section className="tool-object-section"><h4>{label.replaceAll('_',' ')}</h4><dl className="tool-generic-grid">{entries.map(([key,item])=><div key={key}><dt>{key.replaceAll('_',' ')}</dt><dd>{displayValue(item)}</dd></div>)}{truncated&&<div><dt>…</dt><dd>{i18n.t('tool.moreItemsOmitted')}</dd></div>}</dl></section>
}
