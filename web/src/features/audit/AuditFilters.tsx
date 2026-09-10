import { FilterX } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { AppSelect } from '../../components/Controls'
import type { Host } from '../../types'
import type { AuditHistoryFilters } from '../../types/audit'

type FilterField='hostID'|'startedAfter'|'startedBefore'

function localDateTimeValue(value:string){
	if(!value)return''
	const date=new Date(value)
	if(Number.isNaN(date.getTime()))return''
	const part=(number:number)=>String(number).padStart(2,'0')
	return`${date.getFullYear()}-${part(date.getMonth()+1)}-${part(date.getDate())}T${part(date.getHours())}:${part(date.getMinutes())}`
}

function utcDateTimeValue(value:string){
	if(!value)return''
	const date=new Date(value)
	return Number.isNaN(date.getTime())?'':date.toISOString()
}

export function AuditFilters({filters,hosts,invalidRange,onChange}:{filters:AuditHistoryFilters;hosts:Host[];invalidRange:boolean;onChange:(filters:AuditHistoryFilters)=>void}){
	const {t}=useTranslation()
	const startedAfter=localDateTimeValue(filters.startedAfter)
	const startedBefore=localDateTimeValue(filters.startedBefore)
	const update=(field:FilterField,value:string)=>onChange({...filters,[field]:value})
	const hasFilters=!!(filters.hostID||filters.startedAfter||filters.startedBefore)
	return <div className="audit-filter-bar">
		<label className="audit-host-filter"><span>{t('common.host')}</span><AppSelect value={filters.hostID} ariaLabel={t('common.host')} onChange={value=>update('hostID',value)} options={[{value:'',label:t('audit.allHosts')},...hosts.map(host=>({value:host.id,label:host.name}))]}/></label>
		<label><span>{t('audit.startedAfter')}</span><input type="datetime-local" step="60" value={startedAfter} max={startedBefore||undefined} aria-invalid={invalidRange} onChange={event=>update('startedAfter',utcDateTimeValue(event.target.value))}/></label>
		<label><span>{t('audit.startedBefore')}</span><input type="datetime-local" step="60" value={startedBefore} min={startedAfter||undefined} aria-invalid={invalidRange} onChange={event=>update('startedBefore',utcDateTimeValue(event.target.value))}/></label>
		{hasFilters&&<button type="button" className="audit-filter-clear" onClick={()=>onChange({...filters,hostID:'',startedAfter:'',startedBefore:''})}><FilterX size={13}/>{t('audit.clearFilters')}</button>}
		{invalidRange&&<span className="audit-filter-error" role="alert">{t('audit.invalidTimeRange')}</span>}
	</div>
}
