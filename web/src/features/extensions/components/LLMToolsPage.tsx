import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Braces, ChevronRight, FunctionSquare, LoaderCircle, Power, RefreshCw, Search, ShieldAlert, X } from 'lucide-react'
import { api } from '../../../api/api'
import { CopyablePre } from '../../../components/CopyButton'
import { HighlightedCode } from '../../../components/HighlightedCode'
import { AppSelect } from '../../../components/Controls'
import i18n from '../../../lib/i18n'
import { errorText } from '../../../lib/utils'
import type { LLMToolCatalog, LLMToolDescriptor, LLMToolGuard } from '../../../types'
import { Empty } from '../../../components/PageLayout'

type ToolParameterView = {name:string;type:string;description:string;required:boolean}

function toolCategoryLabel(value:string){return i18n.t(`toolCategories.${value}`,{defaultValue:value})}
function toolGuardLabel(value:LLMToolGuard){return i18n.t(`toolGuards.${value}`,{defaultValue:value})}

function schemaRecord(value:unknown):Record<string,unknown>{return value!==null&&typeof value==='object'&&!Array.isArray(value)?value as Record<string,unknown>:{}}
function schemaType(value:unknown){if(Array.isArray(value))return value.map(String).join(' | ');return typeof value==='string'?value:'any'}
function toolParameters(tool?:LLMToolDescriptor):ToolParameterView[]{
	if(!tool)return[]
	const schema=schemaRecord(tool.input_schema)
	const properties=schemaRecord(schema.properties)
	const required=new Set(Array.isArray(schema.required)?schema.required.map(String):[])
	return Object.entries(properties).map(([name,value])=>{const field=schemaRecord(value);return{name,type:schemaType(field.type),description:typeof field.description==='string'?field.description:'',required:required.has(name)}})
}

export function LLMToolsPage({catalog,refresh,onCatalogChanged}:{catalog:LLMToolCatalog|null;refresh:()=>Promise<void>;onCatalogChanged:(catalog:LLMToolCatalog)=>void}){
	const {t}=useTranslation()
	const [query,setQuery]=useState('')
	const [category,setCategory]=useState('all')
	const [selectedName,setSelectedName]=useState('')
	const [refreshing,setRefreshing]=useState(false)
	const [busyName,setBusyName]=useState('')
	const [error,setError]=useState('')
	const tools=catalog?.tools||[]
	const categories=useMemo(()=>Array.from(new Set(tools.map(tool=>tool.category))),[tools])
	const filtered=useMemo(()=>{const needle=query.trim().toLowerCase();return tools.filter(tool=>(category==='all'||tool.category===category)&&(!needle||`${tool.name} ${tool.description} ${tool.category}`.toLowerCase().includes(needle)))},[tools,query,category])
	const selected=filtered.find(tool=>tool.name===selectedName)||filtered[0]
	const parameters=toolParameters(selected)
	const refreshCatalog=async()=>{setRefreshing(true);try{await refresh()}finally{setRefreshing(false)}}
	const setEnabled=async(tool:LLMToolDescriptor)=>{setBusyName(tool.name);setError('');try{onCatalogChanged(await api.setLLMToolEnabled(tool.name,!tool.enabled))}catch(err){setError(errorText(err))}finally{setBusyName('')}}

	return <div className="llm-tools-page page-stack">
		{error&&<div className="tool-function-error"><ShieldAlert size={15}/><span>{error}</span><button onClick={()=>setError('')} title={t('common.dismiss')}><X size={14}/></button></div>}
		<div className="tool-catalog-toolbar panel"><label><Search size={15}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('tools.searchPlaceholder')}/></label><AppSelect value={category} ariaLabel={t('tools.category')} onChange={setCategory} options={[{value:'all',label:t('tools.allCategories',{count:tools.length})},...categories.map(value=>({value,label:`${toolCategoryLabel(value)} · ${tools.filter(tool=>tool.category===value).length}`}))]}/><span>{t('tools.visible',{count:filtered.length})}</span><button className="tool-catalog-refresh" onClick={refreshCatalog} disabled={refreshing} title={t('tools.refreshSnapshot')}><RefreshCw className={refreshing?'spin':''} size={14}/><span>{refreshing?t('common.refreshing'):t('tools.refreshSnapshot')}</span></button></div>
		{!catalog?<div className="tool-catalog-loading panel"><LoaderCircle className="spin" size={20}/>{t('tools.loadingSnapshot')}</div>:!catalog.loaded?<Empty icon={<FunctionSquare/>} title={t('tools.runtimeMissing')} text={t('tools.runtimeMissingText')}/>:<div className="tool-catalog-browser">
			<section className="tool-function-list panel">{filtered.length?filtered.map(tool=>{const count=toolParameters(tool).length;return <button className={`${selected?.name===tool.name?'active':''} ${tool.enabled?'':'disabled'}`} onClick={()=>setSelectedName(tool.name)} key={tool.name}><div className="tool-function-icon"><Braces size={16}/></div><span><code>{tool.name}</code><p>{tool.description}</p><small><em>{toolCategoryLabel(tool.category)}</em><i className={tool.guard}>{toolGuardLabel(tool.guard)}</i>{!tool.enabled&&<i className="disabled">{t('tools.disabled')}</i>}</small></span><b>{count}<small>{t('tools.argsUnit')}</small></b><ChevronRight size={14}/></button>}):<div className="tool-filter-empty"><Search size={20}/><b>{t('tools.noMatch')}</b></div>}</section>
			<aside className={`tool-function-inspector panel ${selected?.enabled?'':'disabled'}`}>{selected?<><header><div className="tool-function-icon"><FunctionSquare size={18}/></div><span><small>{t('tools.functionDetail')}</small><code>{selected.name}</code></span><div className="tool-function-controls"><em className={selected.guard}>{toolGuardLabel(selected.guard)}</em><button className={selected.enabled?'enabled':''} role="switch" aria-checked={selected.enabled} onClick={()=>void setEnabled(selected)} disabled={busyName===selected.name} title={selected.enabled?t('tools.disableFunction'):t('tools.enableFunction')}>{busyName===selected.name?<LoaderCircle className="spin" size={14}/>:<Power size={14}/>}<span>{selected.enabled?t('common.enabled'):t('common.disabled')}</span></button></div></header><p className="tool-function-description">{selected.description}</p><dl className="tool-function-meta"><div><dt>{t('tools.category')}</dt><dd>{toolCategoryLabel(selected.category)}</dd></div><div><dt>{t('common.arguments')}</dt><dd>{parameters.length}</dd></div><div><dt>{t('tools.safetyGate')}</dt><dd>{toolGuardLabel(selected.guard)}</dd></div></dl><section className="tool-parameter-list"><h3>{t('tools.inputParameters')} <span>{t('tools.requiredCount',{count:parameters.filter(item=>item.required).length})}</span></h3>{parameters.length?parameters.map(parameter=><div key={parameter.name}><code>{parameter.name}</code><em>{parameter.type}</em>{parameter.required&&<b>{t('common.required')}</b>}{parameter.description&&<p>{parameter.description}</p>}</div>):<p className="tool-no-arguments">{t('tools.noArguments')}</p>}</section><details className="tool-schema-raw"><summary>{t('tools.rawSchema')} <ChevronRight size={13}/></summary><CopyablePre><HighlightedCode code={JSON.stringify(selected.input_schema,null,2)} language="json"/></CopyablePre></details></>:<div className="tool-inspector-empty"><Braces size={26}/></div>}</aside>
		</div>}
	</div>
}
