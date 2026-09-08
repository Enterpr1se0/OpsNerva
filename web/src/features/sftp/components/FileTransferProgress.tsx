import { useCallback, useSyncExternalStore } from 'react'
import { useTranslation } from 'react-i18next'
import { formatFileSize } from '../../../lib/utils'
import { useDocumentVisible } from '../../../lib/hooks'
import { useFileTransferManager } from '../useFileTransfer'

export function FileTransferProgress({transferKey,active}:{transferKey:string;active:boolean}){
	const {t}=useTranslation()
	const manager=useFileTransferManager()
	const visible=useDocumentVisible()
	const enabled=active&&visible
	const subscribe=useCallback((listener:()=>void)=>enabled?manager.store.subscribeProgress(transferKey,listener):()=>{},[enabled,manager,transferKey])
	const snapshot=useCallback(()=>enabled?manager.store.progress(transferKey):null,[enabled,manager,transferKey])
	const transfer=useSyncExternalStore(subscribe,snapshot,snapshot)
	if(!transfer)return null
	const onCancel=()=>manager.cancel(transferKey)
	const percent=transfer.total>0?Math.min(100,Math.round(transfer.loaded/transfer.total*100)):0
	return <div className={`file-transfer-progress ${transfer.total>0?'':'indeterminate'}`} role="progressbar" aria-valuemin={0} aria-valuemax={transfer.total||undefined} aria-valuenow={transfer.total>0?transfer.loaded:undefined}>
		<div><span title={transfer.name}>{transfer.index&&transfer.count?`${transfer.index}/${transfer.count} · `:''}{transfer.name}</span><b>{formatFileSize(transfer.loaded)}{transfer.total>0?` / ${formatFileSize(transfer.total)}`:''}</b><button type="button" onClick={onCancel}>{t('common.cancel')}</button></div>
		<i><em style={transfer.total>0?{width:`${percent}%`}:undefined}/></i>
	</div>
}
