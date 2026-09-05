import { useState } from 'react'
import { CircleDot, Download, FileText, RefreshCw, Search, ShieldAlert } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { AppSelect } from '../../components/Controls'
import { Empty } from '../../components/PageLayout'
import { useDebouncedValue } from '../../lib/hooks'
import { LogEntries } from './LogEntries'
import { useLogData } from './useLogData'

export function LogsPage(){
	const {t}=useTranslation()
	const [level,setLevel]=useState('debug')
	const [component,setComponent]=useState('')
	const [query,setQuery]=useState('')
	const [live,setLive]=useState(true)
	const search=useDebouncedValue(query,250)
	const {rows,components,minimumLevel,file,loading,error,refresh}=useLogData(level,component,search,live)
	return <div className="logs-page page-stack">
		<div className="logs-toolbar panel">
			<div className="search-box"><Search size={16}/><input value={query} onChange={event=>setQuery(event.target.value)} placeholder={t('logs.search')}/></div>
			<label><span>{t('logs.minimumLevel')}</span><AppSelect value={level} ariaLabel={t('logs.minimumLevel')} onChange={setLevel} options={['debug','info','warn','error'].map(value=>({value,label:value==='error'?'Error':`${value[0].toUpperCase()}${value.slice(1)}+`}))}/></label>
			<label><span>{t('logs.component')}</span><AppSelect value={component} ariaLabel={t('logs.component')} onChange={setComponent} options={[{value:'',label:t('logs.allComponents')},...components.map(value=>({value,label:value}))]}/></label>
			<button className={`live-toggle ${live?'active':''}`} onClick={()=>setLive(value=>!value)}><CircleDot size={13}/>{t(live?'logs.live':'logs.paused')}</button>
			<button className="log-refresh" onClick={refresh} disabled={loading}><RefreshCw size={14} className={loading?'spin':''}/>{t(loading?'common.loading':'common.refresh')}</button>
			<a className="log-export" href="/api/v1/logs/export" download><Download size={14}/>{t('logs.export')}</a>
		</div>
		<div className="logs-meta"><span>{t('logs.entries',{count:rows.length})}</span><span>{file?t('logs.file',{file}):t('logs.fileDisabled')}</span></div>
		{minimumLevel!=='debug'&&level==='debug'&&<div className="log-hint"><ShieldAlert size={15}/><span>{t('logs.debugHint')}</span></div>}
		{error&&<div className="history-error panel">{error}</div>}
		<div className="log-stream panel">
			<div className="log-row log-head"><span>{t('logs.columns.time')}</span><span>{t('logs.columns.level')}</span><span>{t('logs.columns.component')}</span><span>{t('logs.columns.event')}</span></div>
			<LogEntries rows={rows}/>
			{!rows.length&&!error&&!loading&&<Empty icon={<FileText/>} title={t('logs.emptyTitle')}/>}
		</div>
	</div>
}
