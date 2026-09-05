import { memo } from 'react'
import { useTranslation } from 'react-i18next'
import { localeFor } from '../../lib/i18n'
import type { ServerLogEntry } from '../../types'
import type { LogRow } from './useLogData'

function logFieldValue(value:unknown){
	if(value===null||value===undefined)return '—'
	return typeof value==='object'?JSON.stringify(value):String(value)
}

const LogEntry=memo(function LogEntry({entry,locale,general}:{entry:ServerLogEntry;locale:string;general:string}){
	return <div className={`log-row log-entry ${entry.level}`}>
		<time>{new Date(entry.time).toLocaleTimeString(locale,{hour12:false,fractionalSecondDigits:3})}</time>
		<span><i className={`log-level ${entry.level}`}>{entry.level}</i></span>
		<code className="log-component">{entry.component||general}</code>
		<div className="log-event"><b>{entry.message}</b>{entry.fields&&Object.keys(entry.fields).length>0&&<div className="log-fields">
			{Object.entries(entry.fields).map(([key,value])=>{const text=logFieldValue(value);return <span key={key}><em>{key}</em><code title={text}>{text}</code></span>})}
		</div>}</div>
	</div>
})

export const LogEntries=memo(function LogEntries({rows}:{rows:LogRow[]}){
	const {t,i18n}=useTranslation()
	const locale=localeFor(i18n.language),general=t('logs.general')
	return <>{rows.map(row=><LogEntry key={row.id} entry={row.entry} locale={locale} general={general}/>)}</>
})
