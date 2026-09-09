import { useTranslation } from 'react-i18next'
import { History, LoaderCircle } from 'lucide-react'
import { errorText } from '../../lib/utils'
import type { AuditPageSnapshot } from './auditHistoryPage'

type AuditPageFeedbackProps={
	page:Pick<AuditPageSnapshot<unknown>,'loading'|'error'|'nextCursor'|'failedOperation'>
	onRetry:()=>Promise<unknown>
	onLoadMore:()=>Promise<unknown>
	moreLabel:string
}

export function AuditPageFeedback({page,onRetry,onLoadMore,moreLabel}:AuditPageFeedbackProps){
	const {t}=useTranslation()
	if(page.loading==='refresh')return <div className="audit-loading" role="status"><LoaderCircle className="spin" size={16}/><span>{t('common.loading')}</span></div>
	if(page.error)return <div className="audit-page-feedback"><div className="inline-error">{errorText(page.error)}</div><button type="button" className="audit-load-more panel" onClick={()=>void (page.failedOperation==='more'?onLoadMore():onRetry())}>{t('common.retry')}</button></div>
	if(page.nextCursor)return <button type="button" className="audit-load-more panel" disabled={page.loading==='more'} onClick={()=>void onLoadMore()}>{page.loading==='more'?<LoaderCircle className="spin" size={14}/>:<History size={14}/>} {moreLabel}</button>
	return null
}
