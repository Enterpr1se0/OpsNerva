import { useEffect, useState, useSyncExternalStore } from 'react'
import { useTranslation } from 'react-i18next'
import { LoaderCircle } from 'lucide-react'
import { api } from '../../api/api'
import { errorText } from '../../lib/utils'
import type { Host, Run } from '../../types'
import { requestFromRun } from '../tools/request'
import { AuditRunDetail } from './AuditRunDetail'
import { AuditRunDetailResource } from './auditRunDetailResource'

export function AuditRunDetailLoader({run,hosts,active}:{run:Run;hosts:Host[];active:boolean}){
	const {t}=useTranslation()
	const [resource]=useState(()=>new AuditRunDetailResource(api.runDetail))
	const detail=useSyncExternalStore(resource.subscribe,resource.getSnapshot,resource.getSnapshot)
	useEffect(()=>{
		resource.setInput(run,active)
		return()=>resource.cancel()
	},[resource,run,active])
	if(detail.loading&&active)return <div className="audit-loading" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>
	if(detail.error)return <div className="audit-page-feedback"><div className="inline-error">{errorText(detail.error)}</div><button type="button" className="audit-load-more panel" onClick={()=>void resource.retry()}>{t('common.retry')}</button></div>
	return detail.run&&<AuditRunDetail run={detail.run} req={requestFromRun(detail.run)||{request:detail.run.request_json}} hosts={hosts}/>
}
